package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

// TestReceiptParse_RealProcessesAndARealKill reads receipts end to end with real
// processes: the real worker binary with real Tesseract, the real mock LLM binary (with
// its fixtures from the synthetic receipts' ground truth), and the real API handlers
// in-process, over a scratch database. The first worker is killed with SIGKILL while the
// mock LLM is holding its requests; a second worker finishes the jobs.
//
// What the mock does: it answers a prompt containing "REF n" with that receipt's ground
// truth, so this test shows that the real OCR read the REF line and the rest of the
// pipeline carried the right items into the database, once. It says nothing about how a
// real model behaves. mock LLM with simulated latency of 1500 ms.
func TestReceiptParse_RealProcessesAndARealKill(t *testing.T) {
	if !enabled {
		t.Skip("set E2E=1 (make test-e2e)")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		if os.Getenv("REQUIRE_TESSERACT") == "1" {
			t.Fatal("REQUIRE_TESSERACT=1 but tesseract is not installed")
		}
		t.Skip("tesseract is not installed")
	}
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	db, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Drop(context.Background()); err != nil {
			t.Errorf("drop %s: %v", db.Name, err)
		}
	})
	stack := teststack.NewOn(t, db.Pool, teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: 1000}))

	const n = 6
	fixtures := filepath.Join(t.TempDir(), "fixtures.json")
	fx, _ := json.Marshal(synth.Fixtures(1, n))
	if err := os.WriteFile(fixtures, fx, 0o600); err != nil {
		t.Fatal(err)
	}
	mock, mockURL, err := settlerig.StartMockllm(bins.Mockllm, "--latency-median", "1500ms", "--latency-sigma", "0", "--fixtures", fixtures)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mock.Stop()
		if t.Failed() {
			t.Logf("mockllm log tail:\n%s", tail(mock.Log(), 1500))
		}
	})
	var workers []*settlerig.Proc
	t.Cleanup(func() {
		for _, p := range workers {
			p.Stop()
		}
		if t.Failed() {
			for _, p := range workers {
				t.Logf("%s log tail:\n%s", p.Name, tail(p.Log(), 2500))
			}
		}
	})
	startWorker := func(name string) *settlerig.Proc {
		p, err := settlerig.StartWorker(settlerig.WorkerSpec{
			Bin: bins.Worker, DatabaseURL: db.AppURL, Name: name, Concurrency: 3, Lease: time.Second, Poll: 100 * time.Millisecond,
			Env: []string{"RECEIPT_DIR=" + stack.ReceiptDir, "ANTHROPIC_BASE_URL=" + mockURL},
		})
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, p)
		return p
	}
	first := startWorker("worker-1")

	token := stack.Token(t, "payer")
	group := stack.CreateGroup(t, token, "G")
	type up struct{ Expense, Receipt uuid.UUID }
	ups := map[int]up{}
	for ref := 1; ref <= n; ref++ {
		img, err := synth.Generate(ref).Render(synth.RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		w, _ := mw.CreateFormFile("file", "receipt.png")
		w.Write(img)
		mw.Close()
		r := stack.Do(t, token, "POST", "/v1/groups/"+group+"/receipts", buf.String(), "Content-Type", mw.FormDataContentType(), "Idempotency-Key", uuid.NewString())
		if r.Status != http.StatusAccepted {
			t.Fatalf("upload %d = %d: %s", ref, r.Status, r.Body)
		}
		var u struct {
			ExpenseID uuid.UUID `json:"expense_id"`
			ReceiptID uuid.UUID `json:"receipt_id"`
		}
		r.JSON(t, &u)
		ups[ref] = up{u.ExpenseID, u.ReceiptID}
	}

	// Wait until the first worker has OCR'd and is waiting on the model, then kill it.
	waitFor(t, 60*time.Second, "the worker to be waiting on the model", func() bool { return llmRequests(t, mockURL) >= 1 })
	var running int
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = 'receipt.parse' AND state = 'running'`).Scan(&running)
	t.Logf("killing %s: %d receipt.parse jobs running, %d model requests so far", first.Name, running, llmRequests(t, mockURL))
	if err := first.Kill(); err != nil {
		t.Fatal(err)
	}
	startWorker("worker-2")

	waitFor(t, 120*time.Second, "every parse job to finish", func() bool {
		return countWhere(t, db, `SELECT count(*) FROM jobs WHERE kind = 'receipt.parse' AND state = 'succeeded'`) == n
	})

	var planted []string
	for ref, u := range ups {
		truth := synth.Generate(ref)
		units := 0
		for _, l := range truth.Lines {
			units += l.Quantity
		}
		var status string
		var version, items int
		var sum, tax, tip int64
		if err := db.Pool.QueryRow(ctx, `SELECT e.status, e.version, e.tax_minor, e.tip_minor,
			(SELECT count(*) FROM expense_items i WHERE i.expense_id = e.id),
			(SELECT coalesce(sum(price_minor), 0) FROM expense_items i WHERE i.expense_id = e.id)
			FROM expenses e WHERE e.id = $1`, u.Expense).Scan(&status, &version, &tax, &tip, &items, &sum); err != nil {
			t.Fatal(err)
		}
		var ocrChars *int
		var outcome string
		_ = db.Pool.QueryRow(ctx, `SELECT ocr_chars, parse_outcome FROM receipts WHERE id = $1`, u.Receipt).Scan(&ocrChars, &outcome)
		audits := countWhere(t, db, `SELECT count(*) FROM audit_events WHERE kind = 'receipt.parsed' AND data->>'expense_id' = $1`, u.Expense.String())
		if status != "claiming" || version != 2 || items != units || sum+tax+tip != truth.TotalMinor || audits != 1 || ocrChars == nil || *ocrChars < 100 {
			t.Errorf("ref %d: status %s version %d, %d items (want %d) totalling %d (want %d), %d audit events, ocr_chars %v, outcome %s",
				ref, status, version, items, units, sum+tax+tip, truth.TotalMinor, audits, ocrChars, outcome)
		}
		for _, p := range truth.Planted {
			planted = append(planted, p.Value)
		}
	}
	// Nothing about the receipts' content reaches the workers' logs.
	for _, w := range workers {
		for _, v := range planted {
			if strings.Contains(w.Log(), v) {
				t.Errorf("%s logged a planted value %q", w.Name, v)
			}
		}
	}
	attempts := countWhere(t, db, `SELECT coalesce(sum(attempts), 0) FROM jobs WHERE kind = 'receipt.parse'`)
	t.Logf("jobs ran %d attempts for %d receipts; the model was asked %d times", attempts, n, llmRequests(t, mockURL))
	if attempts <= n {
		t.Errorf("%d attempts for %d jobs: the kill left no job to take over, so it proved nothing", attempts, n)
	}
}

func llmRequests(t *testing.T, base string) int {
	t.Helper()
	resp, err := http.Get(base + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s struct {
		Requests int `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.Requests
}

func countWhere(t *testing.T, db *settlerig.Scratch, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
