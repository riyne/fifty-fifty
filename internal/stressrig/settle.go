package stressrig

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"

	"github.com/google/uuid"
)

// SettleUsers is how many users take part in one settle round: two creditors (who pay
// for expenses) and four debtors.
const SettleUsers = 6

// settleLoop runs settle rounds until ctx ends. Each round is a new group, so its
// balances can be frozen and the capacity invariant is exact (a settlement that the
// group lock should have refused cannot be hidden by a later expense or settlement).
func (e *Env) settleLoop(ctx context.Context, users []User, stream uint64, pause func()) {
	rng := newRNG(e.Seed, stream)
	parent := context.WithoutCancel(ctx)
	for ctx.Err() == nil {
		if err := e.settleRound(parent, rng, users); err != nil {
			e.Stats.Inc("settle_round_failed")
			e.Stats.Unexpected("settle round: %v", err)
		}
		pause()
	}
}

// settleRound: a fresh group; two creditors each pay for an expense whose items are
// claimed one per debtor, so every debtor owes both; balances are read and recorded as
// frozen; then, released together, each debtor tries several settlements of about 55-60%
// of what they owe (two of them together are more than they owe, so the group lock must
// refuse all but one per debtor), some sent twice with one key at the same moment; then a
// few small ones, sent with retries.
func (e *Env) settleRound(ctx context.Context, rng *rand.Rand, users []User) error {
	members := append([]User(nil), users[:SettleUsers]...)
	rng.Shuffle(len(members), func(i, j int) { members[i], members[j] = members[j], members[i] })
	gid, err := e.NewGroup(ctx, members)
	if err != nil {
		return err
	}
	g := &group{id: gid, users: members}
	creditors, debtors := []int{0, 1}, []int{2, 3, 4, 5}

	for _, c := range creditors {
		x := e.createExpense(ctx, rng, g, c, len(debtors), func() int64 { return int64(2000 + rng.IntN(7000)) }, false)
		if x == nil {
			return fmt.Errorf("setup: expense not created")
		}
		for i, d := range debtors {
			if r := e.claim(ctx, g, x, d, x.items[i], false); r.Status != http.StatusOK {
				return fmt.Errorf("setup: claim = %d %s %v", r.Status, r.Body, r.Err)
			}
		}
		path := fmt.Sprintf("/v1/groups/%s/expenses/%s/finalize", gid, x.id)
		if r := e.Client.Do(ctx, members[c].Token, "POST", path, nil, uuid.NewString()); r.Status != http.StatusOK {
			return fmt.Errorf("setup: finalize = %d %s %v", r.Status, r.Body, r.Err)
		}
	}
	bal, err := Balances(ctx, e.Pool, gid)
	if err != nil {
		return err
	}
	e.recordFrozen(Frozen{Group: gid, Balance: bal})

	var fns []func()
	opseq := int64(0)
	for _, d := range debtors {
		debt := -bal[members[d].ID]
		if debt <= 0 {
			return fmt.Errorf("setup: debtor %d owes %d", d, debt)
		}
		for k := 0; k < 3; k++ {
			opseq++
			to := creditors[rng.IntN(len(creditors))]
			amount := debt*55/100 + opseq // distinct per operation, below the debt
			srng := newRNG(rng.Uint64(), uint64(opseq))
			fns = append(fns, func() { e.settleOp(ctx, srng, gid, members[d], members[to], amount, false) })
		}
	}
	fire(fns...)

	// Small follow-ups, one at a time, each sent twice with one key.
	for _, d := range debtors {
		debt := -bal[members[d].ID]
		for k := 0; k < 2; k++ {
			opseq++
			e.settleOp(ctx, rng, gid, members[d], members[creditors[rng.IntN(len(creditors))]], debt/10+opseq, true)
		}
	}
	e.Stats.Inc("settle_rounds")
	return nil
}

// settleOp is one logical settlement: one idempotency key, sent with the usual patterns,
// or (retry) once and then again after the answer. It records what the client was told so
// the checker can compare it with the rows.
func (e *Env) settleOp(ctx context.Context, rng *rand.Rand, gid uuid.UUID, from, to User, amount int64, retry bool) {
	key := uuid.NewString()
	path := fmt.Sprintf("/v1/groups/%s/settlements", gid)
	body := map[string]any{"to_member_id": to.ID, "amount_minor": amount}
	send := func(c context.Context) Resp { return e.Client.Do(c, from.Token, "POST", path, body, key) }
	var resps []Resp
	if retry {
		e.Stats.Inc("pattern/settle/retry")
		for i := 0; i < 2; i++ {
			r := send(ctx)
			resps = append(resps, r)
			e.count("settle", r)
		}
	} else {
		resps = e.send(ctx, rng, "settle", send)
	}
	var lg logical
	for _, r := range resps {
		lg.add(e, "settle", r, "exceeds_available")
	}
	e.record(Create{Group: gid, Settlement: true, From: from.ID, To: to.ID, Amount: amount, Rows: lg.rows()})
}
