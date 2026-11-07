// Command redaction measures how much planted personal data the regex redaction
// (internal/receipts.Redact) removes, on synthetic receipts, in two ways:
//
//   - on the ground-truth text, which is what the pipeline would see if OCR were perfect;
//   - on what Tesseract reads from the receipt rendered as an image, clean and with
//     increasing skew and pixel noise.
//
// Everything is synthetic and written by the same author as the patterns, so the
// numbers flatter the system: the planted formats are the ones the patterns were built
// for plus a few they were tested against. They say nothing about real receipts, real
// photos or a real person's address and name, which the redaction does not touch. See
// bench/README.md for the definitions and what is and is not claimed.
//
//	go run ./bench/redaction                 # 1000 receipts, every variant
//	go run ./bench/redaction --receipts 50 --allow-dirty   # smoke run (do not commit)
//
// It needs Tesseract installed (brew install tesseract) and a clean working tree, so the
// commit recorded is the code that ran (--allow-dirty for a smoke run only). It writes
// bench/results/redaction-<UTC timestamp>.json and .txt, then exits non-zero if OCR
// failed on any receipt (the files are written first: a recording is never deleted).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/riyne/fifty-fifty/internal/ocr"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

type variant struct {
	Name   string
	OCR    bool
	Render synth.RenderOptions
}

var allVariants = []variant{
	{Name: "ground-truth-text"},
	{Name: "ocr-clean", OCR: true},
	{Name: "ocr-mild", OCR: true, Render: synth.RenderOptions{SkewDegrees: 1.5, NoiseSigma: 10}},
	{Name: "ocr-moderate", OCR: true, Render: synth.RenderOptions{SkewDegrees: 3, NoiseSigma: 25}},
	{Name: "ocr-harsh", OCR: true, Render: synth.RenderOptions{SkewDegrees: 5, NoiseSigma: 40}},
}

type env struct {
	Machine     string `json:"machine"`
	CPUs        int    `json:"cpus"`
	MemoryBytes string `json:"memory_bytes"`
	OS          string `json:"os"`
	GoVersion   string `json:"go_version"`
	Tesseract   string `json:"tesseract_version"`
	TessPSM     int    `json:"tesseract_page_segmentation_mode"`
	LoadAvg     string `json:"load_average_at_start"`
	Commit      string `json:"commit"`
	Dirty       bool   `json:"working_tree_dirty"`
	Database    string `json:"database"`
}

type variantResult struct {
	Name           string                `json:"variant"`
	Description    string                `json:"description"`
	Receipts       int                   `json:"receipts_scored"`
	OCRFailures    int                   `json:"ocr_failures"`
	PerKind        map[string]*kindTally `json:"per_kind"`
	Overall        *kindTally            `json:"overall"`
	LeakedExamples []example             `json:"leaked_examples_first_10"`
}

type report struct {
	StartedAt  time.Time       `json:"started_at"`
	Params     params          `json:"parameters"`
	Env        env             `json:"environment"`
	DurationS  float64         `json:"duration_seconds"`
	Variants   []variantResult `json:"variants"`
	DigitRun   int             `json:"leak_rule_digit_run"`
	EmailRun   int             `json:"leak_rule_email_run_chars"`
	AnchorMin  int             `json:"leak_rule_anchor_min"`
	Statements []string        `json:"statements"`
}

type params struct {
	FirstRef int      `json:"first_ref"`
	Receipts int      `json:"receipts_per_variant"`
	Workers  int      `json:"ocr_processes_in_parallel"`
	Variants []string `json:"variants"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "redaction:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		first      = flag.Int("from", 1, "first receipt reference number")
		n          = flag.Int("receipts", 1000, "receipts per variant (references from..from+receipts-1)")
		workers    = flag.Int("workers", 4, "Tesseract processes in parallel (timing is not measured)")
		out        = flag.String("out", "bench/results", "output directory")
		only       = flag.String("variants", "", "comma-separated variant names (default: all)")
		allowDirty = flag.Bool("allow-dirty", false, "run with uncommitted changes (smoke runs only; do not commit the output)")
	)
	flag.Parse()
	if *n < 1 || *n > 100_000 || *first < 0 || *workers < 1 || *workers > 32 {
		return errors.New("--receipts must be 1-100000, --from >= 0, --workers 1-32")
	}
	chosen := allVariants
	if *only != "" {
		chosen = nil
		for _, name := range strings.Split(*only, ",") {
			found := false
			for _, v := range allVariants {
				if v.Name == strings.TrimSpace(name) {
					chosen, found = append(chosen, v), true
				}
			}
			if !found {
				return fmt.Errorf("unknown variant %q", name)
			}
		}
	}
	ctx := context.Background()
	reader, err := ocr.New(ocr.Config{})
	if err != nil {
		return err
	}
	e, err := captureEnv(ctx, reader)
	if err != nil {
		return err
	}
	if e.Dirty && !*allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a smoke run)")
	}

	rep := report{StartedAt: time.Now().UTC(), Env: e, DigitRun: digitRun, EmailRun: emailRun, AnchorMin: anchorMin}
	rep.Params = params{FirstRef: *first, Receipts: *n, Workers: *workers}
	for _, v := range chosen {
		rep.Params.Variants = append(rep.Params.Variants, v.Name)
	}
	rep.Statements = statements()

	start := time.Now()
	failures := 0
	for _, v := range chosen {
		fmt.Fprintf(os.Stderr, "%s: %d receipts\n", v.Name, *n)
		vr, err := runVariant(ctx, reader, v, *first, *n, *workers)
		if err != nil {
			return err
		}
		failures += vr.OCRFailures
		rep.Variants = append(rep.Variants, vr)
	}
	rep.DurationS = time.Since(start).Seconds()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(*out, "redaction-"+rep.StartedAt.Format("20060102T150405Z"))
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(base+".json", append(raw, '\n'), 0o644); err != nil {
		return err
	}
	text := render(rep)
	if err := os.WriteFile(base+".txt", []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Print(text)
	fmt.Println("wrote", base+".json and .txt")
	if failures > 0 {
		return fmt.Errorf("%d OCR failure(s): those receipts are excluded from the tallies, see the files", failures)
	}
	return nil
}

func runVariant(ctx context.Context, reader *ocr.Tesseract, v variant, first, n, workers int) (variantResult, error) {
	vr := variantResult{
		Name:        v.Name,
		Description: describe(v),
		PerKind:     map[string]*kindTally{string(synth.Card): {}, string(synth.Email): {}, string(synth.Phone): {}},
		Overall:     &kindTally{},
	}
	per := map[synth.Kind]*kindTally{synth.Card: vr.PerKind["card"], synth.Email: vr.PerKind["email"], synth.Phone: vr.PerKind["phone"]}

	type item struct {
		rc   synth.Receipt
		text string
		err  error
	}
	items := make([]item, n)
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				ref := first + i
				rc := synth.Generate(ref)
				it := item{rc: rc, text: rc.Text()}
				if v.OCR {
					opts := v.Render
					opts.Seed = uint64(ref)
					var img []byte
					if img, it.err = rc.Render(opts); it.err == nil {
						cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
						it.text, it.err = reader.Text(cctx, img, "image/png")
						cancel()
					}
				}
				items[i] = it
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()

	for _, it := range items { // in reference order, so the examples are reproducible
		if it.err != nil {
			vr.OCRFailures++
			fmt.Fprintf(os.Stderr, "  ref %d: %v\n", it.rc.Ref, it.err)
			continue
		}
		vr.Receipts++
		evaluate(it.rc, it.rc.Text(), it.text, per, vr.Overall, &vr.LeakedExamples, 10)
	}
	for _, t := range per {
		t.finish()
	}
	vr.Overall.finish()
	return vr, nil
}

func describe(v variant) string {
	if !v.OCR {
		return "the ground-truth text of the receipt, as a perfect reader would produce it"
	}
	return fmt.Sprintf("the receipt rendered as an image (skew %.1f degrees, Gaussian pixel noise sigma %.0f gray levels), read by Tesseract", v.Render.SkewDegrees, v.Render.NoiseSigma)
}

func captureEnv(ctx context.Context, reader *ocr.Tesseract) (env, error) {
	sh := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(out))
	}
	ver, err := reader.Version(ctx)
	if err != nil {
		return env{}, err
	}
	return env{
		CPUs: runtime.NumCPU(), GoVersion: runtime.Version(), OS: sh("uname", "-srm"),
		Machine: sh("sysctl", "-n", "machdep.cpu.brand_string"), MemoryBytes: sh("sysctl", "-n", "hw.memsize"),
		Tesseract: ver, TessPSM: 6, LoadAvg: sh("uptime"), Commit: sh("git", "rev-parse", "HEAD"),
		Dirty:    sh("git", "status", "--porcelain", "--", ".", ":(exclude)bench/results") != "",
		Database: "not used (this benchmark touches no database)",
	}, nil
}

func statements() []string {
	return []string{
		"Synthetic data only: receipts, planted card numbers, phone numbers and email addresses are generated by internal/receipts/synth, by the same author as the patterns being measured.",
		"A planted value 'leaks' under the rule stated in leak_rule_*: at least digit_run consecutive digits of a card or phone number, or email_run consecutive characters of an email's local part, still readable in the redacted line of the OCR text that is aligned with the line the value was printed on. Weaker than 'no trace': an email's domain, shorter digit runs and anything around the values are not counted. A value of which fewer than 4 digits or characters survive OCR anywhere is reported as lost by OCR, and counts as not leaked.",
		"The synthetic phone numbers are all in the fictional 555-01xx block, so numbers on one receipt share digits; that is why a value is judged against the line it was printed on, found by aligning the OCR lines with the ground-truth lines, and not against the whole text.",
		"Go Mono, the font the receipts are drawn in, has a slashed zero, which Tesseract often reads as @, Q or G. That makes digits harder to read than in a typical receipt font and is a property of this synthetic data, not of receipts in general.",
		"Names and street addresses are not redacted by design and are not part of the planted values, so recall says nothing about them.",
		"The formats planted are the ones the patterns were written and tested against (6 phone layouts, 4 card layouts, plain email addresses). Spelled-out forms, digits as words and other layouts are known gaps pinned in internal/receipts/redact_test.go and are not in this sample.",
		"OCR runs on rendered text in one monospace font, upright or with small skew and Gaussian noise, which is far easier than a photograph. The OCR variants say what Tesseract does to this synthetic noise, not to real photos.",
	}
}

func render(rep report) string {
	var b strings.Builder
	e := rep.Env
	fmt.Fprintf(&b, "Redaction recall benchmark (synthetic receipts; regex redaction of card numbers, phone numbers and email addresses)\n")
	fmt.Fprintf(&b, "started %s, commit %s (working tree dirty: %v), ran %.0f s\n", rep.StartedAt.Format(time.RFC3339), e.Commit, e.Dirty, rep.DurationS)
	fmt.Fprintf(&b, "machine: %s, %d CPUs, %s bytes RAM, %s, %s\n", e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.GoVersion)
	fmt.Fprintf(&b, "%s, page segmentation mode %d; load average at start: %s\n", e.Tesseract, e.TessPSM, e.LoadAvg)
	fmt.Fprintf(&b, "database: %s\n", e.Database)
	fmt.Fprintf(&b, "receipts %d..%d per variant (the same receipts in every variant); %d Tesseract processes in parallel (timing is not measured)\n",
		rep.Params.FirstRef, rep.Params.FirstRef+rep.Params.Receipts-1, rep.Params.Workers)
	fmt.Fprintf(&b, "leak rule: %d+ consecutive digits of a card or phone number, or %d+ consecutive characters of an email's local part, still readable after redaction, judged on the OCR line aligned with the line it was printed on (%d+ digits or characters must be found there, else the value is counted as lost by OCR)\n\n", rep.DigitRun, rep.EmailRun, anchorMin)
	for _, v := range rep.Variants {
		fmt.Fprintf(&b, "== %s: %s\n", v.Name, v.Description)
		fmt.Fprintf(&b, "   receipts scored %d, OCR failures %d\n", v.Receipts, v.OCRFailures)
		fmt.Fprintf(&b, "   %-8s %8s %7s %8s  %-23s %10s %14s %14s %9s %12s\n", "kind", "planted", "leaked", "recall", "95% interval (Wilson)", "read as is", "leaked: intact", "leaked: garbled", "lost(OCR)", "placeholders")
		kinds := make([]string, 0, len(v.PerKind))
		for k := range v.PerKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		row := func(name string, t *kindTally) {
			fmt.Fprintf(&b, "   %-8s %8d %7d %7.2f%%  [%6.2f%%, %6.2f%%]      %10d %14d %14d %9d %12d\n", name, t.Planted, t.Leaked, 100*t.Recall,
				100*t.RecallLow95, 100*t.RecallHigh95, t.ReadExactly, t.LeakedWhenReadExactly, t.LeakedWhenGarbled, t.LostByOCR, t.Replacements)
		}
		for _, k := range kinds {
			row(k, v.PerKind[k])
		}
		row("all", v.Overall)
		if len(v.LeakedExamples) > 0 {
			fmt.Fprintf(&b, "   first leaked values (synthetic):\n")
			for _, x := range v.LeakedExamples {
				fmt.Fprintf(&b, "     ref %d %s %q (read exactly by OCR: %v) -> %q\n", x.Ref, x.Kind, x.Value, x.ReadExactly, x.RedactedLine)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("What this does and does not say:\n")
	for _, s := range rep.Statements {
		fmt.Fprintf(&b, " - %s\n", s)
	}
	return b.String()
}
