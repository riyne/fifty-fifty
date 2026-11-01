package stressrig

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Env is what every part of the workload shares.
type Env struct {
	API    *API
	Client *Client
	Pool   *pgxpool.Pool
	Stats  *Stats
	Seed   uint64

	prefix string // makes expense descriptions unique to this run
	seq    atomic.Uint64

	mu      sync.Mutex
	creates []Create
	frozen  []Frozen
}

func (e *Env) record(c Create) {
	e.mu.Lock()
	e.creates = append(e.creates, c)
	e.mu.Unlock()
}

func (e *Env) recordFrozen(f Frozen) {
	e.mu.Lock()
	e.frozen = append(e.frozen, f)
	e.mu.Unlock()
}

func newRNG(seed uint64, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, stream))
}

// logical accumulates the responses to every attempt of one logical operation (all the
// requests that carried one idempotency key) into what the client was told.
type logical struct{ succ, refused, unknown int }

// add classifies one response. refusedCode is the error code of a 409 that is a definite
// "no": nothing was created and nothing will be. Any other non-success is unknown, which
// is conservative: the checker then accepts zero or one row.
func (l *logical) add(e *Env, op string, r Resp, refusedCode string) {
	switch {
	case r.Err != nil:
		l.unknown++
	case r.OK():
		l.succ++
	case r.Status >= 500:
		l.unknown++
	case r.Status == http.StatusConflict && refusedCode != "" && r.Code() == refusedCode:
		l.refused++
		e.Stats.Inc("refused/" + op)
	default:
		l.unknown++
		e.Stats.Unexpected("%s: status %d code %q body %s", op, r.Status, r.Code(), r.Body)
	}
}

func (l logical) rows() Rows {
	switch {
	case l.succ > 0:
		return One
	case l.unknown == 0 && l.refused > 0:
		return Zero
	}
	return ZeroOrOne
}

// send sends one logical operation the way a real client might: once; once and then
// again after the answer (a retry); two or three copies at the same moment (an impatient
// or duplicated client); or abandoned after a few milliseconds and then sent again (a
// timeout, whose request the server may still carry out). Every attempt uses the same
// Idempotency-Key, which the caller put in send. Each response is counted by status.
func (e *Env) send(ctx context.Context, rng *rand.Rand, op string, send func(context.Context) Resp) []Resp {
	var out []Resp
	note := func(r Resp) {
		out = append(out, r)
		e.count(op, r)
	}
	switch p := rng.Float64(); {
	case p < 0.55:
		e.Stats.Inc("pattern/" + op + "/single")
		note(send(ctx))
	case p < 0.70:
		e.Stats.Inc("pattern/" + op + "/retry")
		note(send(ctx))
		note(send(ctx))
	case p < 0.85:
		e.Stats.Inc("pattern/" + op + "/duplicate")
		n := 2 + rng.IntN(2)
		resps := make([]Resp, n)
		var wg sync.WaitGroup
		for i := range resps {
			wg.Add(1)
			go func() { defer wg.Done(); resps[i] = send(ctx) }()
		}
		wg.Wait()
		replayed := 0
		for _, r := range resps {
			note(r)
			if r.Replayed {
				replayed++
			}
		}
		if replayed > 0 {
			e.Stats.Inc("duplicate_waited_and_replayed/" + op)
		}
	default:
		e.Stats.Inc("pattern/" + op + "/abandon")
		short, cancel := context.WithTimeout(ctx, time.Duration(1+rng.IntN(5))*time.Millisecond)
		first := send(short)
		cancel()
		note(first)
		if first.Err != nil {
			e.Stats.Inc("abandoned/" + op)
		}
		note(send(ctx))
	}
	return out
}

// count tallies one response by status (or "no_response"), plus replays and server errors.
func (e *Env) count(op string, r Resp) {
	if r.Err != nil {
		e.Stats.Inc(op + "/no_response")
		return
	}
	e.Stats.Inc(fmt.Sprintf("%s/%d", op, r.Status))
	if r.Status >= 500 {
		e.Stats.Inc("http_5xx")
	}
	if r.Replayed {
		e.Stats.Inc("replayed/" + op)
	}
}

// exp is what the workload remembers about an expense. state is a hint (the workload
// learns it from responses and a race can make it stale), used only to choose targets.
type exp struct {
	id    string
	payer int
	items []string
	state atomic.Int32
}

const (
	stClaiming int32 = iota
	stFinalized
	stVoid
)

type group struct {
	id    uuid.UUID
	users []User // users[0] owns the group

	mu   sync.Mutex
	exps []*exp
}

func (g *group) add(x *exp) {
	g.mu.Lock()
	g.exps = append(g.exps, x)
	g.mu.Unlock()
}

// pick returns a random expense satisfying keep, or nil.
func (g *group) pick(rng *rand.Rand, keep func(*exp) bool) *exp {
	g.mu.Lock()
	defer g.mu.Unlock()
	var cand []*exp
	for _, x := range g.exps {
		if keep(x) {
			cand = append(cand, x)
		}
	}
	if len(cand) == 0 {
		return nil
	}
	return cand[rng.IntN(len(cand))]
}

func (e *Env) nextDesc() string { return fmt.Sprintf("st-%s-%d", e.prefix, e.seq.Add(1)) }

type expenseBody struct {
	Description string     `json:"description"`
	Items       []itemBody `json:"items"`
	TaxMinor    int64      `json:"tax_minor"`
	TipMinor    int64      `json:"tip_minor"`
}

type itemBody struct {
	Description string `json:"description"`
	PriceMinor  int64  `json:"price_minor"`
}

// createExpense creates one expense as users[payer], as one logical operation, and
// records it for the checker. It returns nil when no attempt got a success.
func (e *Env) createExpense(ctx context.Context, rng *rand.Rand, g *group, payer int, nItems int, prices func() int64, taxTip bool) *exp {
	body := expenseBody{Description: e.nextDesc()}
	for i := 0; i < nItems; i++ {
		body.Items = append(body.Items, itemBody{Description: fmt.Sprintf("item%d", i), PriceMinor: prices()})
	}
	if taxTip {
		body.TaxMinor, body.TipMinor = int64(rng.IntN(400)), int64(rng.IntN(400))
	}
	key := uuid.NewString()
	u := g.users[payer]
	path := fmt.Sprintf("/v1/groups/%s/expenses", g.id)
	resps := e.send(ctx, rng, "create", func(c context.Context) Resp { return e.Client.Do(c, u.Token, "POST", path, body, key) })

	var lg logical
	var made *exp
	for _, r := range resps {
		lg.add(e, "create", r, "")
		if r.OK() && made == nil {
			var out struct {
				ID    string `json:"id"`
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			if json.Unmarshal(r.Body, &out) != nil || out.ID == "" {
				e.Stats.Unexpected("create: unreadable success body %s", r.Body)
				continue
			}
			made = &exp{id: out.ID, payer: payer}
			for _, it := range out.Items {
				made.items = append(made.items, it.ID)
			}
		}
	}
	e.record(Create{Group: g.id, Description: body.Description, Rows: lg.rows()})
	if made != nil {
		g.add(made)
	}
	return made
}

func randPrice(rng *rand.Rand) func() int64 {
	return func() int64 { return int64(100 + rng.IntN(4900)) }
}

func claimPath(g *group, x *exp, item string) string {
	return fmt.Sprintf("/v1/groups/%s/expenses/%s/items/%s/claim", g.id, x.id, item)
}

// claim sends one claim or unclaim. Naturally idempotent, so no key and no pattern.
func (e *Env) claim(ctx context.Context, g *group, x *exp, who int, item string, unclaim bool) Resp {
	method, op := "PUT", "claim"
	if unclaim {
		method, op = "DELETE", "unclaim"
	}
	r := e.Client.Do(ctx, g.users[who].Token, method, claimPath(g, x, item), nil, "")
	switch {
	case r.Err != nil:
		e.Stats.Inc(op + "/no_response")
	case r.Status == http.StatusOK || r.Status == http.StatusNoContent:
		e.Stats.Inc(fmt.Sprintf("%s/%d", op, r.Status))
	case r.Status == http.StatusConflict && r.Code() == "expense_not_claimable":
		e.Stats.Inc(op + "/409_not_claimable")
	default:
		e.Stats.Inc(fmt.Sprintf("%s/%d", op, r.Status))
		e.Stats.Unexpected("%s: status %d body %s", op, r.Status, r.Body)
	}
	if r.Status >= 500 {
		e.Stats.Inc("http_5xx")
	}
	return r
}

// finalizeOnce sends one logical finalize (with the send patterns). split asks the
// server to give unclaimed items to those members.
func (e *Env) finalize(ctx context.Context, rng *rand.Rand, g *group, x *exp, split []uuid.UUID) []Resp {
	var body any
	if len(split) > 0 {
		body = map[string][]uuid.UUID{"split_rest_among": split}
	}
	key := uuid.NewString()
	u := g.users[x.payer]
	path := fmt.Sprintf("/v1/groups/%s/expenses/%s/finalize", g.id, x.id)
	resps := e.send(ctx, rng, "finalize", func(c context.Context) Resp { return e.Client.Do(c, u.Token, "POST", path, body, key) })
	e.learn(x, "finalize", resps)
	return resps
}

func (e *Env) void(ctx context.Context, rng *rand.Rand, g *group, x *exp, who int) []Resp {
	key := uuid.NewString()
	u := g.users[who]
	path := fmt.Sprintf("/v1/groups/%s/expenses/%s/void", g.id, x.id)
	resps := e.send(ctx, rng, "void", func(c context.Context) Resp { return e.Client.Do(c, u.Token, "POST", path, nil, key) })
	e.learn(x, "void", resps)
	return resps
}

// learn updates the state hint and checks every response is one the api documents for
// this operation: 200, or 409 with a known code.
func (e *Env) learn(x *exp, op string, resps []Resp) {
	for _, r := range resps {
		switch {
		case r.Err != nil || r.Status >= 500:
		case r.Status == http.StatusOK:
			if op == "finalize" {
				x.state.Store(stFinalized)
			} else {
				x.state.Store(stVoid)
			}
		case r.Status == http.StatusConflict && (r.Code() == "items_unclaimed" || r.Code() == "invalid_state"):
			e.Stats.Inc(op + "/409_" + r.Code())
			if r.Code() == "invalid_state" && x.state.Load() == stClaiming {
				x.state.Store(stFinalized) // somebody finalized or voided it
			}
		default:
			e.Stats.Unexpected("%s: status %d code %q body %s", op, r.Status, r.Code(), r.Body)
		}
	}
}

// userLoop is one user acting in an expense group until ctx ends.
func (e *Env) userLoop(ctx context.Context, g *group, me int, stream uint64) {
	rng := newRNG(e.Seed, stream)
	parent := context.WithoutCancel(ctx) // a request in flight when the run ends still finishes
	for ctx.Err() == nil {
		switch p := rng.Float64(); {
		case p < 0.18:
			e.createExpense(parent, rng, g, me, 2+rng.IntN(4), randPrice(rng), rng.IntN(2) == 0)
		case p < 0.65:
			x := g.pick(rng, func(x *exp) bool { return x.state.Load() == stClaiming })
			if rng.IntN(5) == 0 { // sometimes any expense, so claims on finalized ones are tried too
				x = g.pick(rng, func(*exp) bool { return true })
			}
			if x == nil || len(x.items) == 0 {
				e.createExpense(parent, rng, g, me, 2+rng.IntN(4), randPrice(rng), true)
				continue
			}
			e.claim(parent, g, x, me, x.items[rng.IntN(len(x.items))], rng.IntN(10) < 3)
		case p < 0.82:
			x := g.pick(rng, func(x *exp) bool { return x.payer == me && x.state.Load() == stClaiming })
			if x == nil {
				continue
			}
			var split []uuid.UUID
			if rng.IntN(100) < 40 {
				for _, u := range g.users {
					if rng.IntN(2) == 0 {
						split = append(split, u.ID)
					}
				}
			}
			e.finalize(parent, rng, g, x, split)
		case p < 0.90:
			x := g.pick(rng, func(x *exp) bool { return x.state.Load() == stFinalized && (x.payer == me || me == 0) })
			if x == nil {
				continue
			}
			e.void(parent, rng, g, x, me)
		default:
			r := e.Client.Do(parent, g.users[me].Token, "GET", fmt.Sprintf("/v1/groups/%s/balances", g.id), nil, "")
			e.Stats.Inc(fmt.Sprintf("balances/%d", r.Status))
			if r.Status != http.StatusOK {
				e.Stats.Unexpected("balances: status %d %s %v", r.Status, r.Body, r.Err)
			}
		}
	}
}

// fire starts every fn at (nearly) the same moment and waits for all of them.
func fire(fns ...func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; fn() }()
	}
	time.Sleep(2 * time.Millisecond) // let them all park on start
	close(start)
	wg.Wait()
}

// raceLoop runs rounds of the situation the locks exist for: an expense with some items
// claimed, then, released together, two finalizes, a void and several claims and
// unclaims by other members. Random user loops collide with a finalize only by luck;
// this makes it the common case. The round records whether the burst was split by the
// finalize (some claims accepted, some refused), which is the sign the two really
// overlapped in time.
func (e *Env) raceLoop(ctx context.Context, g *group, stream uint64) {
	rng := newRNG(e.Seed, stream)
	parent := context.WithoutCancel(ctx)
	for ctx.Err() == nil {
		payer := rng.IntN(len(g.users))
		x := e.createExpense(parent, rng, g, payer, 3+rng.IntN(3), randPrice(rng), true)
		if x == nil {
			continue
		}
		unclaimed := false
		for _, it := range x.items {
			if rng.IntN(100) < 70 {
				e.claim(parent, g, x, rng.IntN(len(g.users)), it, false)
			} else {
				unclaimed = true
			}
		}
		var split []uuid.UUID
		if unclaimed {
			for _, u := range g.users {
				split = append(split, u.ID)
			}
		}

		// Each goroutine of the burst gets its own generator: a rand.Rand is not safe for
		// concurrent use.
		s1, s2, s3 := rng.Uint64(), rng.Uint64(), rng.Uint64()
		var mu sync.Mutex
		var claimOK, claimRefused, finOK, finInvalid, voidOK int
		fns := []func(){
			func() {
				for _, r := range e.finalize(parent, newRNG(s1, 1), g, x, split) {
					mu.Lock()
					count(&finOK, &finInvalid, r)
					mu.Unlock()
				}
			},
			func() {
				for _, r := range e.finalize(parent, newRNG(s2, 2), g, x, split) {
					mu.Lock()
					count(&finOK, &finInvalid, r)
					mu.Unlock()
				}
			},
			func() {
				for _, r := range e.void(parent, newRNG(s3, 3), g, x, 0) {
					if r.Status == http.StatusOK {
						mu.Lock()
						voidOK++
						mu.Unlock()
					}
				}
			},
		}
		for i := 0; i < 4; i++ {
			who, item, un := rng.IntN(len(g.users)), x.items[rng.IntN(len(x.items))], rng.IntN(10) < 3
			fns = append(fns, func() {
				r := e.claim(parent, g, x, who, item, un)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Status == http.StatusOK || r.Status == http.StatusNoContent:
					claimOK++
				case r.Status == http.StatusConflict:
					claimRefused++
				}
			})
		}
		fire(fns...)

		e.Stats.Inc("race_rounds")
		if claimOK > 0 && claimRefused > 0 {
			e.Stats.Inc("race_mixed") // the finalize landed in the middle of the claims
		}
		if finOK > 0 && finInvalid > 0 {
			e.Stats.Inc("race_double_finalize_refused") // two finalizes, one won, one was refused
		}
		if voidOK > 0 {
			e.Stats.Inc("race_void_after_finalize")
		}
	}
}

func count(ok, invalid *int, r Resp) {
	switch {
	case r.Status == http.StatusOK:
		*ok++
	case r.Status == http.StatusConflict && r.Code() == "invalid_state":
		*invalid++
	}
}
