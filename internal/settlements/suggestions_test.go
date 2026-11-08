package settlements_test

import (
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The balances endpoint's suggested transfers are what can be settled right now: net of the
// money open settlements have reserved. Each one must be a payment create-settlement
// accepts. (Before M10.20 they came from the ledger alone, so a member with a pending
// settlement was told to pay money that was already on its way and got a 409.)

type balancesView struct {
	Balances []struct {
		MemberID         uuid.UUID `json:"member_id"`
		BalanceMinor     int64     `json:"balance_minor"`
		ReservedOutMinor int64     `json:"reserved_out_minor"`
		ReservedInMinor  int64     `json:"reserved_in_minor"`
	} `json:"balances"`
	SuggestedTransfers []struct {
		From   uuid.UUID `json:"from_member_id"`
		To     uuid.UUID `json:"to_member_id"`
		Amount int64     `json:"amount_minor"`
	} `json:"suggested_transfers"`
}

func (e *env) balances(t *testing.T, as string) balancesView {
	t.Helper()
	var v balancesView
	r := e.s.Do(t, e.tok[as], "GET", "/v1/groups/"+e.group+"/balances", "")
	if r.Status != http.StatusOK {
		t.Fatalf("balances = %d: %s", r.Status, r.Body)
	}
	r.JSON(t, &v)
	return v
}

func (v balancesView) of(id uuid.UUID) (balance, out, in int64) {
	for _, b := range v.Balances {
		if b.MemberID == id {
			return b.BalanceMinor, b.ReservedOutMinor, b.ReservedInMinor
		}
	}
	return 0, 0, 0
}

func (v balancesView) suggested(from, to uuid.UUID) int64 {
	var n int64
	for _, s := range v.SuggestedTransfers {
		if s.From == from && s.To == to {
			n += s.Amount
		}
	}
	return n
}

func TestSuggestions_AreNetOfOpenSettlements(t *testing.T) {
	e := newEnv(t, "owner", "alice", "bob")
	e.owe(t, "owner", "alice", 3000)
	e.owe(t, "owner", "bob", 2000)
	owner, alice, bob := e.id["owner"], e.id["alice"], e.id["bob"]

	v := e.balances(t, "owner")
	if v.suggested(alice, owner) != 3000 || v.suggested(bob, owner) != 2000 || len(v.SuggestedTransfers) != 2 {
		t.Fatalf("nothing open: %+v, want alice 3000 and bob 2000 to the owner", v.SuggestedTransfers)
	}
	if _, out, in := v.of(alice); out != 0 || in != 0 {
		t.Fatalf("nothing is open but alice has reserved out %d in %d", out, in)
	}

	// Alice starts paying 1000: pending.
	var s settlement
	e.settle(t, "alice", "owner", 1000).JSON(t, &s)

	v = e.balances(t, "owner")
	if bal, out, in := v.of(alice); bal != -3000 || out != 1000 || in != 0 {
		t.Errorf("alice: balance %d out %d in %d, want the ledger's -3000 unchanged, 1000 out", bal, out, in)
	}
	if bal, out, in := v.of(owner); bal != 5000 || out != 0 || in != 1000 {
		t.Errorf("owner: balance %d out %d in %d, want 5000, 1000 in", bal, out, in)
	}
	if got := v.suggested(alice, owner); got != 2000 {
		t.Errorf("alice is suggested to pay %d, want 2000 (3000 owed, 1000 already on its way)", got)
	}
	if got := v.suggested(bob, owner); got != 2000 {
		t.Errorf("bob is suggested to pay %d, want 2000", got)
	}
	// The old suggestion would have been refused: this is what the change fixes.
	if r := e.settle(t, "alice", "owner", 3000); r.Status != http.StatusConflict {
		t.Errorf("creating the full 3000 with 1000 pending = %d, want 409 exceeds_available", r.Status)
	}

	// An unknown settlement (the provider did not answer yet) still reserves.
	e.s.Exec(t, `UPDATE settlements SET status = 'unknown' WHERE id = $1`, s.ID)
	v = e.balances(t, "owner")
	if _, out, _ := v.of(alice); out != 1000 || v.suggested(alice, owner) != 2000 {
		t.Errorf("unknown settlement: out %d, suggested %d, want 1000 and 2000", out, v.suggested(alice, owner))
	}

	// A failed one releases the money: the suggestion is whole again.
	e.s.Exec(t, `UPDATE settlements SET status = 'failed', failure_code = 'declined', resolved_at = now() WHERE id = $1`, s.ID)
	v = e.balances(t, "owner")
	if _, out, _ := v.of(alice); out != 0 || v.suggested(alice, owner) != 3000 {
		t.Errorf("failed settlement: out %d, suggested %d, want 0 and 3000", out, v.suggested(alice, owner))
	}
}

// Every suggestion is accepted, in the order given, and when they are all created nothing
// more is suggested. Random debts and random open settlements.
func TestSuggestions_EveryOneIsAcceptedAndTheyExhaustWhatCanBeSettled(t *testing.T) {
	names := []string{"owner", "a", "b", "c"}
	for seed := uint64(1); seed <= 6; seed++ {
		t.Run("", func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, seed*7))
			e := newEnv(t, names...)
			for range 5 {
				payer := names[rng.IntN(len(names))]
				debtor := names[rng.IntN(len(names))]
				if payer == debtor {
					continue
				}
				e.owe(t, payer, debtor, 100+rng.Int64N(5000))
			}
			// Open settlements that are real: a debtor pays part of what they can still pay a creditor,
			// read from the balances each time (so earlier ones count).
			nameOf := map[uuid.UUID]string{}
			for n, id := range e.id {
				nameOf[id] = n
			}
			for range 3 {
				v := e.balances(t, "owner")
				if len(v.SuggestedTransfers) == 0 {
					break
				}
				s := v.SuggestedTransfers[rng.IntN(len(v.SuggestedTransfers))]
				part := 1 + rng.Int64N(s.Amount) // some of it, up to all
				if r := e.settle(t, nameOf[s.From], nameOf[s.To], part); r.Status != http.StatusCreated && r.Status != http.StatusAccepted {
					t.Fatalf("seed %d: setting up an open settlement of %d: %d %s", seed, part, r.Status, r.Body)
				}
			}

			v := e.balances(t, "owner")
			open := e.count(t, `SELECT count(*) FROM settlements WHERE group_id = $1 AND status IN ('pending', 'unknown')`, e.group)
			t.Logf("seed %d: %d open settlements, %d suggestions", seed, open, len(v.SuggestedTransfers))
			if open == 0 {
				t.Fatalf("seed %d: no open settlement was made, so the test would not show reservations", seed)
			}
			var sum int64
			for _, b := range v.Balances {
				sum += b.BalanceMinor
			}
			if sum != 0 {
				t.Fatalf("seed %d: the ledger balances sum to %d", seed, sum)
			}
			name := nameOf
			for i, s := range v.SuggestedTransfers {
				if s.Amount <= 0 {
					t.Fatalf("seed %d: suggestion %d has amount %d", seed, i, s.Amount)
				}
				if r := e.settle(t, name[s.From], name[s.To], s.Amount); r.Status != http.StatusCreated && r.Status != http.StatusAccepted {
					t.Fatalf("seed %d: suggestion %d (%s pays %s %d) was refused: %d %s\nbalances %+v", seed, i, name[s.From], name[s.To], s.Amount, r.Status, r.Body, v)
				}
			}
			if again := e.balances(t, "owner"); len(again.SuggestedTransfers) != 0 {
				t.Fatalf("seed %d: after creating every suggestion, %+v is still suggested", seed, again.SuggestedTransfers)
			}
		})
	}
}
