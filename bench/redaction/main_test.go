package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/ocr"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

func realReader(t *testing.T) *ocr.Tesseract {
	t.Helper()
	r, err := ocr.New(ocr.Config{})
	if errors.Is(err, ocr.ErrNotInstalled) {
		if os.Getenv("REQUIRE_TESSERACT") == "1" {
			t.Fatalf("REQUIRE_TESSERACT=1 but tesseract is not installed: %v", err)
		}
		t.Skip("tesseract is not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A small end-to-end run of the benchmark's own code: the tallies count exactly what the
// generator planted, the ground-truth variant removes everything, the OCR variants never
// score more values than exist, and the report renders and round-trips as JSON.
func TestRunVariant_SmokeAndReport(t *testing.T) {
	reader := realReader(t)
	const first, n = 1, 12
	wantPlanted := map[synth.Kind]int{}
	for ref := first; ref < first+n; ref++ {
		for _, p := range synth.Generate(ref).Planted {
			wantPlanted[p.Kind]++
		}
	}
	var results []variantResult
	for _, v := range allVariants {
		vr, err := runVariant(context.Background(), reader, v, first, n, 3)
		if err != nil {
			t.Fatal(err)
		}
		if vr.Receipts != n || vr.OCRFailures != 0 {
			t.Fatalf("%s: %d scored, %d failures", v.Name, vr.Receipts, vr.OCRFailures)
		}
		for kind, want := range wantPlanted {
			c := vr.PerKind[string(kind)]
			if c.Planted != want || c.Leaked+c.LostByOCR > c.Planted || c.Recall < 0 || c.Recall > 1 || c.RecallLow95 > c.Recall || c.RecallHigh95 < c.Recall {
				t.Errorf("%s/%s: %+v (want %d planted)", v.Name, kind, c, want)
			}
		}
		if v.Name == "ground-truth-text" && (vr.Overall.Leaked != 0 || vr.Overall.Recall != 1) {
			t.Errorf("ground truth leaked: %+v", vr.Overall)
		}
		results = append(results, vr)
	}
	rep := report{StartedAt: time.Now().UTC(), Variants: results, DigitRun: digitRun, EmailRun: emailRun, AnchorMin: anchorMin, Statements: statements(),
		Params: params{FirstRef: first, Receipts: n, Workers: 3}, Env: env{Tesseract: "tesseract test", Database: "not used"}}
	text := render(rep)
	for _, want := range []string{"ground-truth-text", "ocr-harsh", "Wilson", "Synthetic data only", "Names and street addresses are not redacted", "slashed zero", "lost(OCR)", "database: not used"} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not mention %q", want)
		}
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back report
	if err := json.Unmarshal(raw, &back); err != nil || len(back.Variants) != len(allVariants) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestRunVariant_ReportsReproducibleExamples(t *testing.T) {
	reader := realReader(t)
	v := allVariants[len(allVariants)-1] // the harshest: it has leaks
	a, err := runVariant(context.Background(), reader, v, 1, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := runVariant(context.Background(), reader, v, 1, 20, 4) // more workers, same answer
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Error("two runs over the same receipts differ (worker count must not change the result)")
	}
}
