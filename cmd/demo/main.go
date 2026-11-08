// Command demo starts the whole system on a fresh, throwaway database and walks through
// the product from the outside, over HTTP, as three people: alice, bob and carol sign in
// (real Dex tokens), form a group, upload receipts, claim items, finalize, read balances
// and pay each other.
//
// Everything is local and synthetic. The language model is a MOCK (cmd/mockllm, simulated
// latency of 1500 ms) that answers from the synthetic receipts' ground truth, and the
// payment provider is a MOCK (cmd/mockpay, simulated latency of 300 ms, with duplicated
// and delayed webhooks): no real model is called and no money moves. Receipts are read by
// real Tesseract. The real api and worker binaries run, built from this tree.
//
// Usage: make demo        (the walkthrough, then everything is stopped and dropped)
//
//	make demo-keep   (the same, then leave the stack running for the iOS app)
//
// It needs `make up` (Postgres and Dex; `make demo` does that) and Tesseract. It is a
// development tool: it imports the test rigs and is not part of any shipped binary.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/settlerig"
)

const (
	webhookSecret = "demo-webhook-secret"
	receiptCount  = 12 // generated; the walkthrough uses the first 5, the rest are for the app
)

// apiAddr is where the api listens. The iOS app expects 127.0.0.1:8080 (the default).
var apiAddr string

func main() {
	keep := flag.Bool("keep", false, "after the walkthrough, leave the stack running (for the iOS app) until Ctrl-C")
	kill := flag.Bool("kill", true, "kill -9 a worker while it is reading receipts, to show another worker take the jobs over")
	flag.StringVar(&apiAddr, "api-addr", "127.0.0.1:8080", "where the api listens (the iOS app expects the default)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *keep, *kill); err != nil {
		fmt.Fprintln(os.Stderr, "\ndemo failed:", err)
		os.Exit(1)
	}
}

// cleanups run in reverse order when run returns, however it returns.
type cleanups []func()

func (c *cleanups) add(f func()) { *c = append(*c, f) }
func (c cleanups) run() {
	for i := len(c) - 1; i >= 0; i-- {
		c[i]()
	}
}

func say(format string, args ...any) { fmt.Printf(format+"\n", args...) }
func step(title string)              { fmt.Printf("\n== %s\n", title) }
func money(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

func run(ctx context.Context, keep, kill bool) (err error) {
	var cl cleanups
	defer func() { cl.run() }() // a closure: `defer cl.run()` would copy the empty list now

	ownerURL, appURL := os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		return errors.New("MIGRATE_DATABASE_URL and DATABASE_URL are required (run it through make: make demo)")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return errors.New("tesseract is not installed (brew install tesseract)")
	}
	issuer, clientID := envOr("OIDC_ISSUER", "http://localhost:5556/dex"), envOr("OIDC_CLIENT_ID", "fifty-fifty")
	if resp, err := http.Get(issuer + "/.well-known/openid-configuration"); err != nil {
		return fmt.Errorf("Dex is not answering at %s (make up): %w", issuer, err)
	} else {
		resp.Body.Close()
	}
	// Dial rather than listen: on macOS a listener on 127.0.0.1 can coexist with another
	// process listening on all addresses, and then which one a client reaches is ambiguous.
	_, port, _ := net.SplitHostPort(apiAddr)
	for _, a := range []string{"127.0.0.1:" + port, "[::1]:" + port} {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			return fmt.Errorf("something is already listening on %s (is `make run-api` running? stop it first)", a)
		}
	}
	root, err := settlerig.RepoRoot()
	if err != nil {
		return err
	}

	// ---- setup -------------------------------------------------------------------------
	step("Setup: build the binaries, make a fresh database, start the services")
	tmp, err := os.MkdirTemp("", "fifty-demo-")
	if err != nil {
		return err
	}
	cl.add(func() { os.RemoveAll(tmp) })
	bins := filepath.Join(tmp, "bin")
	for _, b := range []string{"api", "worker", "mockllm", "mockpay", "receiptgen"} {
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(bins, b), "./cmd/"+b)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %v\n%s", b, err, out)
		}
	}
	say("built api, worker, mockllm, mockpay and receiptgen from this tree")

	sample := filepath.Join(tmp, "receipts")
	if out, err := exec.CommandContext(ctx, filepath.Join(bins, "receiptgen"), "--out", sample, "--from", "1", "--count", fmt.Sprint(receiptCount)).CombinedOutput(); err != nil {
		return fmt.Errorf("receiptgen: %v\n%s", err, out)
	}
	say("generated %d synthetic receipts in %s", receiptCount, sample)

	db, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		return err
	}
	cl.add(func() {
		if err := db.Drop(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "drop", db.Name, "failed (drop it by hand):", err)
		}
	})
	say("fresh database %s, all 16 migrations applied (dropped when the demo ends)", db.Name)

	llm, llmURL, err := settlerig.StartMockllm(filepath.Join(bins, "mockllm"),
		"--latency-median", "1500ms", "--latency-sigma", "0", "--fixtures", filepath.Join(sample, "fixtures.json"))
	if err != nil {
		return err
	}
	cl.add(llm.Stop)
	pay, payURL, err := settlerig.StartMockpay(filepath.Join(bins, "mockpay"), webhookSecret,
		"--webhook-url", "http://"+apiAddr+"/v1/webhooks/payments", "--webhook-delay-max", "300ms", "--webhook-duplicate", "0.3")
	if err != nil {
		return err
	}
	cl.add(pay.Stop)

	store := filepath.Join(tmp, "store")
	api, err := settlerig.Start("api", filepath.Join(bins, "api"), nil, []string{
		"DATABASE_URL=" + db.AppURL, "SYSTEM_DATABASE_URL=" + db.SystemURL, "HTTP_ADDR=" + apiAddr,
		"OIDC_ISSUER=" + issuer, "OIDC_CLIENT_ID=" + clientID, "RECEIPT_DIR=" + store,
		"PAYMENTS_WEBHOOK_SECRET=" + webhookSecret,
	})
	if err != nil {
		return err
	}
	cl.add(api.Stop)
	if err := waitHTTP(ctx, "http://"+apiAddr+"/readyz", api); err != nil {
		return err
	}
	startWorker := func(name string) (*settlerig.Proc, error) {
		p, err := settlerig.StartWorker(settlerig.WorkerSpec{
			Bin: filepath.Join(bins, "worker"), DatabaseURL: db.WorkerURL, PaymentsURL: payURL, Name: name,
			Concurrency: 3, Lease: 3 * time.Second, Poll: 200 * time.Millisecond,
			// The key is cleared so a real one in this shell can never be used by the demo.
			Env: []string{"RECEIPT_DIR=" + store, "ANTHROPIC_BASE_URL=" + llmURL, "ANTHROPIC_API_KEY="},
		})
		if err == nil {
			cl.add(p.Stop)
		}
		return p, err
	}
	w1, err := startWorker("worker-1")
	if err != nil {
		return err
	}
	say("running: api %s, worker-1, Tesseract on the host", apiAddr)
	say("         MOCK LLM %s (simulated latency of 1500 ms), MOCK payment provider %s (simulated latency of 300 ms)", llmURL, payURL)

	d := &demo{ctx: ctx, base: "http://" + apiAddr, root: root}

	// ---- act 1: people and a group ------------------------------------------------------
	step("1. Three people, one group (real OIDC tokens from Dex, join code redeemed over HTTP)")
	people := map[string]*person{}
	for _, n := range []string{"alice", "bob", "carol"} {
		p, err := d.login(n)
		if err != nil {
			return err
		}
		people[n] = p
		say("%-6s signed in, user id %s", p.Name, p.ID)
	}
	alice, bob, carol := people["alice"], people["bob"], people["carol"]
	var group struct {
		ID string `json:"id"`
	}
	if err := d.do(alice, "POST", "/v1/groups", map[string]any{"name": "Demo dinner", "currency": "CAD"}, "", &group); err != nil {
		return err
	}
	var code struct {
		Code string `json:"code"`
	}
	if err := d.do(alice, "POST", "/v1/groups/"+group.ID+"/join-codes", map[string]any{}, "", &code); err != nil {
		return err
	}
	say("alice created the group and a join code (shown once; only a hash is stored): %s", code.Code)
	for _, p := range []*person{bob, carol} {
		if err := d.do(p, "POST", "/v1/join", map[string]any{"code": code.Code}, uuid.NewString(), nil); err != nil {
			return err
		}
		say("%s joined", p.Name)
	}
	byID := map[string]*person{alice.ID: alice, bob.ID: bob, carol.ID: carol}

	// ---- act 2: receipts, with a crash --------------------------------------------------
	step("2. Receipts: upload, OCR, redaction, mock LLM, validation (a worker is killed on purpose)")
	uploads := []struct {
		by  *person
		ref int
	}{{alice, 1}, {alice, 2}, {alice, 3}, {bob, 4}, {bob, 5}}
	var expenseIDs []string
	refOf := map[string]int{}
	for _, u := range uploads {
		png, err := os.ReadFile(filepath.Join(sample, fmt.Sprintf("%09d.png", u.ref)))
		if err != nil {
			return err
		}
		id, err := d.upload(u.by, group.ID, png)
		if err != nil {
			return err
		}
		expenseIDs = append(expenseIDs, id)
		refOf[id] = u.ref
	}
	say("5 receipts uploaded (3 by alice, 2 by bob); each is an expense in status `parsing` with a receipt.parse job queued in the same transaction")
	if kill {
		if err := waitFor(ctx, 60*time.Second, func() bool { return mockRequests(llmURL) >= 1 }); err != nil {
			return fmt.Errorf("the worker never called the mock LLM: %w\n%s\n%s", err, dumpJobs(ctx, db), w1.Log())
		}
		if err := w1.Kill(); err != nil {
			return err
		}
		say("worker-1 killed with SIGKILL while its jobs were waiting on the model (no chance to clean up)")
		if _, err := startWorker("worker-2"); err != nil {
			return err
		}
		say("worker-2 started; it takes the jobs over when worker-1's leases (3 s) expire")
	}
	exps := map[string]*expense{}
	for _, id := range expenseIDs {
		e, err := d.waitParsed(group.ID, id, alice)
		if err != nil {
			return err
		}
		exps[id] = e
		match, err := matchesTruth(sample, refOf[id], e)
		if err != nil {
			return err
		}
		say("expense %s  %-12s %2d items  total %s CAD  (ground truth of receipt %d: %s)", id[:8], e.Status, len(e.Items), money(e.TotalMinor), refOf[id], match)
	}
	var jobs, attempts int
	if err := db.Owner.QueryRow(ctx, `SELECT count(*), COALESCE(sum(attempts),0) FROM jobs WHERE kind = 'receipt.parse'`).Scan(&jobs, &attempts); err != nil {
		return err
	}
	say("database: %d receipt.parse jobs took %d attempts in total; the extra attempts are the killed worker's jobs run again (the model call is at-least-once; the items match the ground truth above, so they were written once)", jobs, attempts)

	// ---- act 3: claim and finalize ------------------------------------------------------
	step("3. Claim items, finalize (idempotent), the ledger")
	members := []*person{alice, bob, carol}
	finalized := 0
	// The first receipt (alice paid) and the fourth (bob paid), so the payers differ.
	for _, pick := range []struct {
		idx   int
		payer *person
	}{{0, alice}, {3, bob}} {
		id, payer := expenseIDs[pick.idx], pick.payer
		e := exps[id]
		if e.Status != "claiming" {
			say("expense %s is `%s`, not claimable: skipped", id[:8], e.Status)
			continue
		}
		for i, it := range e.Items {
			if i == len(e.Items)-1 && len(e.Items) > 1 {
				break // leave the last item unclaimed to show "split the rest"
			}
			if err := d.do(members[i%3], "PUT", fmt.Sprintf("/v1/groups/%s/expenses/%s/items/%s/claim", group.ID, id, it.ID), nil, "", nil); err != nil {
				return err
			}
		}
		key := uuid.NewString()
		var fin struct {
			LedgerTx string `json:"ledger_transaction_id"`
			Shares   []struct {
				MemberID string `json:"member_id"`
				Total    int64  `json:"total_minor"`
			} `json:"shares"`
		}
		body := map[string]any{"split_rest_among": []string{alice.ID, bob.ID, carol.ID}}
		if err := d.do(payer, "POST", fmt.Sprintf("/v1/groups/%s/expenses/%s/finalize", group.ID, id), body, key, &fin); err != nil {
			return err
		}
		say("%s finalized %q (total %s): ledger transaction %s", payer.Name, e.Description, money(e.TotalMinor), fin.LedgerTx[:8])
		for _, s := range fin.Shares {
			say("    %-6s owes %s", byID[s.MemberID].Name, money(s.Total))
		}
		var again struct {
			LedgerTx string `json:"ledger_transaction_id"`
		}
		replayed, err := d.doReplay(payer, "POST", fmt.Sprintf("/v1/groups/%s/expenses/%s/finalize", group.ID, id), body, key, &again)
		if err != nil {
			return err
		}
		say("    the same request sent again with the same Idempotency-Key: replayed=%v, same ledger transaction=%v", replayed, again.LedgerTx == fin.LedgerTx)
		finalized++
	}
	if finalized == 0 {
		return errors.New("no expense reached `claiming`, so there is nothing to finalize (see the worker logs)")
	}
	bal, err := d.balances(alice, group.ID)
	if err != nil {
		return err
	}
	d.printBalances("balances after the finalizes", bal, byID)

	// ---- act 4: settle ------------------------------------------------------------------
	step("4. Settle up through the MOCK payment provider (simulated; webhooks duplicated and delayed)")
	if len(bal.Transfers) == 0 {
		return errors.New("no suggested transfer; nothing to settle")
	}
	t := bal.Transfers[0]
	from, to := byID[t.From], byID[t.To]
	var st struct {
		ID string `json:"id"`
	}
	if err := d.do(from, "POST", "/v1/groups/"+group.ID+"/settlements", map[string]any{"to_member_id": t.To, "amount_minor": t.Amount}, uuid.NewString(), &st); err != nil {
		return err
	}
	say("%s pays %s %s CAD: settlement %s created `pending`, a settlement.execute job queued in the same transaction", from.Name, to.Name, money(t.Amount), st.ID[:8])
	var final struct {
		Status     string  `json:"status"`
		LedgerTx   *string `json:"ledger_transaction_id"`
		FailureCod *string `json:"failure_code"`
	}
	if err := waitFor(ctx, 60*time.Second, func() bool {
		if d.do(from, "GET", "/v1/groups/"+group.ID+"/settlements/"+st.ID, nil, "", &final) != nil {
			return false
		}
		return final.Status == "succeeded" || final.Status == "failed"
	}); err != nil {
		return fmt.Errorf("settlement did not reach a final state (status %q): %w", final.Status, err)
	}
	say("settlement is `%s`", final.Status)
	bal, err = d.balances(alice, group.ID)
	if err != nil {
		return err
	}
	d.printBalances("balances after the payment", bal, byID)

	// ---- act 5: verify from the database and the provider -------------------------------
	step("5. Checks (recomputed from the tables and the provider's own record, not from API answers)")
	var ledgerSum int64
	if err := db.Owner.QueryRow(ctx, `SELECT COALESCE(SUM(amount_minor),0) FROM ledger_entries`).Scan(&ledgerSum); err != nil {
		return err
	}
	say("sum of every ledger entry: %d (must be 0)", ledgerSum)
	rows, err := db.Owner.Query(ctx, `SELECT id FROM settlements`)
	if err != nil {
		return err
	}
	var sids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		sids = append(sids, id)
	}
	rows.Close()
	prov, err := settlerig.FetchProvider(ctx, payURL)
	if err != nil {
		return err
	}
	rep, err := settlerig.Verify(ctx, db.Owner, prov, sids)
	if err != nil {
		return err
	}
	say("settlements %d, by status %v: ledger postings %d, provider charges %d, anomalies %d", rep.Settlements, rep.ByStatus, rep.Postings, rep.ProviderCharges, len(rep.Anomalies))
	if ledgerSum != 0 || len(rep.Anomalies) != 0 {
		return fmt.Errorf("a check failed: ledger sum %d, anomalies %v", ledgerSum, rep.Anomalies)
	}
	say("\nAll checks passed.")

	if !keep {
		return nil
	}
	step("Left running for the iOS app (Ctrl-C stops everything and drops the database)")
	say("api:       http://%s   (sign in as alice, bob or carol; password \"password\")", apiAddr)
	say("receipts:  %s", sample)
	say("           xcrun simctl addmedia booted %s/00000000{6,7,8}.png   (receipts 6 to 12 are not used by the walkthrough; only receipts from receiptgen parse into items)", sample)
	say("database:  %s", db.Name)
	<-ctx.Done()
	return nil
}

// ---- helpers ------------------------------------------------------------------------------

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func waitFor(ctx context.Context, timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out after %s", timeout)
}

func waitHTTP(ctx context.Context, url string, p *settlerig.Proc) error {
	err := waitFor(ctx, 20*time.Second, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if err != nil {
		return fmt.Errorf("%s did not become ready: %w\n%s", p.Name, err, p.Log())
	}
	return nil
}

func mockRequests(base string) int {
	resp, err := http.Get(base + "/stats")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var s struct {
		Requests int `json:"requests"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s.Requests
}

type person struct{ Name, ID, Token string }

type expense struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	TotalMinor  int64  `json:"total_minor"`
	Items       []struct {
		ID string `json:"id"`
	} `json:"items"`
}

type balances struct {
	Balances []struct {
		MemberID string `json:"member_id"`
		Balance  int64  `json:"balance_minor"`
	} `json:"balances"`
	Transfers []struct {
		From   string `json:"from_member_id"`
		To     string `json:"to_member_id"`
		Amount int64  `json:"amount_minor"`
	} `json:"suggested_transfers"`
}

type demo struct {
	ctx        context.Context
	base, root string
}

// login gets a real ID token from Dex (scripts/dev-token.sh does the Auth Code + PKCE
// flow) and lets the api provision the user on the first request.
func (d *demo) login(name string) (*person, error) {
	cmd := exec.CommandContext(d.ctx, filepath.Join(d.root, "scripts", "dev-token.sh"), name)
	cmd.Dir = d.root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("token for %s: %w", name, err)
	}
	p := &person{Name: name, Token: strings.TrimSpace(string(out))}
	var me struct {
		ID string `json:"id"`
	}
	if err := d.do(p, "GET", "/v1/me", nil, "", &me); err != nil {
		return nil, err
	}
	p.ID = me.ID
	return p, nil
}

// do sends one request as p and decodes a 2xx answer into out (if not nil). A non-2xx
// answer is an error that carries the body.
func (d *demo) do(p *person, method, path string, body any, key string, out any) error {
	_, err := d.doReplay(p, method, path, body, key, out)
	return err
}

// doReplay is do that also reports whether the answer was a replay of a stored response.
func (d *demo) doReplay(p *person, method, path string, body any, key string, out any) (bool, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(d.ctx, method, d.base+path, rd)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("%s %s as %s: %d %s", method, path, p.Name, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return false, fmt.Errorf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return resp.Header.Get("Idempotent-Replayed") == "true", nil
}

func (d *demo) upload(p *person, group string, png []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile("file", "receipt.png")
	w.Write(png)
	mw.Close()
	req, err := http.NewRequestWithContext(d.ctx, "POST", d.base+"/v1/groups/"+group+"/receipts", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("upload as %s: %d %s", p.Name, resp.StatusCode, b)
	}
	var u struct {
		ExpenseID string `json:"expense_id"`
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return "", err
	}
	return u.ExpenseID, nil
}

// waitParsed polls until the expense has left `parsing`.
func (d *demo) waitParsed(group, id string, p *person) (*expense, error) {
	var e expense
	err := waitFor(d.ctx, 120*time.Second, func() bool {
		if d.do(p, "GET", "/v1/groups/"+group+"/expenses/"+id, nil, "", &e) != nil {
			return false
		}
		return e.Status != "parsing"
	})
	if err != nil {
		return nil, fmt.Errorf("expense %s stayed in status %q: %w", id, e.Status, err)
	}
	return &e, nil
}

func (d *demo) balances(p *person, group string) (*balances, error) {
	var b balances
	if err := d.do(p, "GET", "/v1/groups/"+group+"/balances", nil, "", &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (d *demo) printBalances(title string, b *balances, byID map[string]*person) {
	var sum int64
	say("%s:", title)
	for _, m := range b.Balances {
		sum += m.Balance
		say("    %-6s %9s", byID[m.MemberID].Name, money(m.Balance))
	}
	say("    sum of balances: %d (always 0)", sum)
	for _, t := range b.Transfers {
		say("    suggested: %s pays %s %s", byID[t.From].Name, byID[t.To].Name, money(t.Amount))
	}
}

// dumpJobs describes the receipt.parse jobs, for a failure message.
func dumpJobs(ctx context.Context, db *settlerig.Scratch) string {
	rows, err := db.Owner.Query(ctx, `SELECT state, attempts, run_at <= now(), COALESCE(last_error, '') FROM jobs WHERE kind = 'receipt.parse'`)
	if err != nil {
		return "jobs: " + err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	b.WriteString("receipt.parse jobs:\n")
	for rows.Next() {
		var state, lastErr string
		var attempts int
		var due bool
		if rows.Scan(&state, &attempts, &due, &lastErr) == nil {
			fmt.Fprintf(&b, "  state=%s attempts=%d due=%v last_error=%q\n", state, attempts, due, lastErr)
		}
	}
	return b.String()
}

// matchesTruth compares a parsed expense with the generator's ground truth for the
// receipt: the same total and one item per unit (quantity lines are expanded to units).
// A receipt that went to review is reported, not treated as a failure.
func matchesTruth(dir string, ref int, e *expense) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%09d.json", ref)))
	if err != nil {
		return "", err
	}
	var truth struct {
		Items []struct {
			Quantity int `json:"quantity"`
		} `json:"items"`
		Total int64 `json:"total_minor"`
	}
	if err := json.Unmarshal(b, &truth); err != nil {
		return "", err
	}
	units := 0
	for _, it := range truth.Items {
		units += max(it.Quantity, 1)
	}
	if e.Status != "claiming" {
		return "not compared: the receipt went to review", nil
	}
	if e.TotalMinor != truth.Total || len(e.Items) != units {
		return "", fmt.Errorf("receipt %d: parsed %d items totalling %d, ground truth is %d units totalling %d", ref, len(e.Items), e.TotalMinor, units, truth.Total)
	}
	return fmt.Sprintf("same total and %d units, each once", units), nil
}
