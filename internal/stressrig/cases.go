package stressrig

import (
	"fmt"
	"strings"
)

// Layers the negative controls look for in the api's logged errors.
const (
	LayerPostingIndex = "unique:ledger_transactions_one_settlement_posting"
	LayerExpensePost  = "unique:ledger_transactions_one_expense_posting"
	LayerOneVoid      = "unique:ledger_transactions_one_void"
	LayerMarkGuard    = "mark succeeded: settlement was not in a state"
	// LayerEditGuard is the conditional update that ends an edit (on the version read under
	// the lock and on an editable status): with the edit lock off it is what stops an edit
	// that lost a race, by moving no row.
	LayerEditGuard = "edit expense: mark edited: rows=0"
)

// ControlCase is one negative control: which protections are off and what the harness
// must see. It is shared by `make test-controls` and the bench/stress program, so the
// recorded runs are held to the same expectations as the smoke test.
type ControlCase struct {
	Name string
	// Off names the protections switched off (see internal/controls); DropIndex also drops
	// the unique index on a settlement's ledger posting in the scratch database.
	Off       []string
	DropIndex bool
	// ResolveRace uses the mock whose webhooks mostly arrive twice and soon, so that
	// resolvers meet at the same settlement (ResolveRaceMockFlags).
	ResolveRace bool
	// Violated is the exact set of invariants that must be violated (empty: none). The
	// three zero-sum invariants must hold in every case: the database enforces them.
	Violated []Invariant
	// Layers are substrings of logged error classes that must appear: the database layer
	// or guard that stopped the problem.
	Layers []string
	// Silent: nothing but clients going away may be logged by the api.
	Silent bool
}

// ControlCases is the list, in report order. The Resolve rungs follow the order in which
// the settlement success path meets its layers: the row lock, then the unique index on
// the posting (the posting is inserted first), then the guard on the status update.
var ControlCases = []ControlCase{
	{Name: "claim_lock", Off: []string{"claim_lock"}, Violated: []Invariant{SharesMatchClaims}},
	{Name: "finalize_lock", Off: []string{"finalize_lock"}, Violated: []Invariant{SharesMatchClaims}, Layers: []string{LayerExpensePost}},
	{Name: "void_lock", Off: []string{"void_lock"}, Layers: []string{LayerOneVoid}},
	{Name: "edit_version_check", Off: []string{"edit_version_check"}, Violated: []Invariant{EditStaleVersion}},
	{Name: "edit_lock", Off: []string{"edit_lock"}, Layers: []string{LayerEditGuard}},
	{Name: "idempotency_check", Off: []string{"idempotency_check"}, Violated: []Invariant{DuplicateKey}},
	{Name: "group_lock", Off: []string{"group_lock"}, Violated: []Invariant{OverCapacity}},
	{Name: "resolve_guards (lock still on)", Off: []string{"resolve_guards"}, ResolveRace: true, Silent: true},
	{Name: "resolve_lock", Off: []string{"resolve_lock"}, ResolveRace: true, Layers: []string{LayerPostingIndex}},
	{Name: "resolve_lock + posting index", Off: []string{"resolve_lock"}, DropIndex: true, ResolveRace: true, Layers: []string{LayerMarkGuard}},
	{Name: "resolve_lock + posting index + guards", Off: []string{"resolve_lock", "resolve_guards"}, DropIndex: true, ResolveRace: true,
		Violated: []Invariant{SettlementPosting, ProviderBooks}},
}

// Apply returns p with this case's protections off.
func (c ControlCase) Apply(p Params) Params {
	p.Controls, p.DropPostingIndex = c.Off, c.DropIndex
	if c.ResolveRace {
		p.MockFlags = ResolveRaceMockFlags
	}
	return p
}

// Problems lists every way a run did not match the case; empty means it matched.
func (c ControlCase) Problems(res Result, p Params) []string {
	var out []string
	for _, inv := range []Invariant{ZeroSumTransaction, ZeroSumGroup, ZeroSumLedger} {
		if n := res.Violations[inv]; n != 0 {
			out = append(out, fmt.Sprintf("%s violated %d times: the database's own zero-sum rule must hold under every control", inv, n))
		}
	}
	want := map[Invariant]bool{}
	for _, inv := range c.Violated {
		want[inv] = true
		if res.Violations[inv] == 0 {
			out = append(out, fmt.Sprintf("%s was not violated: the harness did not see the effect of disabling %v", inv, c.Off))
		}
	}
	for _, inv := range Invariants {
		if n := res.Violations[inv]; n > 0 && !want[inv] && inv != ZeroSumTransaction && inv != ZeroSumGroup && inv != ZeroSumLedger {
			out = append(out, fmt.Sprintf("%s violated %d times, which this control should not cause", inv, n))
		}
	}
	for _, layer := range c.Layers {
		found := false
		for k, n := range res.Counts {
			if n > 0 && strings.HasPrefix(k, LogErrorPrefix) && strings.Contains(k, layer) {
				found = true
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("no logged error matching %q: the layer that should have caught it was not seen", layer))
		}
	}
	if c.Silent && len(res.ServerErrors()) > 0 {
		out = append(out, fmt.Sprintf("errors logged that should not occur: %v", res.ServerErrors()))
	}
	if res.Unresolved > 0 {
		out = append(out, fmt.Sprintf("%d settlements never resolved", res.Unresolved))
	}
	for _, v := range res.Vacuous(p) {
		out = append(out, "vacuous: "+v)
	}
	return out
}
