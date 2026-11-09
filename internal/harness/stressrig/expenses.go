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
	// version is the workload's belief about the expense's version: 1 at creation, raised
	// by every accepted edit it hears about. A hint, like state: a request abandoned by
	// the client can still be applied later, which makes it stale and an edit then gets
	// the documented 409.
	version atomic.Int32
}

// seenVersion raises the version hint to v if v is higher.
func (x *exp) seenVersion(v int32) {
	for {
		cur := x.version.Load()
		if v <= cur || x.version.CompareAndSwap(cur, v) {
			return
		}
	}
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
			made.version.Store(1)
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

type editUpdate struct {
	ItemID     string `json:"item_id"`
	PriceMinor int64  `json:"price_minor"`
}

type editBody struct {
	Version int32        `json:"version"`
	Update  []editUpdate `json:"update"`
}

// edit sends one logical edit as the payer: change the price of one item, made from the
// given version. Item ids never change in this workload (edits only reprice), so the
// workload's list of items stays valid.
func (e *Env) edit(ctx context.Context, rng *rand.Rand, g *group, x *exp, version int32, item string) []Resp {
	body := editBody{Version: version, Update: []editUpdate{{ItemID: item, PriceMinor: int64(100 + rng.IntN(4900))}}}
	key := uuid.NewString()
	u := g.users[x.payer]
	path := fmt.Sprintf("/v1/groups/%s/expenses/%s", g.id, x.id)
	resps := e.send(ctx, rng, "edit", func(c context.Context) Resp { return e.Client.Do(c, u.Token, "PATCH", path, body, key) })
	e.learnEdit(x, resps)
	return resps
}

// learnEdit raises the version hint from accepted edits, and checks every response is
// one the api documents for an edit: 200, or 409 version_conflict / invalid_state.
func (e *Env) learnEdit(x *exp, resps []Resp) {
	for _, r := range resps {
		switch {
		case r.Err != nil || r.Status >= 500:
		case r.Status == http.StatusOK:
			var out struct {
				Version int32 `json:"version"`
			}
			if json.Unmarshal(r.Body, &out) != nil || out.Version < 2 {
				e.Stats.Unexpected("edit: unreadable success body %s", r.Body)
				continue
			}
			x.seenVersion(out.Version)
		case r.Status == http.StatusConflict && (r.Code() == "version_conflict" || r.Code() == "invalid_state"):
			e.Stats.Inc("edit/409_" + r.Code())
			if r.Code() == "invalid_state" && x.state.Load() == stClaiming {
				x.state.Store(stFinalized) // somebody finalized or voided it
			}
		default:
			e.Stats.Unexpected("edit: status %d code %q body %s", r.Status, r.Code(), r.Body)
		}
	}
}

// refreshVersion reads the expense and takes its version, as a client does after a conflict.
func (e *Env) refreshVersion(ctx context.Context, g *group, x *exp, who int) {
	r := e.Client.Do(ctx, g.users[who].Token, "GET", fmt.Sprintf("/v1/groups/%s/expenses/%s", g.id, x.id), nil, "")
	var out struct {
		Version int32 `json:"version"`
	}
	if r.OK() && json.Unmarshal(r.Body, &out) == nil {
		x.seenVersion(out.Version)
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
		case p < 0.59:
			x := g.pick(rng, func(x *exp) bool { return x.state.Load() == stClaiming })
			if rng.IntN(5) == 0 { // sometimes any expense, so claims on finalized ones are tried too
				x = g.pick(rng, func(*exp) bool { return true })
			}
			if x == nil || len(x.items) == 0 {
				e.createExpense(parent, rng, g, me, 2+rng.IntN(4), randPrice(rng), true)
				continue
			}
			e.claim(parent, g, x, me, x.items[rng.IntN(len(x.items))], rng.IntN(10) < 3)
		case p < 0.65:
			x := g.pick(rng, func(x *exp) bool { return x.payer == me && x.state.Load() == stClaiming })
			if x == nil || len(x.items) == 0 {
				continue
			}
			resps := e.edit(parent, rng, g, x, x.version.Load(), x.items[rng.IntN(len(x.items))])
			for _, r := range resps {
				if r.Status == http.StatusConflict && r.Code() == "version_conflict" {
					e.refreshVersion(parent, g, x, me)
					break
				}
			}
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
// claimed, then, released together, two finalizes, two voids and several claims and
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
		e.editRace(parent, rng, g, x)

		var split []uuid.UUID
		if unclaimed {
			for _, u := range g.users {
				split = append(split, u.ID)
			}
		}

		// Each goroutine of the burst gets its own generator: a rand.Rand is not safe for
		// concurrent use.
		s1, s2, s3, s4, s5 := rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()
		editItem, editVersion := x.items[rng.IntN(len(x.items))], x.version.Load()
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
				for _, r := range e.void(parent, newRNG(s3, 3), g, x, 0) { // the group owner
					if r.Status == http.StatusOK {
						mu.Lock()
						voidOK++
						mu.Unlock()
					}
				}
			},
			func() {
				for _, r := range e.void(parent, newRNG(s4, 4), g, x, x.payer) { // and the payer, with another key
					if r.Status == http.StatusOK {
						mu.Lock()
						voidOK++
						mu.Unlock()
					}
				}
			},
		}
		fns = append(fns, func() { // an edit racing the finalizes, the voids and the claims
			e.edit(parent, newRNG(s5, 5), g, x, editVersion, editItem)
		})
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

// editRace is the situation the version check exists for: the payer's two edits, made
// from the same version with different keys (two tabs, or a retry that is not a retry),
// released together with a couple of claims. With the check exactly one is applied and
// the other gets 409 version_conflict; without it both are applied, the second on top of
// a version it never saw.
func (e *Env) editRace(ctx context.Context, rng *rand.Rand, g *group, x *exp) {
	version := x.version.Load()
	s1, s2 := rng.Uint64(), rng.Uint64()
	i1 := rng.IntN(len(x.items))
	i2 := (i1 + 1 + rng.IntN(len(x.items)-1)) % len(x.items)
	var r1, r2 []Resp
	fns := []func(){
		func() { r1 = e.edit(ctx, newRNG(s1, 1), g, x, version, x.items[i1]) },
		func() { r2 = e.edit(ctx, newRNG(s2, 2), g, x, version, x.items[i2]) },
	}
	for i := 0; i < 2; i++ {
		who, item := rng.IntN(len(g.users)), x.items[rng.IntN(len(x.items))]
		fns = append(fns, func() { e.claim(ctx, g, x, who, item, false) })
	}
	fire(fns...)

	applied := func(rs []Resp) bool {
		for _, r := range rs {
			if r.Status == http.StatusOK {
				return true
			}
		}
		return false
	}
	refused := func(rs []Resp) bool {
		for _, r := range rs {
			if r.Status == http.StatusConflict && r.Code() == "version_conflict" {
				return true
			}
		}
		return false
	}
	e.Stats.Inc("race_edit_rounds")
	switch {
	case applied(r1) && applied(r2):
		e.Stats.Inc("race_edit_both_applied")
	case applied(r1) && refused(r2), applied(r2) && refused(r1):
		e.Stats.Inc("race_edit_conflict") // one applied, the other refused
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
