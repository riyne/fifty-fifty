// Package mockllm is a stand-in for the Anthropic Messages API, used by the queue
// benchmarks and by the receipt-pipeline tests. It speaks the subset of the wire
// format a client needs (POST /v1/messages, the response envelope, the error bodies,
// the status codes and the retry-after header), so the client code that talks to it
// is the code that talks to the real API.
//
// What it simulates, and nothing else:
//
//   - Latency: log-normal with a configurable median and sigma (sigma 0 is a fixed
//     delay). Real LLM latencies are right-skewed; a fixed delay would make the
//     queue's p99 look unrealistically clean. Applied to successful and malformed
//     responses; errors answer immediately (a rate limit does, and a fast 5xx is the
//     optimistic case, which results must not hide).
//   - Faults: each request independently draws one outcome from the configured
//     rates: timeout, 429, 5xx, malformed 200, otherwise success. The outcomes are
//     mutually exclusive, so the observed frequencies converge on the configured
//     rates. A retry draws again, as it would against a real API.
//   - By default it never looks at the prompt: the reply is the same canned text, and a
//     benchmark of the queue does not need a model. With Config.Fixtures it looks for
//     one thing only, a "REF n" token in the text of the messages, and answers with the
//     fixture stored under n (a receipt's ground-truth answer, or an adversarial one a
//     test wants the client to face). It does not read, understand or follow anything
//     else in the prompt, so it says nothing about how a real model treats a prompt.
//
// Reproducibility: the random draws come from a seeded generator, so the same seed
// gives the same sequence of draws. Which request receives which draw still depends
// on arrival order under concurrency, so the rates are reproducible, the exact fault
// sequence is not. Benchmarks record the observed counts (GET /stats), not just the
// configured rates.
package mockllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxRequestBytes bounds a request body. Real receipts reach the API as text of a few
// kilobytes; the mock refuses anything much larger rather than reading it.
const MaxRequestBytes = 1 << 20

// Config is the behaviour of a Server. The zero value is a fast, fault-free mock.
type Config struct {
	// LatencyMedian is the median response time of successful responses. Zero means
	// respond immediately.
	LatencyMedian time.Duration
	// LatencySigma is the log-normal shape parameter: the standard deviation of the
	// log of the latency. 0 is a fixed delay; 0.4 gives p99 about 2.5x the median.
	LatencySigma float64

	// Probabilities that a request ends in each fault. They are mutually exclusive
	// outcomes, so they must sum to at most 1; the rest succeed.
	RateTimeout   float64 // the connection is held, then aborted, with no response
	Rate429       float64 // 429 with a retry-after header
	Rate5xx       float64 // 500 or 529 (Anthropic's "overloaded"), half each
	RateMalformed float64 // 200 whose body is cut off and is not valid JSON

	// TimeoutHold is how long a timeout fault holds the connection before aborting
	// it, if the client has not given up first. Zero means a minute.
	TimeoutHold time.Duration
	// RetryAfter is the retry-after header on a 429, rounded up to whole seconds
	// (the header's unit). Zero means one second.
	RetryAfter time.Duration

	// Seed seeds the random draws.
	Seed uint64
	// Reply is the assistant text in a successful response. Empty means a short
	// canned sentence.
	Reply string
	// Fixtures maps a reference number (decimal digits, as printed after "REF " on a
	// receipt) to the assistant text returned when the messages contain "REF <number>".
	// The first token in the messages decides; a request with no token, or a number
	// with no fixture, gets Reply. The text is returned as given, valid or not: a
	// fixture is how a test makes the mock say something wrong.
	Fixtures map[string]string
}

// Validate reports a configuration that cannot mean what the caller intends.
func (c Config) Validate() error {
	for name, r := range map[string]float64{
		"timeout": c.RateTimeout, "429": c.Rate429, "5xx": c.Rate5xx, "malformed": c.RateMalformed,
	} {
		if math.IsNaN(r) || r < 0 || r > 1 {
			return fmt.Errorf("mockllm: %s rate %v must be in [0, 1]", name, r)
		}
	}
	if sum := c.RateTimeout + c.Rate429 + c.Rate5xx + c.RateMalformed; sum > 1 {
		return fmt.Errorf("mockllm: fault rates sum to %v; they are exclusive outcomes and must sum to at most 1", sum)
	}
	if c.LatencyMedian < 0 || c.TimeoutHold < 0 || c.RetryAfter < 0 {
		return errors.New("mockllm: durations must not be negative")
	}
	for k := range c.Fixtures {
		if !validRef(k) {
			return fmt.Errorf("mockllm: fixture key %q must be 1 to %d decimal digits", k, maxRefDigits)
		}
	}
	// 2 already means a p99 of ~100x the median; beyond it is almost surely a typo.
	if math.IsNaN(c.LatencySigma) || c.LatencySigma < 0 || c.LatencySigma > 2 {
		return fmt.Errorf("mockllm: latency sigma %v must be in [0, 2]", c.LatencySigma)
	}
	return nil
}

// Stats counts what the server has done since it started. Requests is the number of
// requests that reached /v1/messages; the rest classify them. A request whose client
// disconnected during the simulated delay is in Requests and in no other counter, and
// a timeout is counted when it starts.
type Stats struct {
	Requests    int64 `json:"requests"`
	OK          int64 `json:"ok"`
	RateLimited int64 `json:"rate_limited"`
	ServerError int64 `json:"server_error"`
	Malformed   int64 `json:"malformed"`
	Timeouts    int64 `json:"timeouts"`
	BadRequests int64 `json:"bad_requests"` // refused as invalid, not injected faults
	// FromFixture is how many of the OK responses carried a fixture's text instead of
	// Reply (a malformed response is not counted: it is cut off).
	FromFixture int64 `json:"from_fixture"`
}

// Server is the mock. Create it with New and serve Handler.
type Server struct {
	cfg Config

	mu  sync.Mutex // guards rng
	rng *rand.Rand

	requests, ok, rateLimited, serverError, malformed, timeouts, badRequests, fromFixture atomic.Int64
	nextID                                                                                atomic.Int64
}

// New returns a Server, or an error if cfg is invalid.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.TimeoutHold == 0 {
		cfg.TimeoutHold = time.Minute
	}
	if cfg.RetryAfter == 0 {
		cfg.RetryAfter = time.Second
	}
	if cfg.Reply == "" {
		cfg.Reply = "mock reply"
	}
	return &Server{cfg: cfg, rng: rand.New(rand.NewPCG(cfg.Seed, 0x6d6f636b6c6c6d))}, nil
}

// Handler serves POST /v1/messages and GET /stats.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("GET /stats", s.stats)
	return mux
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Requests:    s.requests.Load(),
		OK:          s.ok.Load(),
		RateLimited: s.rateLimited.Load(),
		ServerError: s.serverError.Load(),
		Malformed:   s.malformed.Load(),
		Timeouts:    s.timeouts.Load(),
		BadRequests: s.badRequests.Load(),
		FromFixture: s.fromFixture.Load(),
	}
}

type outcome int

const (
	outOK outcome = iota
	outTimeout
	out429
	out5xx
	outMalformed
)

// draw makes every random decision for one request under one lock, so the sequence
// of draws is a function of the seed alone.
func (s *Server) draw() (o outcome, latency time.Duration, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.rng.Float64()
	z := s.rng.NormFloat64()
	status = http.StatusInternalServerError
	if s.rng.Float64() < 0.5 {
		status = 529
	}
	c := s.cfg
	switch {
	case u < c.RateTimeout:
		o = outTimeout
	case u < c.RateTimeout+c.Rate429:
		o = out429
	case u < c.RateTimeout+c.Rate429+c.Rate5xx:
		o = out5xx
	case u < c.RateTimeout+c.Rate429+c.Rate5xx+c.RateMalformed:
		o = outMalformed
	}
	return o, c.latency(z), status
}

// latency turns a standard normal draw into a log-normal delay with the configured
// median: median * exp(sigma * z).
func (c Config) latency(z float64) time.Duration {
	if c.LatencyMedian == 0 {
		return 0
	}
	return time.Duration(float64(c.LatencyMedian) * math.Exp(c.LatencySigma*z))
}

type messageRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

// message is one entry of messages. Content is a string or a list of blocks, as in the
// real API; only text is kept, because that is all the fixture lookup reads.
type message struct {
	Content json.RawMessage `json:"content"`
}

// maxRefDigits keeps a reference number far below int overflow and a token cheap to scan.
const maxRefDigits = 9

var refRE = regexp.MustCompile(`\bREF (\d{1,` + strconv.Itoa(maxRefDigits) + `})\b`)

func validRef(k string) bool {
	if k == "" || len(k) > maxRefDigits {
		return false
	}
	for _, c := range k {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// promptText joins the text of the messages: a string content as is, and the text
// blocks of a list. Anything else (images, tool blocks) is ignored.
func promptText(msgs []message) string {
	var sb strings.Builder
	for _, m := range msgs {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			sb.WriteString(s)
			sb.WriteByte('\n')
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
					sb.WriteByte('\n')
				}
			}
		}
	}
	return sb.String()
}

// reply chooses the assistant text for a request and says whether it is a fixture's.
func (s *Server) reply(msgs []message) (text string, fixture bool) {
	if len(s.cfg.Fixtures) == 0 {
		return s.cfg.Reply, false
	}
	if m := refRE.FindStringSubmatch(promptText(msgs)); m != nil {
		if f, ok := s.cfg.Fixtures[m[1]]; ok {
			return f, true
		}
	}
	return s.cfg.Reply, false
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		s.badRequests.Add(1)
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large or unreadable")
		return
	}
	// The real API rejects these, so a client bug shows up here and not only in
	// production. Nothing else about the request is looked at.
	var req messageRequest
	switch {
	case json.Unmarshal(body, &req) != nil:
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	case req.Model == "" || req.MaxTokens <= 0 || len(req.Messages) == 0:
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model, max_tokens > 0 and a non-empty messages are required")
		return
	}

	reply, fromFixture := s.reply(req.Messages)
	o, latency, status := s.draw()
	switch o {
	case outTimeout:
		s.timeouts.Add(1)
		s.hold(r.Context())
	case out429:
		s.rateLimited.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(s.cfg.RetryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "mock rate limit")
	case out5xx:
		s.serverError.Add(1)
		typ := "api_error"
		if status == 529 {
			typ = "overloaded_error"
		}
		writeError(w, status, typ, "mock server error")
	case outMalformed:
		if !sleep(r.Context(), latency) {
			return
		}
		s.malformed.Add(1)
		full := s.success(req.Model, len(body), reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(full[:len(full)/2]) // a truncated body: 200, and not JSON
	default:
		if !sleep(r.Context(), latency) {
			return
		}
		s.ok.Add(1)
		if fromFixture {
			s.fromFixture.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.success(req.Model, len(body), reply))
	}
}

// hold keeps the connection open without answering, then aborts it: after TimeoutHold,
// or as soon as the client gives up or the server shuts down (the context is
// canceled). Returning normally instead would make net/http send an empty 200, which
// is a response, not a timeout. The client sees a reset or EOF.
func (s *Server) hold(ctx context.Context) {
	sleep(ctx, s.cfg.TimeoutHold)
	panic(http.ErrAbortHandler)
}

// sleep waits d or until ctx is done, and reports whether the full time passed. A
// client that disconnected is not owed a response.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// success builds a Messages API response. The token counts are a rough bytes/4
// estimate: the mock has no tokenizer, and a client only needs the fields to exist.
func (s *Server) success(model string, requestBytes int, reply string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":            "msg_mock_" + strconv.FormatInt(s.nextID.Add(1), 10),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]string{{"type": "text", "text": reply}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  requestBytes / 4,
			"output_tokens": len(reply) / 4,
		},
	})
	return b
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Stats())
}
