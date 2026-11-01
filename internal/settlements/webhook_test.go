package settlements_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/mockpay"
	"github.com/riyne/fifty-fifty/internal/paysig"
	"github.com/riyne/fifty-fifty/internal/settlements"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

const whSecret = "whsec_test_secret"

// hookRig is a world with a settlement in the database and an api that accepts webhooks.
type hookRig struct {
	*world
	s *teststack.Stack
}

func newHookRig(t *testing.T) *hookRig {
	t.Helper()
	return &hookRig{world: newWorld(t), s: teststack.New(t, teststack.WithWebhookSecret(whSecret))}
}

func (h *hookRig) eventBody(typ string, id uuid.UUID, mutate func(m map[string]any)) []byte {
	status, code := "succeeded", any(nil)
	switch typ {
	case "payment.failed":
		status, code = "failed", "card_declined"
	case "payment.processing":
		status = "processing"
	}
	pay := map[string]any{
		"id": "pay_1", "idempotency_key": id.String(), "reference": id.String(), "from": h.from.String(), "to": h.to.String(),
		"amount_minor": 700, "currency": "CAD", "status": status, "failure_code": code,
	}
	m := map[string]any{"id": "evt_" + uuid.NewString(), "type": typ, "data": map[string]any{"payment": pay}}
	if mutate != nil {
		mutate(pay)
	}
	b, _ := json.Marshal(m)
	return b
}

// deliver posts body signed at `at` with the given secret; a nil header means unsigned.
func (h *hookRig) deliver(t *testing.T, body []byte, secret string, at time.Time) teststack.Response {
	t.Helper()
	var headers []string
	if secret != "" {
		sig, err := paysig.Sign([]byte(secret), at, body)
		if err != nil {
			t.Fatal(err)
		}
		headers = []string{paysig.Header, sig}
	}
	return h.s.Do(t, "", "POST", settlements.WebhookPath, string(body), headers...)
}

func (h *hookRig) send(t *testing.T, body []byte) teststack.Response {
	return h.deliver(t, body, whSecret, time.Now())
}

func result(t *testing.T, r teststack.Response) string {
	t.Helper()
	var out struct {
		Received bool   `json:"received"`
		Result   string `json:"result"`
	}
	r.JSON(t, &out)
	if r.Status != 200 || !out.Received {
		t.Fatalf("response %d: %s", r.Status, r.Body)
	}
	return out.Result
}

func TestWebhook_SuccessAndFailureResolveTheSettlement(t *testing.T) {
	h := newHookRig(t)

	ok := h.settlement(t, 700)
	if got := result(t, h.send(t, h.eventBody("payment.succeeded", ok, nil))); got != "apply" {
		t.Errorf("result = %q", got)
	}
	if row := h.row(t, ok); row.status != "succeeded" || row.ledgerTx == nil || h.postings(t, ok) != 1 {
		t.Errorf("settlement = %+v, postings %d", row, h.postings(t, ok))
	}
	if b := h.balances(t); b[h.from] != 700 || b[h.to] != -700 {
		t.Errorf("balances = %v", b)
	}

	declined := h.settlement(t, 700)
	result(t, h.send(t, h.eventBody("payment.failed", declined, nil)))
	if row := h.row(t, declined); row.status != "failed" || row.failureCode == nil || *row.failureCode != "card_declined" || h.postings(t, declined) != 0 {
		t.Errorf("declined settlement = %+v", row)
	}

	// An unknown settlement (the response was lost) is resolved the same way.
	unknown := h.settlement(t, 700)
	h.resolve(t, unknown, settlements.AmbiguousResult())
	result(t, h.send(t, h.eventBody("payment.succeeded", unknown, nil)))
	if h.row(t, unknown).status != "succeeded" || h.postings(t, unknown) != 1 {
		t.Error("an unknown settlement was not resolved by the webhook")
	}
}

// Duplicates, in any number and at the same time, post once.
func TestWebhook_DuplicateDeliveriesPostOnce(t *testing.T) {
	h := newHookRig(t)
	id := h.settlement(t, 700)
	body := h.eventBody("payment.succeeded", id, nil)

	const n = 12
	results := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = result(t, h.send(t, body))
		}()
	}
	close(start)
	wg.Wait()
	applied := 0
	for _, r := range results {
		if r == "apply" {
			applied++
		} else if r != "ignore" {
			t.Errorf("result %q", r)
		}
	}
	if applied != 1 || h.postings(t, id) != 1 {
		t.Errorf("%d deliveries applied, %d postings; want 1 and 1", applied, h.postings(t, id))
	}
	if got := h.balances(t); got[h.from] != 700 {
		t.Errorf("balances = %v", got)
	}
}

func TestWebhook_OutOfOrderAndContradictoryEvents(t *testing.T) {
	h := newHookRig(t)

	// The terminal event first, then the processing event that should have preceded it.
	id := h.settlement(t, 700)
	result(t, h.send(t, h.eventBody("payment.succeeded", id, nil)))
	if got := result(t, h.send(t, h.eventBody("payment.processing", id, nil))); got != "ignored" {
		t.Errorf("late processing event: %q", got)
	}
	if h.row(t, id).status != "succeeded" || h.postings(t, id) != 1 {
		t.Error("a late processing event disturbed the settlement")
	}

	// A failure after a recorded success, and a success after a recorded failure: neither is
	// applied, both are recorded.
	if got := result(t, h.send(t, h.eventBody("payment.failed", id, nil))); got != "conflict" {
		t.Errorf("failure after success: %q", got)
	}
	failed := h.settlement(t, 700)
	result(t, h.send(t, h.eventBody("payment.failed", failed, nil)))
	if got := result(t, h.send(t, h.eventBody("payment.succeeded", failed, nil))); got != "conflict" {
		t.Errorf("success after failure: %q", got)
	}
	if h.row(t, id).status != "succeeded" || h.row(t, failed).status != "failed" || h.postings(t, failed) != 0 {
		t.Error("a contradicting event changed a final settlement")
	}
	if n := h.audits(t, "settlement.conflict"); n != 2 {
		t.Errorf("conflict audit events = %d, want 2", n)
	}
}

// Anything that is not a valid signature is refused, the same way, before the body is read
// as anything, and changes nothing.
func TestWebhook_RefusesWhatIsNotSigned(t *testing.T) {
	h := newHookRig(t)
	id := h.settlement(t, 700)
	body := h.eventBody("payment.succeeded", id, nil)
	tampered := bytes.Replace(body, []byte(`"amount_minor":700`), []byte(`"amount_minor":701`), 1)
	goodSig, _ := paysig.Sign([]byte(whSecret), time.Now(), body)

	var seen []string
	for name, r := range map[string]teststack.Response{
		"no header":         h.deliver(t, body, "", time.Now()),
		"wrong secret":      h.deliver(t, body, "another-secret", time.Now()),
		"expired":           h.deliver(t, body, whSecret, time.Now().Add(-6*time.Minute)),
		"from the future":   h.deliver(t, body, whSecret, time.Now().Add(6*time.Minute)),
		"body was changed":  h.s.Do(t, "", "POST", settlements.WebhookPath, string(tampered), paysig.Header, goodSig),
		"garbage header":    h.s.Do(t, "", "POST", settlements.WebhookPath, string(body), paysig.Header, "t=1,v1=zz"),
		"empty body signed": h.deliver(t, nil, whSecret, time.Now()), // valid signature, nothing to read
	} {
		want := 401
		if name == "empty body signed" {
			want = 400
		}
		if r.Status != want {
			t.Errorf("%s: status %d, want %d: %s", name, r.Status, want, r.Body)
		}
		if want == 401 {
			seen = append(seen, string(r.Body))
		}
	}
	for _, b := range seen {
		if b != seen[0] {
			t.Errorf("signature failures answered differently:\n%s\n%s", seen[0], b)
		}
	}
	if h.row(t, id).status != "pending" || h.postings(t, id) != 0 {
		t.Error("a refused webhook changed the settlement")
	}
	// The window is 5 minutes (fixed numbers here, not the code's own constant): 4 minutes
	// old is accepted, and so is a signature made just now (controls).
	if r := h.deliver(t, body, whSecret, time.Now().Add(-4*time.Minute)); r.Status != 200 {
		t.Errorf("a 4-minute-old signature: %d %s", r.Status, r.Body)
	}
	if r := h.send(t, body); r.Status != 200 {
		t.Errorf("control: %d %s", r.Status, r.Body)
	}
}

func TestWebhook_OversizeBodyIsRefusedBeforeAnythingElse(t *testing.T) {
	h := newHookRig(t)
	big := []byte(`{"pad":"` + strings.Repeat("x", 70_000) + `"}`)
	if r := h.deliver(t, big, whSecret, time.Now()); r.Status != 413 {
		t.Errorf("status %d, want 413", r.Status)
	}
}

func TestWebhook_SignedButMalformedEvents(t *testing.T) {
	h := newHookRig(t)
	id := h.settlement(t, 700)
	for name, body := range map[string][]byte{
		"not json":                       []byte(`nope`),
		"success event, failed payment":  h.eventBody("payment.succeeded", id, func(p map[string]any) { p["status"] = "failed" }),
		"failure event, success payment": h.eventBody("payment.failed", id, func(p map[string]any) { p["status"] = "succeeded" }),
	} {
		if r := h.send(t, body); r.Status != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, r.Status, r.Body)
		}
	}
	if h.row(t, id).status != "pending" {
		t.Error("a malformed event changed the settlement")
	}
}

// Events that are valid and signed but not for us are acknowledged and do nothing: a
// provider that sees an error would keep retrying.
func TestWebhook_EventsThatAreNotForUsAreAcknowledged(t *testing.T) {
	h := newHookRig(t)
	id := h.settlement(t, 700)
	for name, body := range map[string][]byte{
		"processing":            h.eventBody("payment.processing", id, nil),
		"a new event type":      h.eventBody("payment.refunded", id, nil),
		"a settlement we lack":  h.eventBody("payment.succeeded", uuid.New(), nil),
		"a key that is no uuid": h.eventBody("payment.succeeded", id, func(p map[string]any) { p["idempotency_key"] = "order-17"; p["reference"] = "order-17" }),
		"reference differs":     h.eventBody("payment.succeeded", id, func(p map[string]any) { p["reference"] = uuid.NewString() }),
	} {
		if got := result(t, h.send(t, body)); got != "ignored" {
			t.Errorf("%s: result %q, want ignored", name, got)
		}
	}
	if h.row(t, id).status != "pending" || h.postings(t, id) != 0 {
		t.Error("an event that was not for us changed the settlement")
	}
}

// A signed event that names a real settlement but disagrees about the money or the
// parties is not applied; it is recorded for a person.
func TestWebhook_AnEventThatDisagreesAboutTheMoneyIsNotApplied(t *testing.T) {
	h := newHookRig(t)
	for name, mutate := range map[string]func(p map[string]any){
		"amount":   func(p map[string]any) { p["amount_minor"] = 701 },
		"currency": func(p map[string]any) { p["currency"] = "USD" },
		"sender":   func(p map[string]any) { p["from"] = h.to.String() },
		"payee":    func(p map[string]any) { p["to"] = h.bystander.String() },
	} {
		id := h.settlement(t, 700)
		before := h.audits(t, "settlement.webhook_mismatch")
		if got := result(t, h.send(t, h.eventBody("payment.succeeded", id, mutate))); got != "ignored" {
			t.Errorf("%s: result %q", name, got)
		}
		if h.row(t, id).status != "pending" || h.postings(t, id) != 0 {
			t.Errorf("%s: a mismatched event was applied", name)
		}
		if h.audits(t, "settlement.webhook_mismatch") != before+1 {
			t.Errorf("%s: mismatch not recorded", name)
		}
	}
}

func TestWebhook_IsNotMountedWithoutASecret(t *testing.T) {
	s := teststack.New(t) // no secret
	id := uuid.New()
	body := fmt.Sprintf(`{"type":"payment.succeeded","data":{"payment":{"idempotency_key":%q}}}`, id)
	sig, _ := paysig.Sign([]byte(whSecret), time.Now(), []byte(body))
	// The route does not exist, so the request lands in the authenticated /v1 tree: refused
	// as an unauthenticated user (401), and 404 for a user who is signed in.
	if r := s.Do(t, "", "POST", settlements.WebhookPath, body, paysig.Header, sig); r.Status != 401 {
		t.Errorf("without a secret, no user token: %d, want 401", r.Status)
	}
	if r := s.Do(t, s.Token(t, "someone"), "POST", settlements.WebhookPath, body, paysig.Header, sig); r.Status != 404 {
		t.Errorf("without a secret, with a user token: %d, want 404 (no such route)", r.Status)
	}
	if _, err := settlements.NewWebhookHandler(nil, nil); err == nil {
		t.Error("a handler with an empty secret was created")
	}
}

// The reason the webhook exists: the provider took the payment but the response was lost, so
// execute can only say "unknown"; the provider's own signed events then reach the api and
// resolve the settlement, even with the events duplicated, delayed and reordered. Either
// order of (execute's commit, the webhook) ends the same, with one posting.
func TestWebhook_HealsASettlementWhoseResponseWasLost(t *testing.T) {
	stack := teststack.New(t, teststack.WithWebhookSecret(whSecret))
	r := newRig(t, mockpay.Config{
		Rate5xxAfter: 1, // the payment is created, the response is lost
		WebhookURL:   stack.Server.URL + settlements.WebhookPath, WebhookSecret: whSecret,
		WebhookDelayMax: 80 * time.Millisecond, WebhookRateDuplicate: 1, WebhookRateOutOfOrder: 1,
	})
	var ids []uuid.UUID
	var jobs []uuid.UUID
	for i := range 6 {
		id := r.settlement(t, int64(100+i))
		ids = append(ids, id)
		jobs = append(jobs, r.enqueue(t, id, 0))
	}
	r.run(t, r.executeHandler(r.client))
	for _, j := range jobs {
		if info := r.waitJob(t, j, "succeeded", "dead"); info.state != "succeeded" {
			t.Fatalf("job = %+v", info)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, id := range ids {
		for r.row(t, id).status != "succeeded" && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if r.row(t, id).status != "succeeded" {
			t.Fatalf("settlement %s is %q: the webhook did not resolve it", id, r.row(t, id).status)
		}
		if r.postings(t, id) != 1 {
			t.Errorf("settlement %s has %d postings", id, r.postings(t, id))
		}
	}
	// Let the duplicates and the late processing events arrive, then recheck.
	time.Sleep(400 * time.Millisecond)
	for _, id := range ids {
		if r.postings(t, id) != 1 || r.row(t, id).status != "succeeded" {
			t.Errorf("settlement %s changed after the remaining events: %q, %d postings", id, r.row(t, id).status, r.postings(t, id))
		}
	}
	st := r.mock.Stats()
	if st.Payments != 6 || st.Charges != 6 || st.WebhookDuplicates == 0 || st.WebhookNon2xxOrFail != 0 {
		t.Errorf("provider stats = %+v", st)
	}
}
