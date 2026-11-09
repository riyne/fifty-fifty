package mockpay_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/mockpay"
	"github.com/riyne/fifty-fifty/internal/paysig"
)

// ---- helpers ----

func start(t *testing.T, cfg mockpay.Config) (*mockpay.Server, string) {
	t.Helper()
	if cfg.TimeoutHold == 0 {
		cfg.TimeoutHold = 20 * time.Millisecond // keep tests fast; the real default is a minute
	}
	s, err := mockpay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts.URL
}

// No keep-alive: Go retries a request carrying an Idempotency-Key when a reused
// connection is closed under it, and a server that aborts connections on purpose
// would make the client add calls the test did not plan.
var client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

type reply struct {
	status int // 0 when the connection was aborted
	header http.Header
	body   []byte
}

// same compares payments by value (FailureCode is a pointer).
func same(a, b mockpay.Payment) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (r reply) payment(t *testing.T) mockpay.Payment {
	t.Helper()
	var p mockpay.Payment
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("not a payment: %q (%v)", r.body, err)
	}
	return p
}

func do(method, url, key, body string) reply {
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

func reqBody(ref string, amount int64) string {
	return fmt.Sprintf(`{"reference":%q,"from":"alice","to":"bob","amount_minor":%d,"currency":"CAD"}`, ref, amount)
}

func create(base, key, ref string, amount int64) reply {
	return do("POST", base+"/v1/payments", key, reqBody(ref, amount))
}

func byKey(base, key string) reply {
	return do("GET", base+"/v1/payments?idempotency_key="+key, "", "")
}

// ---- the API ----

func TestCreateReadAndReplay(t *testing.T) {
	s, url := start(t, mockpay.Config{})
	first := create(url, "k1", "settlement-1", 700)
	if first.status != 201 {
		t.Fatalf("create = %d: %s", first.status, first.body)
	}
	p := first.payment(t)
	if p.Status != "succeeded" || p.FailureCode != nil || p.AmountMinor != 700 || p.Currency != "CAD" ||
		p.Reference != "settlement-1" || p.From != "alice" || p.To != "bob" || p.IdempotencyKey != "k1" || p.ID == "" {
		t.Errorf("payment = %+v", p)
	}

	got := byKey(url, "k1")
	if got.status != 200 || !same(got.payment(t), p) {
		t.Errorf("read = %d %s, want the created payment", got.status, got.body)
	}

	again := create(url, "k1", "settlement-1", 700)
	if again.status != 200 || again.header.Get("Idempotent-Replayed") != "true" || !same(again.payment(t), p) {
		t.Errorf("replay = %d replayed=%q: %s", again.status, again.header.Get("Idempotent-Replayed"), again.body)
	}
	st := s.Stats()
	if st.CreateCalls != 2 || st.Payments != 1 || st.Charges != 1 || st.Replays != 1 {
		t.Errorf("stats = %+v", st)
	}
	if byKey(url, "nope").status != 404 {
		t.Error("unknown key should be 404")
	}
}

func TestKeyReuseWithADifferentRequestIsRefused(t *testing.T) {
	s, url := start(t, mockpay.Config{})
	create(url, "k", "ref", 700)
	for name, body := range map[string]string{
		"amount":    reqBody("ref", 701),
		"reference": reqBody("other", 700),
		"payee":     `{"reference":"ref","from":"alice","to":"carol","amount_minor":700,"currency":"CAD"}`,
		"currency":  `{"reference":"ref","from":"alice","to":"bob","amount_minor":700,"currency":"USD"}`,
	} {
		if r := do("POST", url+"/v1/payments", "k", body); r.status != 422 {
			t.Errorf("changed %s: %d, want 422", name, r.status)
		}
	}
	if st := s.Stats(); st.Payments != 1 || st.KeyReuse != 4 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRequestValidation(t *testing.T) {
	_, url := start(t, mockpay.Config{})
	ok := reqBody("r", 5)
	tests := []struct{ name, key, body string }{
		{"no key", "", ok},
		{"key too long", strings.Repeat("k", 256), ok},
		{"empty body", "k", ""},
		{"not json", "k", "pay me"},
		{"unknown field", "k", `{"reference":"r","from":"a","to":"b","amount_minor":5,"currency":"CAD","fee":1}`},
		{"no reference", "k", `{"from":"a","to":"b","amount_minor":5,"currency":"CAD"}`},
		{"zero amount", "k", reqBody("r", 0)},
		{"negative amount", "k", reqBody("r", -1)},
		{"huge amount", "k", reqBody("r", 1_000_000_001)},
		{"float amount", "k", `{"reference":"r","from":"a","to":"b","amount_minor":5.5,"currency":"CAD"}`},
		{"bad currency", "k", `{"reference":"r","from":"a","to":"b","amount_minor":5,"currency":"EUR"}`},
		{"oversize body", "k", `{"reference":"` + strings.Repeat("x", 1<<15) + `"}`},
	}
	for _, tc := range tests {
		if r := do("POST", url+"/v1/payments", tc.key, tc.body); r.status < 400 || r.status >= 500 {
			t.Errorf("%s: status %d, want a 4xx", tc.name, r.status)
		}
	}
	if byKey(url, "").status != 400 {
		t.Error("read without a key should be 400")
	}
}

func TestConcurrentDuplicatesCreateOnePayment(t *testing.T) {
	s, url := start(t, mockpay.Config{LatencyMedian: 5 * time.Millisecond})
	const n = 30
	replies := make([]reply, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replies[i] = create(url, "dup", "ref", 500)
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for _, r := range replies {
		if r.status == 201 {
			created++
		} else if r.status != 200 {
			t.Errorf("status %d: %s", r.status, r.body)
		}
		if r.payment(t).ID != replies[0].payment(t).ID {
			t.Error("replies disagree on the payment")
		}
	}
	if created != 1 {
		t.Errorf("%d calls reported creating the payment, want 1", created)
	}
	if st := s.Stats(); st.Payments != 1 || st.Charges != 1 || st.CreateCalls != n || st.Replays != n-1 {
		t.Errorf("stats = %+v", st)
	}
}

// Two concurrent calls with one key and different requests: exactly one defines the
// payment and the other is refused; neither silently replays the wrong payment.
func TestConcurrentKeyReuseIsRefusedNotReplayed(t *testing.T) {
	for range 20 {
		s, url := start(t, mockpay.Config{})
		var a, b reply
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a = create(url, "k", "ref", 100) }()
		go func() { defer wg.Done(); b = create(url, "k", "ref", 200) }()
		wg.Wait()
		if (a.status == 201) == (b.status == 201) || (a.status != 422 && b.status != 422) {
			t.Fatalf("statuses %d and %d: want one 201 and one 422", a.status, b.status)
		}
		if s.Stats().Payments != 1 {
			t.Fatalf("payments = %d", s.Stats().Payments)
		}
	}
}

func TestLatencyIsApplied(t *testing.T) {
	_, url := start(t, mockpay.Config{LatencyMedian: 80 * time.Millisecond})
	t0 := time.Now()
	create(url, "k", "r", 5)
	if d := time.Since(t0); d < 80*time.Millisecond {
		t.Errorf("create took %v, want at least the 80 ms median (sigma 0)", d)
	}
	t0 = time.Now()
	byKey(url, "k")
	if d := time.Since(t0); d < 80*time.Millisecond {
		t.Errorf("read took %v, want at least 80 ms", d)
	}
}

// ---- faults ----

// Faults are a function of (seed, key, call number): the same keys get the same
// outcomes whatever order, or concurrency, they arrive in. A different seed differs.
func TestFaultsAreAFunctionOfSeedKeyAndCall(t *testing.T) {
	cfg := mockpay.Config{Seed: 7, RateDecline: 0.2, RateTimeoutBefore: 0.1, Rate5xxBefore: 0.1, RateTimeoutAfter: 0.1, Rate5xxAfter: 0.1}
	keys := make([]string, 60)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	// Two calls per key, so call numbers 1 and 2 are both exercised.
	run := func(cfg mockpay.Config, order []string, parallel bool) map[string]string {
		_, url := start(t, cfg)
		out := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, k := range order {
			one := func() {
				defer wg.Done()
				// The two calls of one key are sequential so they are calls 1 and 2.
				a := create(url, k, "r", 100)
				b := create(url, k, "r", 100)
				mu.Lock()
				out[k] = fmt.Sprintf("%d/%s %d/%s", a.status, status(a), b.status, status(b))
				mu.Unlock()
			}
			wg.Add(1)
			if parallel {
				go one()
			} else {
				one()
			}
		}
		wg.Wait()
		return out
	}
	reversed := slices.Clone(keys)
	slices.Reverse(reversed)
	a := run(cfg, keys, false)
	b := run(cfg, reversed, true)
	for _, k := range keys {
		if a[k] != b[k] {
			t.Errorf("%s: %q in order, %q reversed and concurrent", k, a[k], b[k])
		}
	}
	other := cfg
	other.Seed = 8
	c := run(other, keys, false)
	same := 0
	for _, k := range keys {
		if a[k] == c[k] {
			same++
		}
	}
	if same == len(keys) {
		t.Error("a different seed gave identical outcomes for every key")
	}
	kinds := map[string]bool{}
	for _, v := range a {
		kinds[v] = true
	}
	if len(kinds) < 4 {
		t.Errorf("only %d distinct outcome patterns over %d keys; the faults are not being drawn", len(kinds), len(keys))
	}
}

func status(r reply) string {
	if r.status == 0 {
		return "aborted"
	}
	if r.status < 300 {
		var p mockpay.Payment
		_ = json.Unmarshal(r.body, &p)
		return p.Status
	}
	return "error"
}

func TestObservedRatesMatchConfiguredRates(t *testing.T) {
	cfg := mockpay.Config{Seed: 3, RateDecline: 0.10, RateTimeoutBefore: 0.05, Rate5xxBefore: 0.05, RateTimeoutAfter: 0.05, Rate5xxAfter: 0.05}
	s, url := start(t, cfg)
	const n = 4000
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			create(url, fmt.Sprintf("rate-%d", i), "r", 100)
		}()
	}
	wg.Wait()
	st := s.Stats()
	check := func(name string, got int64, p float64) {
		t.Helper()
		mean, sd := p*n, math.Sqrt(n*p*(1-p))
		if math.Abs(float64(got)-mean) > 5*sd {
			t.Errorf("%s: %d of %d, expected about %.0f (+/- %.0f)", name, got, n, mean, 5*sd)
		}
	}
	check("declined", st.Declined, 0.10)
	check("timeout before", st.TimeoutBefore, 0.05)
	check("5xx before", st.Errors5xxBef, 0.05)
	check("timeout after", st.TimeoutAfter, 0.05)
	check("5xx after", st.Errors5xxAft, 0.05)
	// A payment exists exactly when the call got past the "before" faults.
	check("payments", st.Payments, 0.90)
	check("charges", st.Charges, 0.80)
	if st.Payments != st.Charges+st.Declined {
		t.Errorf("payments %d != charges %d + declined %d", st.Payments, st.Charges, st.Declined)
	}
}

// After a fault where the provider had already acted, the payment exists and a read
// by key finds it; after a fault where it had not, nothing exists. This is the
// distinction reconciliation depends on.
func TestLostResponsesAndWhatTheyLeaveBehind(t *testing.T) {
	t.Run("after: created, response lost", func(t *testing.T) {
		s, url := start(t, mockpay.Config{Seed: 1, RateTimeoutAfter: 0.5, Rate5xxAfter: 0.3})
		lost := 0
		for i := range 40 {
			key := fmt.Sprintf("after-%d", i)
			first := create(url, key, "r", 100)
			if first.status == 201 {
				continue
			}
			lost++
			got := byKey(url, key)
			if got.status != 200 || got.payment(t).Status != "succeeded" {
				t.Fatalf("%s: response lost (%d) but read = %d %s", key, first.status, got.status, got.body)
			}
			// Retrying with the same key finds the payment (some retries are lost too).
			var final reply
			for range 30 {
				if final = create(url, key, "r", 100); final.status == 200 {
					break
				}
			}
			if final.status != 200 || !same(final.payment(t), got.payment(t)) {
				t.Fatalf("%s: retry did not return the stored payment: %d %s", key, final.status, final.body)
			}
		}
		if lost == 0 {
			t.Fatal("no response was lost; the test proved nothing")
		}
		if st := s.Stats(); st.Payments != st.Charges || st.Payments > 40 {
			t.Errorf("stats = %+v: a retry created a second payment", st)
		}
	})
	t.Run("before: nothing happened", func(t *testing.T) {
		s, url := start(t, mockpay.Config{Seed: 1, RateTimeoutBefore: 0.5, Rate5xxBefore: 0.3})
		failed := 0
		for i := range 40 {
			key := fmt.Sprintf("before-%d", i)
			if r := create(url, key, "r", 100); r.status == 201 {
				continue
			}
			failed++
			if got := byKey(url, key); got.status != 404 {
				t.Fatalf("%s: failed before the provider acted but read = %d %s", key, got.status, got.body)
			}
		}
		if failed == 0 {
			t.Fatal("no call failed; the test proved nothing")
		}
		if st := s.Stats(); st.Payments != int64(40-failed) {
			t.Errorf("payments = %d, want %d", st.Payments, 40-failed)
		}
	})
}

// A decline is decided once, with the payment: a later call returns it, even if that
// call's own draw would have been a success.
func TestADeclineIsFinal(t *testing.T) {
	_, url := start(t, mockpay.Config{Seed: 5, RateDecline: 0.5})
	declined := 0
	for i := range 60 {
		key := fmt.Sprintf("d-%d", i)
		first := create(url, key, "r", 100)
		if first.status != 201 {
			t.Fatalf("status %d", first.status)
		}
		p := first.payment(t)
		if p.Status == "failed" {
			declined++
			if p.FailureCode == nil || *p.FailureCode != mockpay.FailureCode {
				t.Errorf("declined payment has failure code %v", p.FailureCode)
			}
		}
		for range 5 {
			if again := create(url, key, "r", 100); !same(again.payment(t), p) {
				t.Fatalf("%s changed on a retry: %+v then %+v", key, p, again.payment(t))
			}
		}
	}
	if declined < 15 || declined > 45 {
		t.Errorf("%d of 60 declined at a rate of 0.5", declined)
	}
}

func TestReadErrors(t *testing.T) {
	s, url := start(t, mockpay.Config{RateReadError: 1})
	create(url, "k", "r", 100)
	if r := byKey(url, "k"); r.status != 503 {
		t.Errorf("read = %d, want 503", r.status)
	}
	if st := s.Stats(); st.Reads != 1 || st.ReadErrors != 1 {
		t.Errorf("stats = %+v", st)
	}
	// Reads have their own random stream: they cannot change what a create draws.
	_, url2 := start(t, mockpay.Config{Seed: 4, RateDecline: 0.5, RateReadError: 0.5})
	_, url3 := start(t, mockpay.Config{Seed: 4, RateDecline: 0.5})
	for i := range 30 {
		key := fmt.Sprintf("r-%d", i)
		byKey(url2, key) // reads before and after must not matter
		a := create(url2, key, "r", 1).payment(t)
		byKey(url2, key)
		b := create(url3, key, "r", 1).payment(t)
		if a.Status != b.Status {
			t.Fatalf("%s: a read changed the create outcome (%s vs %s)", key, a.Status, b.Status)
		}
	}
}

func TestDebugListsPaymentsWithCallCounts(t *testing.T) {
	s, url := start(t, mockpay.Config{Seed: 1, RateTimeoutBefore: 1})
	create(url, "a", "ref-a", 100) // lost before the provider acted: no payment
	_, url = start(t, mockpay.Config{})
	create(url, "k1", "ref-1", 100)
	create(url, "k1", "ref-1", 100)
	create(url, "k2", "ref-2", 200)
	r := do("GET", url+"/debug/payments", "", "")
	var out struct {
		Payments []mockpay.Ledger `json:"payments"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || len(out.Payments) != 2 {
		t.Fatalf("debug = %d %s (%v)", r.status, r.body, err)
	}
	if p := out.Payments[0]; p.IdempotencyKey != "k1" || p.CreateCalls != 2 || p.Processed != 2 || p.Reference != "ref-1" {
		t.Errorf("first = %+v", p)
	}
	if got := s.Payments(); len(got) != 0 {
		t.Errorf("first server holds %d payments, want 0", len(got))
	}
}

// ---- webhooks ----

type hook struct {
	mu     sync.Mutex
	events []received
	status int // answer to give; 0 means 200
}

type received struct {
	at    time.Time
	event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Payment mockpay.Payment `json:"payment"`
		} `json:"data"`
	}
	sigErr error
}

const secret = "whsec_test"

func newHook(t *testing.T) (*hook, string) {
	t.Helper()
	h := &hook{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rc received
		rc.at = time.Now()
		_ = json.Unmarshal(body, &rc.event)
		rc.sigErr = paysig.Verify([]byte(secret), r.Header.Get(paysig.Header), body, time.Now(), time.Minute)
		h.mu.Lock()
		h.events = append(h.events, rc)
		code := h.status
		h.mu.Unlock()
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(ts.Close)
	return h, ts.URL
}

func (h *hook) wait(t *testing.T, n int) []received {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if len(h.events) >= n {
			out := slices.Clone(h.events)
			h.mu.Unlock()
			return out
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("waited for %d webhook deliveries, got %d", n, len(h.events))
	return nil
}

func (h *hook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.events)
}

func TestWebhooksAreSignedAndOrdered(t *testing.T) {
	h, hookURL := newHook(t)
	s, url := start(t, mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret})
	create(url, "k", "settlement-9", 450)
	ev := h.wait(t, 2)
	if len(ev) != 2 || ev[0].event.Type != "payment.processing" || ev[1].event.Type != "payment.succeeded" {
		t.Fatalf("events = %+v", ev)
	}
	for _, e := range ev {
		if e.sigErr != nil {
			t.Errorf("%s: signature: %v", e.event.Type, e.sigErr)
		}
		if p := e.event.Data.Payment; p.Reference != "settlement-9" || p.AmountMinor != 450 || p.IdempotencyKey != "k" {
			t.Errorf("%s payload = %+v", e.event.Type, p)
		}
	}
	if ev[0].event.Data.Payment.Status != "processing" || ev[1].event.Data.Payment.Status != "succeeded" {
		t.Errorf("statuses: %q then %q", ev[0].event.Data.Payment.Status, ev[1].event.Data.Payment.Status)
	}
	if ev[0].event.ID == ev[1].event.ID {
		t.Error("two different events share an id")
	}

	// A replayed create is not a new payment and sends nothing.
	create(url, "k", "settlement-9", 450)
	time.Sleep(100 * time.Millisecond)
	if h.count() != 2 {
		t.Errorf("a replay sent %d extra webhooks", h.count()-2)
	}
	if st := s.Stats(); st.WebhookEvents != 2 || st.WebhookDeliveries != 2 || st.WebhookNon2xxOrFail != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestADeclinedPaymentSendsAFailedEvent(t *testing.T) {
	h, hookURL := newHook(t)
	_, url := start(t, mockpay.Config{RateDecline: 1, WebhookURL: hookURL, WebhookSecret: secret})
	create(url, "k", "r", 100)
	ev := h.wait(t, 2)
	if last := ev[1].event; last.Type != "payment.failed" || last.Data.Payment.Status != "failed" || last.Data.Payment.FailureCode == nil {
		t.Errorf("terminal event = %+v", last)
	}
	if ev[0].event.Data.Payment.FailureCode != nil {
		t.Error("the processing event already shows a failure")
	}
}

func TestDuplicateWebhooksShareAnEventID(t *testing.T) {
	h, hookURL := newHook(t)
	s, url := start(t, mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret, WebhookRateDuplicate: 1, WebhookDelayMax: 30 * time.Millisecond})
	create(url, "k", "r", 100)
	ev := h.wait(t, 4)
	ids := map[string]int{}
	for _, e := range ev {
		ids[e.event.ID]++
		if e.sigErr != nil {
			t.Errorf("signature: %v", e.sigErr)
		}
	}
	if len(ids) != 2 {
		t.Errorf("event ids = %v, want 2 distinct ids each delivered twice", ids)
	}
	for id, n := range ids {
		if n != 2 {
			t.Errorf("%s delivered %d times", id, n)
		}
	}
	if st := s.Stats(); st.WebhookDuplicates != 2 || st.WebhookDeliveries != 4 {
		t.Errorf("stats = %+v", st)
	}
}

func TestDroppedWebhooksAreNeverSent(t *testing.T) {
	h, hookURL := newHook(t)
	s, url := start(t, mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret, WebhookRateDrop: 1})
	create(url, "k", "r", 100)
	time.Sleep(150 * time.Millisecond)
	if h.count() != 0 {
		t.Errorf("%d webhooks arrived, want none", h.count())
	}
	if st := s.Stats(); st.WebhookDropped != 2 || st.WebhookEvents != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestOutOfOrderWebhooksArriveTerminalFirst(t *testing.T) {
	h, hookURL := newHook(t)
	s, url := start(t, mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret, WebhookRateOutOfOrder: 1, WebhookDelayMax: 100 * time.Millisecond})
	for i := range 5 {
		create(url, fmt.Sprintf("k%d", i), "r", 100)
	}
	ev := h.wait(t, 10)
	firstSeen := map[string]string{} // payment id -> type of the first event that arrived
	for _, e := range ev {
		if _, ok := firstSeen[e.event.Data.Payment.IdempotencyKey]; !ok {
			firstSeen[e.event.Data.Payment.IdempotencyKey] = e.event.Type
		}
	}
	for key, typ := range firstSeen {
		if typ != "payment.succeeded" {
			t.Errorf("%s: first event to arrive was %s, want the terminal one", key, typ)
		}
	}
	if st := s.Stats(); st.WebhookOutOfOrder != 5 {
		t.Errorf("stats = %+v", st)
	}
}

// Random delays alone must not reorder a payment's events; only the out-of-order fault does.
func TestDelaysAloneDoNotReorderEvents(t *testing.T) {
	h, hookURL := newHook(t)
	_, url := start(t, mockpay.Config{Seed: 9, WebhookURL: hookURL, WebhookSecret: secret, WebhookDelayMax: 60 * time.Millisecond})
	const n = 25
	for i := range n {
		create(url, fmt.Sprintf("k%d", i), "r", 100)
	}
	ev := h.wait(t, 2*n)
	seen := map[string][]string{}
	for _, e := range ev {
		k := e.event.Data.Payment.IdempotencyKey
		seen[k] = append(seen[k], e.event.Type)
	}
	for k, types := range seen {
		if !slices.Equal(types, []string{"payment.processing", "payment.succeeded"}) {
			t.Errorf("%s: events arrived as %v", k, types)
		}
	}
}

func TestAFailingReceiverIsCounted(t *testing.T) {
	h, hookURL := newHook(t)
	h.status = 500
	s, url := start(t, mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret})
	create(url, "k", "r", 100)
	h.wait(t, 2)
	time.Sleep(50 * time.Millisecond)
	if st := s.Stats(); st.WebhookNon2xxOrFail != 2 {
		t.Errorf("stats = %+v, want 2 failures", st)
	}
}

func TestCloseStopsPendingWebhooks(t *testing.T) {
	h, hookURL := newHook(t)
	s, err := mockpay.New(mockpay.Config{WebhookURL: hookURL, WebhookSecret: secret, WebhookDelayMax: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	create(ts.URL, "k", "r", 100)
	t0 := time.Now()
	s.Close()
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("Close took %v with deliveries an hour away", d)
	}
	if h.count() != 0 {
		t.Errorf("%d webhooks were delivered after Close", h.count())
	}
}

// ---- configuration ----

func TestConfigValidation(t *testing.T) {
	bad := map[string]mockpay.Config{
		"negative rate":          {RateDecline: -0.1},
		"rate above one":         {Rate5xxAfter: 1.1},
		"NaN rate":               {RateReadError: math.NaN()},
		"create rates sum > 1":   {RateDecline: 0.5, RateTimeoutBefore: 0.4, Rate5xxAfter: 0.2},
		"negative latency":       {LatencyMedian: -time.Second},
		"huge sigma":             {LatencySigma: 3},
		"webhook faults, no url": {WebhookRateDrop: 0.5},
		"webhook url, no secret": {WebhookURL: "http://x"},
		"out of order, no delay": {WebhookURL: "http://x", WebhookSecret: "s", WebhookRateOutOfOrder: 1},
	}
	for name, cfg := range bad {
		if _, err := mockpay.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := mockpay.Config{RateDecline: 0.2, RateTimeoutBefore: 0.2, Rate5xxBefore: 0.2, RateTimeoutAfter: 0.2, Rate5xxAfter: 0.2, RateReadError: 1}
	if s, err := mockpay.New(ok); err != nil {
		t.Errorf("rates summing to exactly 1 refused: %v", err)
	} else {
		s.Close()
	}
}
