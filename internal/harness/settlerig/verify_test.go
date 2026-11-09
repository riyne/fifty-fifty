package settlerig_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/harness/mockpay"
	"github.com/riyne/fifty-fifty/internal/harness/settlerig"
	"github.com/riyne/fifty-fifty/internal/harness/testutil"
	"github.com/riyne/fifty-fifty/internal/settlements"
)

var ctx = context.Background()

type fixture struct {
	t        *testing.T
	group    uuid.UUID
	from, to uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.SharedOwnerPool(t)
	f := &fixture{t: t}
	for _, id := range []*uuid.UUID{&f.from, &f.to} {
		if err := pool.QueryRow(ctx, `INSERT INTO users (issuer, subject) VALUES ('test', $1) RETURNING id`, testutil.UniqueSubject("rig")).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO groups (name, currency, created_by) VALUES ('g', 'CAD', $1) RETURNING id`, f.from).Scan(&f.group); err != nil {
		t.Fatal(err)
	}
	for _, m := range []uuid.UUID{f.from, f.to} {
		if _, err := pool.Exec(ctx, `INSERT INTO memberships (group_id, user_id, role) VALUES ($1, $2, 'member')`, f.group, m); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// settlement inserts one and brings it to status through Resolve (the real path).
func (f *fixture) settlement(status string) uuid.UUID {
	f.t.Helper()
	pool := testutil.SharedOwnerPool(f.t)
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO settlements (group_id, from_member, to_member, amount_minor) VALUES ($1, $2, $3, 700) RETURNING id`, f.group, f.from, f.to).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	var o *settlements.Outcome
	switch status {
	case "succeeded":
		x := settlements.Success()
		o = &x
	case "failed":
		x := settlements.Failure("declined")
		o = &x
	case "unknown":
		x := settlements.AmbiguousResult()
		o = &x
	}
	if o != nil {
		tx, _ := pool.Begin(ctx)
		if _, err := settlements.Resolve(ctx, tx, id, *o); err != nil {
			f.t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}

func (f *fixture) payment(id uuid.UUID, status string, mutate func(*mockpay.Ledger)) mockpay.Ledger {
	p := mockpay.Ledger{Payment: mockpay.Payment{
		ID: "pay_" + uuid.NewString(), IdempotencyKey: id.String(), Reference: id.String(), From: f.from.String(), To: f.to.String(),
		AmountMinor: 700, Currency: "CAD", Status: status,
	}, CreateCalls: 1, Processed: 1}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// provider serves payments the way mockpay's /debug/payments does.
func provider(t *testing.T, payments ...mockpay.Ledger) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"payments": payments})
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func verify(t *testing.T, ids []uuid.UUID, payments ...mockpay.Ledger) settlerig.Report {
	t.Helper()
	got, err := settlerig.FetchProvider(ctx, provider(t, payments...))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := settlerig.Verify(ctx, testutil.SharedOwnerPool(t), got, ids)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestVerify_ACleanRunHasNoAnomalies(t *testing.T) {
	f := newFixture(t)
	ok, declined, inFlight := f.settlement("succeeded"), f.settlement("failed"), f.settlement("unknown")
	rep := verify(t, []uuid.UUID{ok, declined, inFlight},
		f.payment(ok, "succeeded", nil),
		f.payment(declined, "failed", nil),
		f.payment(inFlight, "succeeded", nil)) // charged, still being reconciled: not yet an anomaly
	if len(rep.Anomalies) != 0 {
		t.Fatalf("anomalies: %v", rep.Anomalies)
	}
	if rep.Settlements != 3 || rep.Postings != 1 || rep.ProviderCharges != 2 || rep.InFlightCharged != 1 || rep.Unresolved() != 1 {
		t.Errorf("report = %+v", rep)
	}
}

// The checker must be able to fail: each of these is a state it has to call wrong.
func TestVerify_FlagsEachKindOfWrongState(t *testing.T) {
	f := newFixture(t)
	for name, tc := range map[string]struct {
		status   string
		payments func(id uuid.UUID) []mockpay.Ledger
		want     string
	}{
		"succeeded, but the provider holds nothing": {"succeeded", func(uuid.UUID) []mockpay.Ledger { return nil }, "succeeded payments"},
		"succeeded, but the provider's payment failed": {"succeeded", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "failed", nil)}
		}, "succeeded payments"},
		"failed, but the provider charged": {"failed", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "succeeded", nil)}
		}, "is failed but the provider charged"},
		"charged twice": {"succeeded", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "succeeded", nil), f.payment(id, "succeeded", func(p *mockpay.Ledger) { p.IdempotencyKey = "other-key" })}
		}, "charged it 2 times"},
		"created twice": {"failed", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "failed", nil), f.payment(id, "failed", func(p *mockpay.Ledger) { p.IdempotencyKey = "other-key" })}
		}, "a double create"},
		"the amount differs": {"succeeded", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "succeeded", func(p *mockpay.Ledger) { p.AmountMinor = 701 })}
		}, "differs from it"},
		"the payee differs": {"succeeded", func(id uuid.UUID) []mockpay.Ledger {
			return []mockpay.Ledger{f.payment(id, "succeeded", func(p *mockpay.Ledger) { p.To = f.from.String() })}
		}, "differs from it"},
	} {
		id := f.settlement(tc.status)
		rep := verify(t, []uuid.UUID{id}, tc.payments(id)...)
		if len(rep.Anomalies) == 0 || !strings.Contains(strings.Join(rep.Anomalies, "\n"), tc.want) {
			t.Errorf("%s: anomalies %v, want one mentioning %q", name, rep.Anomalies, tc.want)
		}
	}
}

func TestVerify_ScopesToTheSettlementsItIsGiven(t *testing.T) {
	f := newFixture(t)
	mine, other := f.settlement("succeeded"), f.settlement("succeeded")
	// The provider holds a payment for a settlement that is not in the set: ignored.
	rep := verify(t, []uuid.UUID{mine}, f.payment(mine, "succeeded", nil), f.payment(other, "succeeded", func(p *mockpay.Ledger) { p.AmountMinor = 1 }))
	if len(rep.Anomalies) != 0 || rep.Settlements != 1 {
		t.Errorf("report = %+v", rep)
	}
}
