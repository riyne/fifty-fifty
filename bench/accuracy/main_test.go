package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/anthropic"
	"github.com/riyne/fifty-fifty/internal/mockllm"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

var groundTruthOnly = []variant{allVariants[0]}

// fakeModel answers from the ground truth of the REF line it is shown, after a test has
// damaged the answer. It also checks what the harness sent: planted personal data must
// already be redacted.
type fakeModel struct {
	t      *testing.T
	damage func(a *ans, ref int)
	raw    func(ref int) string // if set, the answer text instead
	mu     sync.Mutex
	calls  int
}

var refRE = regexp.MustCompile(`REF (\d+)`)

func (f *fakeModel) Extract(_ context.Context, text string) (receiptparse.Extraction, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	m := refRE.FindStringSubmatch(text)
	if m == nil {
		f.t.Errorf("no REF line in the text sent:\n%s", text)
		return receiptparse.Extraction{}, errors.New("no REF")
	}
	ref, _ := strconv.Atoi(m[1])
	truth := synth.Generate(ref)
	for _, p := range truth.Planted {
		if strings.Contains(text, p.Value) {
			f.t.Errorf("ref %d: planted %s %q reached the model unredacted", ref, p.Kind, p.Value)
		}
	}
	if f.raw != nil {
		return receiptparse.Extraction{Answer: []byte(f.raw(ref)), Model: "fake", InputTokens: 100, OutputTokens: 50}, nil
	}
	a := truthAnswer(truth)
	if f.damage != nil {
		f.damage(&a, ref)
	}
	b, _ := json.Marshal(a)
	return receiptparse.Extraction{Answer: b, Model: "fake", InputTokens: 100, OutputTokens: 50}, nil
}

func params5() evalParams {
	return evalParams{First: 5001, N: 5, Variants: groundTruthOnly, Workers: 2, Attempts: 3, Wait: func(context.Context, time.Duration) {}}
}

func TestEvaluate_PerfectModelIsAllAcceptedAndCorrect(t *testing.T) {
	recs, err := evaluate(context.Background(), params5(), &fakeModel{t: t}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl := summarize(recs)
	if tl.Calls != 5 || tl.AcceptedCorrect != 5 || tl.AmountsExact.K != 5 || tl.Exact.K != 5 || tl.AcceptedWrong+tl.FlaggedWrong+tl.FlaggedCorrect != 0 {
		t.Fatalf("%+v", tl)
	}
	for i, r := range recs {
		if r.Ref != 5001+i || r.Variant != "ground-truth-text" || r.Answer != "" || r.InputText != "" {
			t.Errorf("record %d: %+v", i, r)
		}
		if r.Redactions.Total() == 0 || r.OCRChars == 0 {
			t.Errorf("record %d: redaction counts missing: %+v", i, r)
		}
	}
}

// These three show the harness can see the failures it exists to measure: if it counted
// every damaged answer as fine, a clean run would mean nothing.
func TestEvaluate_ASilentErrorIsCountedAsOne(t *testing.T) {
	// A price and the printed total misread together: valid, adds up, accepted, wrong.
	m := &fakeModel{t: t, damage: func(a *ans, _ int) { a.Items[0].LineTotal += 100; a.Total += 100 }}
	recs, err := evaluate(context.Background(), params5(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl := summarize(recs)
	if tl.AcceptedWrong != 5 || tl.AcceptedCorrect != 0 || tl.FlaggedWrong != 0 || tl.CatchRate.K != 0 || tl.CatchRate.N != 5 || tl.SilentErrorRate.K != 5 {
		t.Fatalf("%+v", tl)
	}
	if recs[0].Answer == "" || recs[0].InputText == "" {
		t.Error("a wrong answer and the text that produced it must be kept for study")
	}
}

func TestEvaluate_ACaughtErrorIsCountedAsCaught(t *testing.T) {
	// A price misread alone: the arithmetic no longer holds, so the flag fires.
	m := &fakeModel{t: t, damage: func(a *ans, _ int) { a.Items[0].LineTotal += 100 }}
	recs, _ := evaluate(context.Background(), params5(), m, nil)
	tl := summarize(recs)
	if tl.FlaggedWrong != 5 || tl.AcceptedWrong != 0 || tl.CatchRate.K != 5 || tl.CatchRate.N != 5 || tl.Reasons[receipts.ReasonMismatch] != 5 {
		t.Fatalf("%+v", tl)
	}
}

func TestEvaluate_AFalseAlarmAndUnusableAnswers(t *testing.T) {
	// Only the printed total misread: the prices are right but the sum disagrees.
	recs, _ := evaluate(context.Background(), params5(), &fakeModel{t: t, damage: func(a *ans, _ int) { a.Total += 7 }}, nil)
	if tl := summarize(recs); tl.FlaggedCorrect != 5 || tl.FalseAlarmRate.K != 5 || tl.AmountsExact.K != 5 || tl.Exact.K != 0 {
		t.Fatalf("false alarm: %+v", tl)
	}
	recs, _ = evaluate(context.Background(), params5(), &fakeModel{t: t, raw: func(int) string { return "Sorry, I can't help with that." }}, nil)
	if tl := summarize(recs); tl.FlaggedWrong != 5 || tl.NotParsed != 5 || tl.Reasons[receipts.ReasonInvalid] != 5 {
		t.Fatalf("unusable: %+v", tl)
	}
}

// flaky fails the first n calls of each text with err, then answers like the fake model.
type flaky struct {
	inner *fakeModel
	err   error
	n     int
	mu    sync.Mutex
	seen  map[string]int
}

func (f *flaky) Extract(ctx context.Context, text string) (receiptparse.Extraction, error) {
	f.mu.Lock()
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[text]++
	k := f.seen[text]
	f.mu.Unlock()
	if k <= f.n {
		return receiptparse.Extraction{}, f.err
	}
	return f.inner.Extract(ctx, text)
}

func TestRetry_WhatIsRetriedAndWhatIsNot(t *testing.T) {
	var waits []time.Duration
	p := params5()
	p.Workers = 1
	p.Wait = func(_ context.Context, d time.Duration) { waits = append(waits, d) }

	// Two 429s that ask for 7 s, then success: retried, and the wait honors retry-after.
	recs, _ := evaluate(context.Background(), p, &flaky{inner: &fakeModel{t: t}, n: 2, err: &anthropic.APIError{Status: 429, After: 7 * time.Second}}, nil)
	tl := summarize(recs)
	if tl.CallsFailed != 0 || tl.Calls != 5 || tl.Retried != 5 || tl.AttemptsTotal != 15 {
		t.Fatalf("429: %+v", tl)
	}
	for _, w := range waits {
		if w < 7*time.Second {
			t.Fatalf("waited %v, less than retry-after", w)
		}
	}

	// A 400 is never retried: repeating a bad request only repeats the spend.
	bad := &flaky{inner: &fakeModel{t: t}, n: 99, err: &anthropic.APIError{Status: 400, Type: "invalid_request_error"}}
	recs, _ = evaluate(context.Background(), params5(), bad, nil)
	tl = summarize(recs)
	if tl.CallsFailed != 5 || tl.AttemptsTotal != 5 || !strings.Contains(recs[0].CallError, "HTTP 400") {
		t.Fatalf("400: %+v / %q", tl, recs[0].CallError)
	}

	// A 500 that never clears stops at the attempt limit and is recorded, not dropped.
	down := &flaky{inner: &fakeModel{t: t}, n: 99, err: &anthropic.APIError{Status: 500}}
	waits = nil
	recs, err := evaluate(context.Background(), p, down, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl = summarize(recs)
	if tl.CallsFailed != 5 || tl.AttemptsTotal != 15 || tl.Calls != 0 {
		t.Fatalf("500: %+v", tl)
	}
	// 3 attempts a call means 2 pauses, none after the last attempt, and the error says so.
	if len(waits) != 10 || !strings.Contains(recs[0].CallError, "after 3 attempt(s)") {
		t.Fatalf("500: %d pauses (want 10), error %q", len(waits), recs[0].CallError)
	}
}

// The real client against the mock LLM server: the same code path as a run against the
// service, with the request and the response going over HTTP.
func TestEvaluate_RealClientAgainstTheMockLLM(t *testing.T) {
	srv, err := mockllm.New(mockllm.Config{Fixtures: synth.Fixtures(5001, 5), Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client, err := anthropic.New(anthropic.Config{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := evaluate(context.Background(), params5(), client, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl := summarize(recs)
	if tl.Calls != 5 || tl.AcceptedCorrect != 5 || tl.Exact.K != 5 || tl.CallsFailed != 0 {
		t.Fatalf("%+v", tl)
	}
}

func TestCheckSpend(t *testing.T) {
	if err := checkSpend("", 250, 0); err == nil {
		t.Error("the real service must need a confirmation")
	}
	if err := checkSpend("", 250, 100); err == nil || !strings.Contains(err.Error(), "250") {
		t.Errorf("a wrong number must be refused and name the right one: %v", err)
	}
	if err := checkSpend("https://api.anthropic.com", 250, 250); err != nil {
		t.Errorf("the right number must pass: %v", err)
	}
	if err := checkSpend("https://example.com", 5, 0); err == nil {
		t.Error("any non-loopback host is a real service")
	}
	for _, u := range []string{"http://127.0.0.1:8089", "http://localhost:8089", "http://[::1]:8089"} {
		if err := checkSpend(u, 250, 0); err != nil {
			t.Errorf("%s is the mock and needs no confirmation: %v", u, err)
		}
	}
}

func TestChooseOut_OnlyAnExplicitCleanRealRunWritesIntoTheRepository(t *testing.T) {
	// not recording: never the repository
	for _, c := range []struct{ out string }{{""}, {"/somewhere"}} {
		d, err := chooseOut(false, c.out, true, false, false)
		if err != nil || strings.HasPrefix(d, "bench") || (c.out == "" && !strings.Contains(d, "fifty-accuracy")) {
			t.Errorf("not recording, out %q: %q %v", c.out, d, err)
		}
	}
	// a dirty first look against the real service is not written into bench/results either
	if d, err := chooseOut(false, "", true, true, true); err != nil || d == recordDir {
		t.Errorf("first look: %q %v", d, err)
	}
	if d, err := chooseOut(true, "", true, false, false); err != nil || d != recordDir {
		t.Errorf("a recording: %q %v", d, err)
	}
	for name, args := range map[string][5]any{
		"with --out":         {true, "elsewhere", true, false, false},
		"on a dirty tree":    {true, "", true, true, false},
		"with --allow-dirty": {true, "", true, false, true},
		"against the mock":   {true, "", false, false, false},
	} {
		if d, err := chooseOut(args[0].(bool), args[1].(string), args[2].(bool), args[3].(bool), args[4].(bool)); err == nil {
			t.Errorf("--record %s must be refused, got %q", name, d)
		}
	}
}

// analyze reads a recording and must reproduce the counts the harness itself printed:
// here a model that drops every quantity (accepted, because the arithmetic still holds)
// and one that moves a price and back-solves another (accepted too).
func TestAnalyze_SplitsWrongParsesByKindAndDecision(t *testing.T) {
	p := params5()
	p.N = 6
	// Only the even references are damaged, so some answers are right and must not be
	// counted as wrong.
	dropQuantities := &fakeModel{t: t, damage: func(a *ans, ref int) {
		if ref%2 != 0 {
			return
		}
		for i := range a.Items {
			a.Items[i].Quantity = 0
		}
	}}
	recs, err := evaluate(context.Background(), p, dropQuantities, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl := summarize(recs)
	if tl.AcceptedWrong == 0 || tl.AcceptedCorrect == 0 {
		t.Fatalf("the test needs both wrong and right answers: %+v", tl)
	}
	rep := report{Env: env{Commit: "test"}, Records: recs}
	f := filepath.Join(t.TempDir(), "rec.json")
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := analyze(f, &out); err != nil {
		t.Fatal(err)
	}
	// Every wrong parse is quantity-only and accepted, none flagged.
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "ground-truth-text") {
			line = l
		}
	}
	want := fmt.Sprintf("%-18s %6d | %10d / %-10d | %10d / %-10d | %10d / %-10d", "ground-truth-text", tl.AcceptedWrong, tl.AcceptedWrong, 0, 0, 0, 0, 0)
	if line != want {
		t.Errorf("analysis row\n got: %q\nwant: %q\nfull output:\n%s", line, want, out.String())
	}
	if n := strings.Count(out.String(), ": quantity_only"); n != tl.AcceptedWrong {
		t.Errorf("silent error lines = %d, want %d", n, tl.AcceptedWrong)
	}
	if err := analyze(filepath.Join(t.TempDir(), "missing.json"), &out); err == nil {
		t.Error("a missing file must be an error")
	}
}
