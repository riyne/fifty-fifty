package mockllm_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/mockllm"
)

const validBody = `{"model":"claude-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

func newServer(t *testing.T, cfg mockllm.Config) *mockllm.Server {
	t.Helper()
	s, err := mockllm.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func post(h http.Handler, ctx context.Context, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// postAborted is post for a server that may abort the connection (a timeout fault
// whose hold has ended): the abort is a panic with http.ErrAbortHandler, which
// net/http recovers and turns into a dropped connection.
func postAborted(h http.Handler, body string) (w *httptest.ResponseRecorder, aborted bool) {
	defer func() {
		if r := recover(); r != nil {
			if r != http.ErrAbortHandler {
				panic(r)
			}
			aborted = true
		}
	}()
	return post(h, context.Background(), body), false
}

func TestSuccess_HasTheMessagesShape(t *testing.T) {
	s := newServer(t, mockllm.Config{Reply: "hello there"})
	w := post(s.Handler(), context.Background(), validBody)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", w.Code, w.Header().Get("Content-Type"))
	}
	var got struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct{ Type, Text string }
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, w.Body)
	}
	if got.Type != "message" || got.Role != "assistant" || got.Model != "claude-test" || got.StopReason != "end_turn" ||
		!strings.HasPrefix(got.ID, "msg_mock_") || len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "hello there" ||
		got.Usage.InputTokens == 0 {
		t.Fatalf("unexpected response: %+v", got)
	}
	// Two responses never share an id.
	var second struct{ ID string }
	_ = json.Unmarshal(post(s.Handler(), context.Background(), validBody).Body.Bytes(), &second)
	if second.ID == got.ID {
		t.Fatalf("duplicate id %s", got.ID)
	}
}

func TestInvalidRequestsAreRefusedAndNotCountedAsFaults(t *testing.T) {
	s := newServer(t, mockllm.Config{})
	cases := map[string]string{
		"not json":     `{"model":`,
		"no model":     `{"max_tokens":8,"messages":[{}]}`,
		"no max":       `{"model":"m","messages":[{}]}`,
		"zero max":     `{"model":"m","max_tokens":0,"messages":[{}]}`,
		"no messages":  `{"model":"m","max_tokens":8,"messages":[]}`,
		"wrong type":   `{"model":1,"max_tokens":8,"messages":[{}]}`,
		"empty object": `{}`,
	}
	for name, body := range cases {
		w := post(s.Handler(), context.Background(), body)
		var e struct {
			Type  string
			Error struct{ Type string }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 400 || e.Type != "error" || e.Error.Type != "invalid_request_error" {
			t.Errorf("%s: status %d, body %s", name, w.Code, w.Body)
		}
	}
	if w := post(s.Handler(), context.Background(), strings.Repeat("x", mockllm.MaxRequestBytes+1)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d", w.Code)
	}
	st := s.Stats()
	if st.BadRequests != int64(len(cases))+1 || st.OK != 0 || st.Requests != st.BadRequests {
		t.Errorf("stats %+v", st)
	}
}

func TestRoutes_OnlyPostMessagesAndGetStats(t *testing.T) {
	h := newServer(t, mockllm.Config{}).Handler()
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v1/messages", 405},
		{"POST", "/stats", 405},
		{"POST", "/v1/complete", 404},
		{"GET", "/", 404},
		{"GET", "/stats", 200},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	bad := map[string]mockllm.Config{
		"negative rate":     {Rate429: -0.1},
		"rate over 1":       {Rate5xx: 1.5},
		"NaN rate":          {RateTimeout: math.NaN()},
		"rates sum over 1":  {RateTimeout: 0.4, Rate429: 0.4, RateMalformed: 0.3},
		"negative median":   {LatencyMedian: -1},
		"negative hold":     {TimeoutHold: -1},
		"negative retry":    {RetryAfter: -1},
		"negative sigma":    {LatencySigma: -0.1},
		"sigma over 2":      {LatencySigma: 2.1},
		"NaN sigma":         {LatencySigma: math.NaN()},
		"all rates are one": {RateTimeout: 1, Rate429: 1},
	}
	for name, cfg := range bad {
		if _, err := mockllm.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The boundary is allowed: every request fails one way, and rates may sum to exactly 1.
	if _, err := mockllm.New(mockllm.Config{Rate429: 0.5, Rate5xx: 0.5, LatencySigma: 2}); err != nil {
		t.Errorf("rates summing to 1 refused: %v", err)
	}
}

// The configured rates are what a client sees, within what chance allows. The seed is
// fixed, so this is deterministic, but the bound is four standard deviations of a
// binomial so that changing the seed would not flip it.
func TestFaultRatesMatchTheConfiguration(t *testing.T) {
	// A 1ms hold makes a timeout fault abort almost at once instead of for a minute.
	cfg := mockllm.Config{RateTimeout: 0.05, Rate429: 0.10, Rate5xx: 0.08, RateMalformed: 0.07, Seed: 7, TimeoutHold: time.Millisecond}
	s := newServer(t, cfg)

	const n = 40000
	var counts struct{ s429, s500, s529, ok, bad, aborted int }
	for range n {
		w, aborted := postAborted(s.Handler(), validBody)
		switch {
		case aborted:
			counts.aborted++
		case w.Code == 429:
			counts.s429++
			if w.Header().Get("Retry-After") != "1" {
				t.Fatalf("429 without retry-after: %v", w.Header())
			}
		case w.Code == 500:
			counts.s500++
		case w.Code == 529:
			counts.s529++
		case w.Code == 200 && json.Valid(w.Body.Bytes()):
			counts.ok++
		case w.Code == 200:
			counts.bad++
		default:
			t.Fatalf("unexpected response %d %s", w.Code, w.Body)
		}
	}
	within := func(name string, got int, p float64) {
		t.Helper()
		tol := 4 * math.Sqrt(p*(1-p)/n)
		if f := float64(got) / n; math.Abs(f-p) > tol {
			t.Errorf("%s: observed %.4f, configured %.4f (tolerance %.4f)", name, f, p, tol)
		}
	}
	within("429", counts.s429, cfg.Rate429)
	within("5xx", counts.s500+counts.s529, cfg.Rate5xx)
	within("malformed", counts.bad, cfg.RateMalformed)
	within("timeout", counts.aborted, cfg.RateTimeout)
	within("ok", counts.ok, 1-0.05-0.10-0.08-0.07)
	// 500 and 529 are a fair coin.
	within("500 of all 5xx", counts.s500, cfg.Rate5xx/2)

	st := s.Stats()
	want := mockllm.Stats{
		Requests: n, OK: int64(counts.ok), RateLimited: int64(counts.s429),
		ServerError: int64(counts.s500 + counts.s529), Malformed: int64(counts.bad), Timeouts: int64(counts.aborted),
	}
	if st != want {
		t.Errorf("stats %+v, want %+v", st, want)
	}
}

func TestErrorBodiesNameTheRealErrorTypes(t *testing.T) {
	for _, c := range []struct {
		cfg    mockllm.Config
		status int
		typ    string
	}{
		{mockllm.Config{Rate429: 1}, 429, "rate_limit_error"},
		{mockllm.Config{Rate5xx: 1, Seed: 1}, 500, "api_error"},
		{mockllm.Config{Rate5xx: 1, Seed: 1}, 529, "overloaded_error"},
	} {
		s := newServer(t, c.cfg)
		found := false
		for range 200 { // 5xx picks 500 or 529 by coin flip; both must occur
			w := post(s.Handler(), context.Background(), validBody)
			if w.Code != c.status {
				continue
			}
			found = true
			var e struct {
				Type  string
				Error struct{ Type string }
			}
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Type != "error" || e.Error.Type != c.typ {
				t.Fatalf("status %d body %s, want error type %s", w.Code, w.Body, c.typ)
			}
			break
		}
		if !found {
			t.Errorf("never saw status %d", c.status)
		}
	}
}

func TestRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	for in, want := range map[time.Duration]string{0: "1", 1500 * time.Millisecond: "2", 3 * time.Second: "3"} {
		w := post(newServer(t, mockllm.Config{Rate429: 1, RetryAfter: in}).Handler(), context.Background(), validBody)
		if got := w.Header().Get("Retry-After"); got != want {
			t.Errorf("RetryAfter %v: header %q, want %q", in, got, want)
		}
	}
}

func TestSameSeedGivesTheSameSequence(t *testing.T) {
	seq := func(seed uint64) []int {
		s := newServer(t, mockllm.Config{Rate429: 0.3, Rate5xx: 0.3, RateMalformed: 0.1, Seed: seed})
		var out []int
		for range 300 {
			w := post(s.Handler(), context.Background(), validBody)
			code := w.Code
			if code == 200 && !json.Valid(w.Body.Bytes()) {
				code = -1
			}
			out = append(out, code)
		}
		return out
	}
	if !slices.Equal(seq(1), seq(1)) {
		t.Error("the same seed gave different sequences")
	}
	if slices.Equal(seq(1), seq(2)) {
		t.Error("different seeds gave the same sequence")
	}
}

// Latency is log-normal with the configured median. The distribution is checked on
// the draws a server would make: the median and the 99th percentile
// (median * exp(sigma * 2.326)).
func TestLatencyIsLogNormalWithTheConfiguredMedian(t *testing.T) {
	cfg := mockllm.Config{LatencyMedian: 100 * time.Millisecond, LatencySigma: 0.4}
	// A standard-normal sample through the server's own formula (export_test.go), so
	// the test does not sleep through 20,000 simulated responses.
	var samples []float64
	for _, z := range normals(20000, 3) {
		samples = append(samples, float64(mockllm.LatencyFor(cfg, z)))
	}
	slices.Sort(samples)
	median, p99 := samples[len(samples)/2], samples[len(samples)*99/100]
	if want := float64(cfg.LatencyMedian); math.Abs(median-want)/want > 0.03 {
		t.Errorf("median %v, want %v", time.Duration(median), cfg.LatencyMedian)
	}
	if want := float64(cfg.LatencyMedian) * math.Exp(0.4*2.326); math.Abs(p99-want)/want > 0.10 {
		t.Errorf("p99 %v, want about %v", time.Duration(p99), time.Duration(want))
	}

	fixed := mockllm.Config{LatencyMedian: 80 * time.Millisecond}
	for _, z := range []float64{-3, 0, 3} {
		if got := mockllm.LatencyFor(fixed, z); got != 80*time.Millisecond {
			t.Errorf("sigma 0 latency %v for z=%v, want exactly the median", got, z)
		}
	}
	if got := mockllm.LatencyFor(mockllm.Config{LatencySigma: 1}, 1); got != 0 {
		t.Errorf("zero median gave %v", got)
	}
}

func TestResponseWaitsForTheSimulatedLatency(t *testing.T) {
	srv := httptest.NewServer(newServer(t, mockllm.Config{LatencyMedian: 120 * time.Millisecond}).Handler())
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := time.Since(start); d < 120*time.Millisecond {
		t.Fatalf("answered in %v, before the simulated 120ms", d)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Errors answer at once; only responses that carry a result are slow.
func TestFaultsThatAreErrorsAreNotDelayed(t *testing.T) {
	srv := httptest.NewServer(newServer(t, mockllm.Config{LatencyMedian: 5 * time.Second, Rate429: 1}).Handler())
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := time.Since(start); d > 2*time.Second || resp.StatusCode != 429 {
		t.Fatalf("429 took %v (status %d)", d, resp.StatusCode)
	}
}

// A client that gives up during the delay frees the handler; the server does not
// finish a sleep for nobody.
func TestClientDisconnectEndsTheDelay(t *testing.T) {
	s := newServer(t, mockllm.Config{LatencyMedian: time.Minute})
	srv := httptest.NewServer(s.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(validBody))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the client to time out")
	}
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }() // blocks until every handler has returned
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler kept sleeping after its client left")
	}
	if st := s.Stats(); st.OK != 0 || st.Requests != 1 {
		t.Fatalf("stats %+v: an abandoned request must not count as a success", st)
	}
}

func TestTimeoutFault_HoldsThenAbortsTheConnection(t *testing.T) {
	s := newServer(t, mockllm.Config{RateTimeout: 1, TimeoutHold: 200 * time.Millisecond})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(validBody))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got a response (%d) from a timeout fault", resp.StatusCode)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("connection dropped after %v, before the 200ms hold", d)
	}
	if got := s.Stats().Timeouts; got != 1 {
		t.Fatalf("timeouts = %d", got)
	}
}

func TestTimeoutFault_ReleasedWhenTheClientGivesUp(t *testing.T) {
	srv := httptest.NewServer(newServer(t, mockllm.Config{RateTimeout: 1, TimeoutHold: time.Hour}).Handler())
	client := &http.Client{Timeout: 150 * time.Millisecond}
	_, err := client.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(validBody))
	var ne interface{ Timeout() bool }
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a client timeout", err)
	}
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the held connection outlived its client")
	}
}

func TestConcurrentRequestsAreSafeAndCounted(t *testing.T) {
	s := newServer(t, mockllm.Config{Rate429: 0.3, Rate5xx: 0.2, RateMalformed: 0.2, Seed: 5})
	var wg sync.WaitGroup
	const workers, each = 40, 100
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				post(s.Handler(), context.Background(), validBody)
			}
		}()
	}
	wg.Wait()
	st := s.Stats()
	if st.Requests != workers*each || st.OK+st.RateLimited+st.ServerError+st.Malformed+st.Timeouts != st.Requests {
		t.Fatalf("stats %+v do not add up to %d requests", st, workers*each)
	}
}

func TestStatsEndpointReportsTheCounters(t *testing.T) {
	s := newServer(t, mockllm.Config{})
	post(s.Handler(), context.Background(), validBody)
	post(s.Handler(), context.Background(), `{}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	var got mockllm.Stats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.OK != 1 || got.BadRequests != 1 {
		t.Fatalf("stats %+v", got)
	}
}

// normals returns n standard-normal draws from a fixed seed.
func normals(n int, seed uint64) []float64 {
	r := rand.New(rand.NewPCG(seed, seed))
	out := make([]float64, n)
	for i := range out {
		out[i] = r.NormFloat64()
	}
	return out
}
