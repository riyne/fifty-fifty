// Package mockpay is a stand-in for a card or bank payment provider, used to build and
// measure settlement. Settlement in this project is SIMULATED: nothing here moves
// money, and every result that uses it must say "mock payment provider".
//
// It is modelled on the parts of real providers that make payments hard:
//
//   - Idempotency keys. POST /v1/payments needs an Idempotency-Key. The first call
//     with a key creates the payment; later calls with the same key and the same
//     request return that payment (header Idempotent-Replayed: true) and charge
//     nothing; the same key with a different request is 422.
//   - A response can be lost AFTER the payment happened (timeout_after, 5xx_after).
//     That is the case that makes a client unsure whether it was charged, and the
//     reason it must be able to ask: GET /v1/payments?idempotency_key=K.
//   - Webhooks, HMAC-signed (internal/paysig), that may be delayed, duplicated,
//     dropped or delivered out of order, sent when a payment is created.
//
// Unlike the mock LLM this server has state (the payments), so it also exposes the
// ground truth a benchmark needs: GET /stats and GET /debug/payments.
//
// Reproducibility: every random decision is a function of (seed, idempotency key,
// call number for that key), NOT of arrival order. The same settlement therefore
// gets the same faults on every run, even under concurrency, and "the first call times
// out, the retry succeeds" is expressible. (The mock LLM draws from one shared
// sequence, so its exact fault sequence depends on arrival order; this one does not.)
//
// Not simulated: authentication (none; bind to loopback), real payment rails, refunds,
// webhook redelivery after a non-2xx answer (an event is sent once, plus a duplicate
// if configured), and anything with currency conversion.
package mockpay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/riyne/fifty-fifty/internal/paysig"
)

// MaxRequestBytes bounds a request body; a payment request is a few hundred bytes.
const MaxRequestBytes = 1 << 14

// Payment statuses. A payment is created already final (the mock decides at once);
// the "processing" status only appears in the non-terminal webhook event.
const (
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusProcessing = "processing"
)

// FailureCode is the failure_code of a declined payment.
const FailureCode = "card_declined"

// Config is the behaviour of a Server. The zero value is a fast, fault-free provider
// with no webhooks.
type Config struct {
	// LatencyMedian and LatencySigma: log-normal response time of every processed
	// request (as for the mock LLM). Errors that happen before the provider does
	// anything answer immediately, which is the optimistic case results must not hide.
	LatencyMedian time.Duration
	LatencySigma  float64

	// Outcomes of one create call, drawn per call and mutually exclusive, so they must
	// sum to at most 1; the rest succeed.
	RateDecline       float64 // a NEW payment is created as failed (definite failure)
	RateTimeoutBefore float64 // connection held then aborted; the payment was NOT created
	Rate5xxBefore     float64 // 500; the payment was NOT created
	RateTimeoutAfter  float64 // the payment WAS created; connection held then aborted
	Rate5xxAfter      float64 // the payment WAS created; then 500
	// RateReadError is the chance a GET by key answers 503, per read.
	RateReadError float64

	// TimeoutHold is how long a timeout fault holds the connection before aborting
	// it, if the client has not given up first. Zero means a minute.
	TimeoutHold time.Duration

	Seed uint64

	// Webhooks are sent to WebhookURL, signed with WebhookSecret, when a payment is
	// created: first "payment.processing", then "payment.succeeded" or "payment.failed".
	// Empty WebhookURL means none.
	WebhookURL    string
	WebhookSecret string
	// WebhookDelayMax: each delivery is delayed by a uniform draw in [0, max).
	WebhookDelayMax time.Duration
	// Per event: the chance it is never sent, and (if sent) the chance it is sent twice.
	WebhookRateDrop      float64
	WebhookRateDuplicate float64
	// Per payment: the chance the terminal event overtakes the processing event
	// (needs WebhookDelayMax > 0).
	WebhookRateOutOfOrder float64

	// HTTPClient sends the webhooks; nil means a client with a 5 s timeout.
	HTTPClient *http.Client
}

// Validate reports a configuration that cannot mean what the caller intends.
func (c Config) Validate() error {
	rate := func(name string, r float64) error {
		if math.IsNaN(r) || r < 0 || r > 1 {
			return fmt.Errorf("mockpay: %s %v must be in [0, 1]", name, r)
		}
		return nil
	}
	for name, r := range map[string]float64{
		"decline rate": c.RateDecline, "timeout-before rate": c.RateTimeoutBefore, "5xx-before rate": c.Rate5xxBefore,
		"timeout-after rate": c.RateTimeoutAfter, "5xx-after rate": c.Rate5xxAfter, "read-error rate": c.RateReadError,
		"webhook drop rate": c.WebhookRateDrop, "webhook duplicate rate": c.WebhookRateDuplicate,
		"webhook out-of-order rate": c.WebhookRateOutOfOrder,
	} {
		if err := rate(name, r); err != nil {
			return err
		}
	}
	if sum := c.RateDecline + c.RateTimeoutBefore + c.Rate5xxBefore + c.RateTimeoutAfter + c.Rate5xxAfter; sum > 1 {
		return fmt.Errorf("mockpay: create outcome rates sum to %v; they are exclusive outcomes and must sum to at most 1", sum)
	}
	if c.LatencyMedian < 0 || c.TimeoutHold < 0 || c.WebhookDelayMax < 0 {
		return errors.New("mockpay: durations must not be negative")
	}
	if math.IsNaN(c.LatencySigma) || c.LatencySigma < 0 || c.LatencySigma > 2 {
		return fmt.Errorf("mockpay: latency sigma %v must be in [0, 2]", c.LatencySigma)
	}
	if c.WebhookURL == "" {
		if c.WebhookRateDrop > 0 || c.WebhookRateDuplicate > 0 || c.WebhookRateOutOfOrder > 0 || c.WebhookDelayMax > 0 {
			return errors.New("mockpay: webhook faults are set but there is no webhook URL")
		}
	} else if c.WebhookSecret == "" {
		return errors.New("mockpay: webhooks need a secret")
	}
	if c.WebhookRateOutOfOrder > 0 && c.WebhookDelayMax == 0 {
		return errors.New("mockpay: out-of-order webhooks need a delay maximum above zero")
	}
	return nil
}

// Payment is what the API returns for a payment.
type Payment struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Reference      string    `json:"reference"`
	From           string    `json:"from"`
	To             string    `json:"to"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	FailureCode    *string   `json:"failure_code"`
	CreatedAt      time.Time `json:"created_at"`
}

// request is the body of POST /v1/payments. It is also what an idempotency key is
// compared against: the same key must carry the same request.
type request struct {
	Reference   string `json:"reference"`
	From        string `json:"from"`
	To          string `json:"to"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (r request) validate() error {
	text := func(name, v string) error {
		if n := len(v); n < 1 || n > 200 {
			return fmt.Errorf("%s must be 1-200 bytes", name)
		}
		return nil
	}
	for name, v := range map[string]string{"reference": r.Reference, "from": r.From, "to": r.To} {
		if err := text(name, v); err != nil {
			return err
		}
	}
	switch {
	case r.AmountMinor < 1 || r.AmountMinor > 1_000_000_000:
		return errors.New("amount_minor must be between 1 and 1000000000")
	case r.Currency != "CAD" && r.Currency != "USD":
		return errors.New("currency must be CAD or USD")
	}
	return nil
}

// Stats counts what the server did. Everything is cumulative since start.
type Stats struct {
	// CreateCalls: valid POST /v1/payments requests. Every one is in exactly one of the
	// outcome counters below, except calls whose client left during a simulated delay.
	CreateCalls int64 `json:"create_calls"`
	// Payments is the number of distinct payments created (at most one per key);
	// Charges of them succeeded and Declined failed. Charges is the number to compare
	// with the ledger: it is what the provider thinks it took from the sender.
	Payments int64 `json:"payments"`
	Charges  int64 `json:"charges"`
	Declined int64 `json:"declined"`
	// Replays: calls that found their key's payment already there.
	Replays       int64 `json:"replays"`
	KeyReuse      int64 `json:"key_reuse"` // same key, different request: 422
	BadRequests   int64 `json:"bad_requests"`
	TimeoutBefore int64 `json:"timeout_before"`
	Errors5xxBef  int64 `json:"errors_5xx_before"`
	TimeoutAfter  int64 `json:"timeout_after"`
	Errors5xxAft  int64 `json:"errors_5xx_after"`
	Reads         int64 `json:"reads"`
	ReadErrors    int64 `json:"read_errors"`

	WebhookEvents       int64 `json:"webhook_events"`       // events that were scheduled (not dropped)
	WebhookDropped      int64 `json:"webhook_dropped"`      // events never sent
	WebhookDuplicates   int64 `json:"webhook_duplicates"`   // extra deliveries scheduled
	WebhookOutOfOrder   int64 `json:"webhook_out_of_order"` // payments whose terminal event was scheduled to overtake
	WebhookDeliveries   int64 `json:"webhook_deliveries"`   // POSTs attempted
	WebhookNon2xxOrFail int64 `json:"webhook_failures"`     // POSTs that failed or got a non-2xx answer
}

// Server is the mock. Create it with New, serve Handler, and Close it when done.
type Server struct {
	cfg    Config
	client *http.Client

	mu      sync.Mutex
	byKey   map[string]*record // by idempotency key
	order   []string           // keys in creation order
	creates map[string]int     // valid create calls per key
	reads   map[string]int     // reads per key

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // webhook deliveries in flight

	createCalls, payments, charges, declined, replays, keyReuse, badRequests   atomic.Int64
	timeoutBefore, err5xxBefore, timeoutAfter, err5xxAfter, readCount, readErr atomic.Int64
	whEvents, whDropped, whDuplicates, whOutOfOrder, whDeliveries, whFailures  atomic.Int64
}

type record struct {
	Payment
	req   request
	calls int // processed create calls (the one that created it plus replays)
}

// New returns a Server, or an error if cfg is invalid.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.TimeoutHold == 0 {
		cfg.TimeoutHold = time.Minute
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg: cfg, client: client, ctx: ctx, cancel: cancel,
		byKey: map[string]*record{}, creates: map[string]int{}, reads: map[string]int{},
	}, nil
}

// Close stops pending webhook deliveries and waits for the ones in flight.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// Handler serves the API, GET /stats and GET /debug/payments.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payments", s.create)
	mux.HandleFunc("GET /v1/payments", s.read)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /debug/payments", s.debugPayments)
	return mux
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		CreateCalls: s.createCalls.Load(), Payments: s.payments.Load(), Charges: s.charges.Load(), Declined: s.declined.Load(),
		Replays: s.replays.Load(), KeyReuse: s.keyReuse.Load(), BadRequests: s.badRequests.Load(),
		TimeoutBefore: s.timeoutBefore.Load(), Errors5xxBef: s.err5xxBefore.Load(),
		TimeoutAfter: s.timeoutAfter.Load(), Errors5xxAft: s.err5xxAfter.Load(),
		Reads: s.readCount.Load(), ReadErrors: s.readErr.Load(),
		WebhookEvents: s.whEvents.Load(), WebhookDropped: s.whDropped.Load(), WebhookDuplicates: s.whDuplicates.Load(),
		WebhookOutOfOrder: s.whOutOfOrder.Load(), WebhookDeliveries: s.whDeliveries.Load(), WebhookNon2xxOrFail: s.whFailures.Load(),
	}
}

// ---- randomness ----

// rngFor returns the generator for one (key, stream) pair. Everything random about a
// call is drawn from it, so the draws depend on the seed, the key and the call number
// and on nothing else, in particular not on which request arrived first.
func (s *Server) rngFor(key string, stream uint64) *rand.Rand {
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], s.cfg.Seed)
	h.Write(seed[:])
	h.Write([]byte(key))
	return rand.New(rand.NewPCG(h.Sum64(), stream))
}

// Streams: calls to create are streams 1, 2, 3, ... for the key; reads and the
// webhooks of a payment have their own, so they cannot disturb the create draws.
const (
	streamWebhooks = 0
	streamReads    = 1 << 32
)

type outcome int

const (
	outOK outcome = iota
	outDecline
	outTimeoutBefore
	out5xxBefore
	outTimeoutAfter
	out5xxAfter
)

func (c Config) pick(u float64) outcome {
	edges := []struct {
		rate float64
		o    outcome
	}{
		{c.RateTimeoutBefore, outTimeoutBefore}, {c.Rate5xxBefore, out5xxBefore},
		{c.RateTimeoutAfter, outTimeoutAfter}, {c.Rate5xxAfter, out5xxAfter}, {c.RateDecline, outDecline},
	}
	acc := 0.0
	for _, e := range edges {
		acc += e.rate
		if u < acc {
			return e.o
		}
	}
	return outOK
}

// latency turns a standard normal draw into a log-normal delay: median * exp(sigma * z).
func (c Config) latency(z float64) time.Duration {
	if c.LatencyMedian == 0 {
		return 0
	}
	return time.Duration(float64(c.LatencyMedian) * math.Exp(c.LatencySigma*z))
}

// ---- create ----

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if n := len(key); n < 1 || n > 255 {
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key is required (1-255 bytes)")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		s.badRequests.Add(1)
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body is too large or unreadable")
		return
	}
	var req request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is not the expected JSON object")
		return
	}
	if err := req.validate(); err != nil {
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	s.mu.Lock()
	s.creates[key]++
	call := uint64(s.creates[key])
	s.mu.Unlock()
	s.createCalls.Add(1)

	rng := s.rngFor(key, call)
	u, z := rng.Float64(), rng.NormFloat64()
	o, latency := s.cfg.pick(u), s.cfg.latency(z)

	// Faults where the provider has done nothing.
	switch o {
	case outTimeoutBefore:
		s.timeoutBefore.Add(1)
		s.hold(r.Context())
	case out5xxBefore:
		s.err5xxBefore.Add(1)
		writeError(w, http.StatusInternalServerError, "api_error", "mock provider error")
		return
	}

	// The provider processes the request: creates the payment, or finds it.
	p, created, reused := s.process(key, req, o == outDecline)
	if reused {
		s.keyReuse.Add(1)
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
		return
	}
	if created {
		s.schedule(p)
	} else {
		s.replays.Add(1)
	}

	// Faults where it has: the payment exists, the client does not know.
	switch o {
	case outTimeoutAfter:
		s.timeoutAfter.Add(1)
		s.hold(r.Context())
	case out5xxAfter:
		if !sleep(r.Context(), latency) {
			return
		}
		s.err5xxAfter.Add(1)
		writeError(w, http.StatusInternalServerError, "api_error", "mock provider error")
		return
	}

	if !sleep(r.Context(), latency) {
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, status, p)
}

// process creates the payment for key if there is none, and reports whether it did.
// The decision (success or decline) is made here, once, under the lock: later calls
// get the stored payment whatever they draw. reused is true when the key already
// belongs to a different request, decided under the same lock so two concurrent calls
// with one key and different requests cannot both pass.
func (s *Server) process(key string, req request, decline bool) (p Payment, created, reused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byKey[key]; ok {
		if rec.req != req {
			return Payment{}, false, true
		}
		rec.calls++
		return rec.Payment, false, false
	}
	h := fnv.New64a()
	h.Write([]byte(key))
	p = Payment{
		ID: fmt.Sprintf("pay_%016x", h.Sum64()), IdempotencyKey: key, Reference: req.Reference, From: req.From, To: req.To,
		AmountMinor: req.AmountMinor, Currency: req.Currency, Status: StatusSucceeded, CreatedAt: time.Now().UTC(),
	}
	if decline {
		code := FailureCode
		p.Status, p.FailureCode = StatusFailed, &code
	}
	s.byKey[key] = &record{Payment: p, req: req, calls: 1}
	s.order = append(s.order, key)
	s.payments.Add(1)
	if decline {
		s.declined.Add(1)
	} else {
		s.charges.Add(1)
	}
	return p, true, false
}

// ---- read ----

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("idempotency_key")
	if n := len(key); n < 1 || n > 255 {
		s.badRequests.Add(1)
		writeError(w, http.StatusBadRequest, "invalid_request", "idempotency_key is required (1-255 bytes)")
		return
	}
	s.mu.Lock()
	s.reads[key]++
	n := uint64(s.reads[key])
	rec := s.byKey[key]
	s.mu.Unlock()
	s.readCount.Add(1)

	rng := s.rngFor(key, streamReads+n)
	u, z := rng.Float64(), rng.NormFloat64()
	if u < s.cfg.RateReadError {
		s.readErr.Add(1)
		writeError(w, http.StatusServiceUnavailable, "api_error", "mock provider unavailable")
		return
	}
	if !sleep(r.Context(), s.cfg.latency(z)) {
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, "not_found", "no payment for this idempotency key")
		return
	}
	writeJSON(w, http.StatusOK, rec.Payment)
}

// ---- webhooks ----

type event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		Payment Payment `json:"payment"`
	} `json:"data"`
}

// schedule decides, from the payment's own random stream, what is sent and when, and
// starts one sender for the payment. All the randomness is drawn here, in a fixed
// order, so the plan for a payment is the same on every run.
//
// Events are sent in the order planned: the processing event, then the terminal one,
// each possibly delayed, so the ONLY thing that reorders them is the out-of-order
// fault. A payment has one sender, which sends its deliveries one after another (a
// slow receiver delays the payment's later deliveries); different payments are
// independent and concurrent.
func (s *Server) schedule(p Payment) {
	if s.cfg.WebhookURL == "" {
		return
	}
	rng := s.rngFor(p.IdempotencyKey, streamWebhooks)
	max := s.cfg.WebhookDelayMax
	delay := func() time.Duration { return time.Duration(rng.Float64() * float64(max)) }

	types := []string{"payment.processing", "payment." + p.Status}
	statuses := []string{StatusProcessing, p.Status}

	// Draws, in order: out-of-order, then per event: drop, duplicate, delay, extra delay of the copy.
	outOfOrder := rng.Float64() < s.cfg.WebhookRateOutOfOrder
	type plan struct {
		drop, dup   bool
		gap, dupGap time.Duration
	}
	plans := make([]plan, len(types))
	for i := range plans {
		plans[i] = plan{drop: rng.Float64() < s.cfg.WebhookRateDrop, dup: rng.Float64() < s.cfg.WebhookRateDuplicate, gap: delay(), dupGap: delay()}
	}
	at := []time.Duration{plans[0].gap, plans[0].gap + plans[1].gap} // later events are never earlier
	if outOfOrder {
		at[0] = at[1] + max // the processing event arrives after the terminal one
		s.whOutOfOrder.Add(1)
	}

	type delivery struct {
		at   time.Duration
		body []byte
	}
	var plan_ []delivery
	for i := range types {
		pl := plans[i]
		if pl.drop {
			s.whDropped.Add(1)
			continue
		}
		s.whEvents.Add(1)
		e := event{ID: fmt.Sprintf("evt_%s_%d", p.ID[len("pay_"):], i), Type: types[i], CreatedAt: time.Now().UTC()}
		snapshot := p
		snapshot.Status = statuses[i]
		if snapshot.Status == StatusProcessing {
			snapshot.FailureCode = nil
		}
		e.Data.Payment = snapshot
		body, _ := json.Marshal(e)
		plan_ = append(plan_, delivery{at[i], body})
		if pl.dup {
			s.whDuplicates.Add(1)
			plan_ = append(plan_, delivery{at[i] + pl.dupGap, body}) // same event id, as a real provider's retry would be
		}
	}
	sort.SliceStable(plan_, func(i, j int) bool { return plan_[i].at < plan_[j].at })

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		start := time.Now()
		for _, d := range plan_ {
			if !sleep(s.ctx, time.Until(start.Add(d.at))) {
				return
			}
			s.send(d.body)
		}
	}()
}

// send POSTs one signed webhook and counts the result.
func (s *Server) send(body []byte) {
	sig, err := paysig.Sign([]byte(s.cfg.WebhookSecret), time.Now(), body)
	if err != nil {
		s.whFailures.Add(1)
		return
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		s.whFailures.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(paysig.Header, sig)
	s.whDeliveries.Add(1)
	resp, err := s.client.Do(req)
	if err != nil {
		s.whFailures.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.whFailures.Add(1)
	}
}

// ---- introspection ----

func (s *Server) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Stats())
}

// Ledger is one line of GET /debug/payments: what the provider holds for a key.
type Ledger struct {
	Payment
	// CreateCalls: every valid create call that named this key, including those that
	// timed out or failed before reaching the payment.
	CreateCalls int `json:"create_calls"`
	// Processed: the calls that reached the payment (it was created or found).
	Processed int `json:"processed"`
}

// Payments returns every payment in creation order. A benchmark compares this with
// the app's ledger: charged = Status succeeded; two succeeded payments with one
// Reference would be a double charge.
func (s *Server) Payments() []Ledger {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Ledger, 0, len(s.order))
	for _, k := range s.order {
		rec := s.byKey[k]
		out = append(out, Ledger{Payment: rec.Payment, CreateCalls: s.creates[k], Processed: rec.calls})
	}
	return out
}

func (s *Server) debugPayments(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"payments": s.Payments()})
}

// ---- helpers ----

// hold keeps the connection open without answering, then aborts it: after TimeoutHold,
// or as soon as the client gives up or the server shuts down. Returning normally
// would make net/http send an empty 200, which is a response, not a timeout.
func (s *Server) hold(ctx context.Context) {
	sleep(ctx, s.cfg.TimeoutHold)
	panic(http.ErrAbortHandler)
}

// sleep waits d or until ctx is done, and reports whether the full time passed.
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}
