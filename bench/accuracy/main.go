// Command accuracy measures how well a language model reads receipts through the same
// path the worker uses: Tesseract (or perfect text), the regex redaction, the Anthropic
// client with the project's prompt, and the strict validator that decides whether a
// person has to look (receipts.Interpret). Each answer is scored against the ground truth
// of a synthetic receipt (internal/receipts/synth).
//
// The result that matters is not "how often is the model right" but the split of the
// system's decision against correctness: of the wrong parses, how many did the review
// flag catch, and how many were accepted silently. See eval.go for the definitions and
// bench/README.md for what the numbers do and do not say.
//
// Everything is synthetic and the receipts, the prompt and this harness were written by
// one author, so the numbers flatter the system; they say nothing about real receipts or
// photographs.
//
//	# a first look: 3 receipts, perfect text and a clean render (6 calls). Not a recording:
//	# the result goes to a directory outside the repository.
//	read -s ANTHROPIC_API_KEY; export ANTHROPIC_API_KEY        # this shell only
//	go run ./bench/accuracy --from 1 --receipts 3 --variants ground-truth-text,ocr-clean --allow-dirty --confirm-calls 6
//
//	# the recorded run: 50 receipts x 5 variants = 250 calls, clean tree, into bench/results
//	make bench-accuracy        # = go run ./bench/accuracy --record --confirm-calls 250
//
//	# the same code against the mock LLM, no key, no spend (for a dry run)
//	go run ./bench/accuracy --base-url http://127.0.0.1:8089 --allow-dirty
//
// A run against the real API refuses to start unless --confirm-calls equals the number of
// calls it is about to make, so a typo cannot multiply the spend. It needs a clean working
// tree (so the commit recorded is the code that ran), Tesseract for the ocr-* variants,
// and writes accuracy-<UTC timestamp>.json and .txt (into bench/results only with
// --record) before exiting non-zero if any call failed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/riyne/fifty-fifty/internal/anthropic"
	"github.com/riyne/fifty-fifty/internal/ocr"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

type variant struct {
	Name   string
	OCR    bool
	Render synth.RenderOptions
}

// The same five variants as the redaction benchmark, so the two can be read together.
var allVariants = []variant{
	{Name: "ground-truth-text"},
	{Name: "ocr-clean", OCR: true},
	{Name: "ocr-mild", OCR: true, Render: synth.RenderOptions{SkewDegrees: 1.5, NoiseSigma: 10}},
	{Name: "ocr-moderate", OCR: true, Render: synth.RenderOptions{SkewDegrees: 3, NoiseSigma: 25}},
	{Name: "ocr-harsh", OCR: true, Render: synth.RenderOptions{SkewDegrees: 5, NoiseSigma: 40}},
}

// record is one (receipt, variant) call and what came of it.
type record struct {
	Ref     int    `json:"ref"`
	Variant string `json:"variant"`

	OCRChars   int                 `json:"text_chars_sent"`
	Redactions receipts.Redactions `json:"redactions"`

	CallError    string `json:"call_error,omitempty"`
	Attempts     int    `json:"attempts"`
	LatencyMS    int64  `json:"latency_ms_of_the_answered_attempt"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`

	Status  string   `json:"status"` // what the system chose: claiming or needs_review
	Reasons []string `json:"reasons,omitempty"`
	Score   score    `json:"score"`
	// AmountsNotInText: the amounts of the answer that are not printed in the text sent (see
	// amountsNotInText). Computed from M10.9; absent in earlier recordings.
	AmountsNotInText []int64 `json:"amounts_not_in_the_text_sent,omitempty"`

	// Kept only for answers that are not Exact, to study what went wrong. Synthetic data.
	Answer    string `json:"answer,omitempty"`
	InputText string `json:"redacted_text_sent,omitempty"` // only when not AmountsExact
}

type env struct {
	Machine     string `json:"machine"`
	CPUs        int    `json:"cpus"`
	MemoryBytes string `json:"memory_bytes"`
	OS          string `json:"os"`
	GoVersion   string `json:"go_version"`
	Tesseract   string `json:"tesseract_version,omitempty"`
	LoadAvg     string `json:"load_average_at_start"`
	Commit      string `json:"commit"`
	Dirty       bool   `json:"working_tree_dirty"`
	BaseURL     string `json:"base_url"`
	Model       string `json:"model_requested"`
	MaxTokens   int    `json:"max_tokens"`
	// PromptVersion and PromptSHA256 say which system prompt the model was given (M10.9).
	// Recordings made before it have neither: they used what is now PromptV1.
	PromptVersion string `json:"prompt_version,omitempty"`
	PromptSHA256  string `json:"prompt_sha256,omitempty"`
	Note          string `json:"note"`
}

type params struct {
	FirstRef  int      `json:"first_ref"`
	Receipts  int      `json:"receipts_per_variant"`
	Variants  []string `json:"variants"`
	Calls     int      `json:"calls_planned"`
	Workers   int      `json:"calls_in_parallel"`
	Attempts  int      `json:"max_attempts_per_call"`
	RealModel bool     `json:"base_url_is_not_loopback"`
}

type variantResult struct {
	Name        string `json:"variant"`
	Description string `json:"description"`
	Tally       tally  `json:"tally"`
}

type report struct {
	StartedAt  time.Time       `json:"started_at"`
	Params     params          `json:"parameters"`
	Env        env             `json:"environment"`
	DurationS  float64         `json:"duration_seconds"`
	Variants   []variantResult `json:"variants"`
	Records    []record        `json:"records"`
	Statements []string        `json:"statements"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "accuracy:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		first      = flag.Int("from", 5001, "first receipt reference number (the redaction benchmark used 1-1000, the first look 1-3)")
		n          = flag.Int("receipts", 50, "receipts per variant")
		only       = flag.String("variants", "", "comma-separated variant names (default: all five)")
		workers    = flag.Int("workers", 4, "calls in parallel")
		attempts   = flag.Int("attempts", 4, "attempts per call (a 429, a 5xx or an unreadable answer is retried after the longer of retry-after and a backoff)")
		baseURL    = flag.String("base-url", "", "Messages API base URL (default the public API; an http loopback address, such as the mock LLM, needs no key)")
		model      = flag.String("model", "", "model (default "+anthropic.DefaultModel+")")
		maxTokens  = flag.Int("max-tokens", 0, "max answer tokens (default the client's)")
		promptVer  = flag.String("prompt-version", "", "system prompt: "+anthropic.PromptV1+" (the default, what the worker uses and what every recording before M10.9 used) or "+anthropic.PromptV2+" (adds the rule about prices that cannot be read)")
		confirm    = flag.Int("confirm-calls", 0, "required for a non-loopback base URL: must equal the number of calls about to be made")
		out        = flag.String("out", "", "output directory for a run that is not recorded (default: a directory under the system temp dir; nothing is written into the repository)")
		recording  = flag.Bool("record", false, "write the result into bench/results (a recording: real service, clean working tree, never with --allow-dirty or --out)")
		allowDirty = flag.Bool("allow-dirty", false, "run with uncommitted changes (first looks and smoke runs only; do not commit the output)")
	)
	analyzePath := flag.String("analyze", "", "read a recorded .json and print the wrong parses by kind (no calls, no spend), then exit")
	flag.Parse()
	if *analyzePath != "" {
		return analyze(*analyzePath, os.Stdout)
	}
	if *n < 1 || *n > 1000 || *first < 0 || *workers < 1 || *workers > 16 || *attempts < 1 || *attempts > 10 {
		return errors.New("--receipts must be 1-1000, --from >= 0, --workers 1-16, --attempts 1-10")
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
	calls := *n * len(chosen)
	live := isLive(*baseURL)
	if err := checkSpend(*baseURL, calls, *confirm); err != nil {
		return err
	}

	client, err := anthropic.New(anthropic.Config{BaseURL: *baseURL, APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: *model, MaxTokens: *maxTokens, PromptVersion: *promptVer})
	if err != nil {
		return err
	}
	ctx := context.Background()
	needOCR := false
	for _, v := range chosen {
		needOCR = needOCR || v.OCR
	}
	var reader receiptparse.OCR
	tessVersion := ""
	if needOCR {
		t, err := ocr.New(ocr.Config{})
		if err != nil {
			return err
		}
		if tessVersion, err = t.Version(ctx); err != nil {
			return err
		}
		reader = t
	}
	e := captureEnv(client, tessVersion)
	if e.Dirty && !*allowDirty {
		return errors.New("working tree has uncommitted changes; commit first so the recorded commit hash is the code that ran (or --allow-dirty for a first look)")
	}
	outDir, err := chooseOut(*recording, *out, live, e.Dirty, *allowDirty)
	if err != nil {
		return err
	}

	rep := report{StartedAt: time.Now().UTC(), Env: e, Statements: statements()}
	rep.Params = params{FirstRef: *first, Receipts: *n, Calls: calls, Workers: *workers, Attempts: *attempts, RealModel: live}
	for _, v := range chosen {
		rep.Params.Variants = append(rep.Params.Variants, v.Name)
	}

	start := time.Now()
	recs, err := evaluate(ctx, evalParams{First: *first, N: *n, Variants: chosen, Workers: *workers, Attempts: *attempts, Wait: wait, Log: os.Stderr}, client, reader)
	if err != nil {
		return err
	}
	rep.DurationS = time.Since(start).Seconds()
	rep.Records = recs
	failed := 0
	for _, v := range chosen {
		var mine []record
		for _, r := range recs {
			if r.Variant == v.Name {
				mine = append(mine, r)
			}
		}
		t := summarize(mine)
		failed += t.CallsFailed
		rep.Variants = append(rep.Variants, variantResult{Name: v.Name, Description: describe(v), Tally: t})
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(outDir, "accuracy-"+rep.StartedAt.Format("20060102T150405Z"))
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
	if failed > 0 {
		return fmt.Errorf("%d call(s) failed: they are excluded from the tallies and listed in the files", failed)
	}
	return nil
}

// recordDir is where a recording goes.
const recordDir = "bench/results"

// chooseOut decides where the result is written. Only an explicit --record writes into
// the repository, and only for a run that can be a recording: the real service, a clean
// tree. Everything else (a first look, a dry run against the mock) goes to a directory
// outside the repository, because `git add .` would otherwise commit it.
func chooseOut(record bool, out string, live, dirty, allowDirty bool) (string, error) {
	if !record {
		if out != "" {
			return out, nil
		}
		return filepath.Join(os.TempDir(), "fifty-accuracy"), nil
	}
	switch {
	case out != "":
		return "", errors.New("--record writes to " + recordDir + "; do not combine it with --out")
	case allowDirty || dirty:
		return "", errors.New("--record needs a clean working tree and no --allow-dirty: the commit recorded must be the code that ran")
	case !live:
		return "", errors.New("--record is for a run against the real service; a run against the mock is not a result")
	}
	return recordDir, nil
}

// isLive reports whether the base URL is a real service (anything but a loopback address).
func isLive(baseURL string) bool { return baseURL == "" || !loopbackURL(baseURL) }

// checkSpend refuses a run against a real service unless the caller typed the number of
// calls it is about to make, so a mistake in --receipts or --variants cannot silently
// multiply the spend.
func checkSpend(baseURL string, calls, confirm int) error {
	if isLive(baseURL) && confirm != calls {
		return fmt.Errorf("this would make %d calls to %s; pass --confirm-calls %d to go ahead (set a spend limit in the console first)", calls, orDefault(baseURL, anthropic.DefaultBaseURL), calls)
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

type evalParams struct {
	First, N          int
	Variants          []variant
	Workers, Attempts int
	// Wait pauses between attempts; tests replace it.
	Wait func(ctx context.Context, d time.Duration)
	Log  interface{ Write([]byte) (int, error) }
}

// evaluate prepares the text of every (receipt, variant), asks the extractor, and scores
// the answers. Records come back in (variant, ref) order. A call that fails after its
// attempts is a record with CallError, not an abort: a run is never thrown away.
func evaluate(ctx context.Context, p evalParams, ex receiptparse.Extractor, reader receiptparse.OCR) ([]record, error) {
	type task struct {
		rec     *record
		truth   synth.Receipt
		text    string
		skipped bool // no text to send
	}
	tasks := make([]*task, 0, p.N*len(p.Variants))

	// Phase one: the text each variant would send. OCR is local and parallel.
	for _, v := range p.Variants {
		vt := make([]*task, p.N)
		var wg sync.WaitGroup
		next := make(chan int)
		for w := 0; w < p.Workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					ref := p.First + i
					truth := synth.Generate(ref)
					t := &task{rec: &record{Ref: ref, Variant: v.Name}, truth: truth}
					text := truth.Text()
					if v.OCR {
						opts := v.Render
						opts.Seed = uint64(ref)
						img, err := truth.Render(opts)
						if err == nil {
							cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
							text, err = reader.Text(cctx, img, "image/png")
							cancel()
						}
						if err != nil {
							t.rec.CallError = "ocr failed: " + err.Error()
							vt[i] = t
							continue
						}
					}
					// The worker's own function: redact, cut, trim (receiptparse.PrepareText, the
					// default cap).
					redacted, counts := receiptparse.PrepareText(text, 0)
					t.rec.Redactions, t.rec.OCRChars = counts, utf8.RuneCountInString(redacted)
					t.text, t.skipped = redacted, redacted == ""
					vt[i] = t
				}
			}()
		}
		for i := 0; i < p.N; i++ {
			next <- i
		}
		close(next)
		wg.Wait()
		tasks = append(tasks, vt...)
	}

	// Phase two: the calls.
	var done atomic.Int64
	total := int64(len(tasks))
	var wg sync.WaitGroup
	next := make(chan *task)
	for w := 0; w < p.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range next {
				r := t.rec
				switch {
				case r.CallError != "": // OCR failed
				case t.skipped:
					r.Status, r.Reasons = receipts.StatusNeedsReview, []string{receiptparse.ReasonNoText}
					r.Score = scoreAnswer(t.truth, nil)
				default:
					got, attempts, lat, err := extractWithRetry(ctx, ex, t.text, p.Attempts, p.Wait)
					r.Attempts = attempts
					if err != nil {
						r.CallError = err.Error()
						break
					}
					out := receipts.Interpret(got.Answer)
					r.LatencyMS, r.Model, r.InputTokens, r.OutputTokens = lat.Milliseconds(), got.Model, got.InputTokens, got.OutputTokens
					r.Status, r.Reasons = out.Status, out.Reasons
					r.Score = scoreAnswer(t.truth, got.Answer)
					r.AmountsNotInText = amountsNotInText(t.text, got.Answer)
					if !r.Score.Exact {
						r.Answer = string(got.Answer)
					}
					if !r.Score.AmountsExact {
						r.InputText = t.text
					}
				}
				if d := done.Add(1); p.Log != nil && (d%25 == 0 || d == total) {
					fmt.Fprintf(p.Log, "  %d of %d\n", d, total)
				}
			}
		}()
	}
	for _, t := range tasks {
		next <- t
	}
	close(next)
	wg.Wait()

	recs := make([]record, len(tasks))
	for i, t := range tasks {
		recs[i] = *t.rec
	}
	return recs, ctx.Err()
}

// extractWithRetry asks the extractor, retrying what is worth retrying: a 429, a 5xx, a
// transport failure, an unreadable 200. A permanent refusal (anthropic.APIError.Permanent: a
// bad request, a bad key) is final, because repeating it only repeats the spend. It returns the number of
// attempts made and the duration of the answered one.
func extractWithRetry(ctx context.Context, ex receiptparse.Extractor, text string, maxAttempts int, wait func(context.Context, time.Duration)) (receiptparse.Extraction, int, time.Duration, error) {
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		got, err := ex.Extract(cctx, text)
		d := time.Since(start)
		cancel()
		if err == nil {
			return got, attempt, d, nil
		}
		last = err
		if ctx.Err() != nil || !retryable(err) || attempt == maxAttempts {
			return receiptparse.Extraction{}, attempt, d, fmt.Errorf("after %d attempt(s): %w", attempt, last)
		}
		pause := 2 * time.Second << (attempt - 1)
		var ra interface{ RetryAfter() time.Duration }
		if errors.As(err, &ra) && ra.RetryAfter() > pause {
			pause = ra.RetryAfter()
		}
		wait(ctx, min(pause, time.Minute))
	}
	return receiptparse.Extraction{}, maxAttempts, 0, last
}

func retryable(err error) bool {
	// The same classification the worker uses (anthropic.APIError.Permanent): a refusal that
	// cannot succeed on a retry is not retried; everything else (429, 5xx, transport) is.
	var ae *anthropic.APIError
	if errors.As(err, &ae) {
		return !ae.Permanent()
	}
	return true
}

func wait(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func describe(v variant) string {
	if !v.OCR {
		return "the ground-truth text of the receipt, as a perfect reader would produce it, redacted, sent to the model"
	}
	return fmt.Sprintf("the receipt rendered as an image (skew %.1f degrees, Gaussian pixel noise sigma %.0f gray levels), read by Tesseract, redacted, sent to the model", v.Render.SkewDegrees, v.Render.NoiseSigma)
}

func captureEnv(c *anthropic.Client, tess string) env {
	sh := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(out))
	}
	return env{
		CPUs: runtime.NumCPU(), GoVersion: runtime.Version(), OS: sh("uname", "-srm"),
		Machine: sh("sysctl", "-n", "machdep.cpu.brand_string"), MemoryBytes: sh("sysctl", "-n", "hw.memsize"),
		Tesseract: tess, LoadAvg: sh("uptime"), Commit: sh("git", "rev-parse", "HEAD"),
		Dirty:   sh("git", "status", "--porcelain", "--", ".", ":(exclude)bench/results") != "",
		BaseURL: c.BaseURL(), Model: c.Model(), MaxTokens: anthropic.DefaultMaxTokens,
		PromptVersion: c.PromptVersion(), PromptSHA256: c.PromptSHA256(),
		Note: "the system prompt (the version and hash above) is fixed in internal/anthropic/client.go at the recorded commit; no sampling parameter is sent (the model's defaults apply); the model field in each record is what the service reported",
	}
}

func statements() []string {
	return []string{
		"Synthetic data only. The receipts (internal/receipts/synth), the prompt (internal/anthropic) and this harness were written by one author, in one clean layout (a monospace font, item lines with a price at the right, tax, tip and total lines). A model reading real receipts, photographs, other languages or other layouts would do differently, probably worse. Nothing here is a statement about real-world accuracy.",
		"Sample size: the receipts per variant are few, so every rate carries a wide 95% Wilson interval, which is printed. Differences between variants smaller than the intervals say nothing. The same receipts are used in every variant, so the variants are not independent samples.",
		"One run of one model with its default sampling (no temperature is sent: Claude Haiku 5.5 refuses any other value). A repeat can give different answers, and the service may change the model behind a name; the model string the service reported is in every record.",
		"'Correct' means the prices of the claimable units equal the receipt's, as a multiset, and tax and tip are equal (amounts_exact). A misread description does not make a parse wrong in this sense; 'exact' also requires the descriptions and the printed total. A parse that is wrong and accepted (status claiming) is a silent error. The review flag is the arithmetic check items + tax + tip = printed total, nothing else.",
		"A wrong parse that adds up (a model that misreads a price and the total together, or misreads consistently) is not caught by the flag. The count of such parses is the silent-error figure.",
		"Prompt injection is NOT tested: no receipt here contains instructions aimed at the model. Nothing in this evaluation supports a claim of resistance to it.",
		"The text sent is what the worker would send: OCR (or the ground-truth text), redaction of cards, emails and phone numbers, trimmed and cut at 6000 characters. The synthetic receipts contain planted card numbers, emails and phone numbers; after redaction the model sees placeholders in their place.",
		"Cost is not computed here: no price is written in this repository. The token counts are recorded; multiply by the current price list.",
		"If the prompt or the validator is changed after seeing a result, the earlier result stays recorded and is not replaced. A prompt version other than v1 was written after seeing the first recording's errors, so a result for it on the receipts used to find them (5001-5050) is not independent evidence; the comparison that counts is on references that were not looked at (5101-5150).",
		"'Amount not printed in the text sent' (M10.9) counts the amounts in an answer (line totals, tax, tip, total) that are not whole amount tokens of the text the model was given: it measures guessing, correct or not, and is computed from the text and the answer alone. An amount that is printed but wrong (the subtotal used as the total) is not counted. It is a property of this synthetic format, which prints every amount with two decimals.",
	}
}

func pct(r rate) string {
	return fmt.Sprintf("%3d/%-3d %5.1f%% [%5.1f%%, %5.1f%%]", r.K, r.N, 100*r.P, 100*r.Low, 100*r.High)
}

func render(rep report) string {
	var b strings.Builder
	e := rep.Env
	fmt.Fprintf(&b, "Receipt extraction accuracy (synthetic receipts; text through OCR, redaction, the model and the validator)\n")
	fmt.Fprintf(&b, "started %s, commit %s (working tree dirty: %v), ran %.0f s\n", rep.StartedAt.Format(time.RFC3339), e.Commit, e.Dirty, rep.DurationS)
	fmt.Fprintf(&b, "machine: %s, %d CPUs, %s bytes RAM, %s, %s; %s; load average at start: %s\n", e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.GoVersion, orDefault(e.Tesseract, "no Tesseract (no OCR variant)"), e.LoadAvg)
	fmt.Fprintf(&b, "model requested %s via %s, max tokens %d; real service: %v\n", e.Model, e.BaseURL, e.MaxTokens, rep.Params.RealModel)
	if e.PromptVersion != "" {
		fmt.Fprintf(&b, "system prompt %s (sha256 %s)\n", e.PromptVersion, e.PromptSHA256)
	}
	fmt.Fprintf(&b, "receipts %d..%d in each of %d variants = %d calls planned, %d in parallel, up to %d attempts each\n\n",
		rep.Params.FirstRef, rep.Params.FirstRef+rep.Params.Receipts-1, len(rep.Params.Variants), rep.Params.Calls, rep.Params.Workers, rep.Params.Attempts)
	for _, v := range rep.Variants {
		t := v.Tally
		fmt.Fprintf(&b, "== %s: %s\n", v.Name, v.Description)
		fmt.Fprintf(&b, "   calls answered %d, failed %d (calls that needed a retry %d, attempts %d); answers not valid for the schema %d\n", t.Calls, t.CallsFailed, t.Retried, t.AttemptsTotal, t.NotParsed)
		fmt.Fprintf(&b, "   prices of all units right (amounts exact) %s\n", pct(t.AmountsExact))
		fmt.Fprintf(&b, "   also descriptions and printed total (exact)  %s\n", pct(t.Exact))
		fmt.Fprintf(&b, "   printed total read right                      %s\n", pct(t.TotalRead))
		fmt.Fprintf(&b, "   what the system did with the answers:\n")
		fmt.Fprintf(&b, "     accepted (claiming) and correct             %3d\n", t.AcceptedCorrect)
		fmt.Fprintf(&b, "     accepted (claiming) and WRONG (silent)      %3d   silent-error rate %s\n", t.AcceptedWrong, pct(t.SilentErrorRate))
		fmt.Fprintf(&b, "     flagged (needs_review) and wrong (caught)   %3d   catch rate of wrong parses %s\n", t.FlaggedWrong, pct(t.CatchRate))
		fmt.Fprintf(&b, "     flagged (needs_review) but correct          %3d   false-alarm rate of correct parses %s\n", t.FlaggedCorrect, pct(t.FalseAlarmRate))
		fmt.Fprintf(&b, "   answers with an amount NOT printed in the text sent (guessed or invented) %s; of those %d accepted, %d wrong; %d such amounts in all\n", pct(t.AmountNotInText), t.AmountNotInTextAccepted, t.AmountNotInTextWrong, t.AmountsNotInTextTotal)
		fmt.Fprintf(&b, "   units: strict precision %.3f recall %.3f; on price alone precision %.3f recall %.3f\n", t.UnitPrecision, t.UnitRecall, t.AmountPrec, t.AmountRecall)
		fmt.Fprintf(&b, "   review reasons: %v\n", t.Reasons)
		fmt.Fprintf(&b, "   tokens: input %d (mean %.0f), output %d (mean %.0f); latency of an answered attempt p50 %d ms, p99 %d ms\n\n", t.InputTokens, t.MeanInput, t.OutputTokens, t.MeanOutput, t.LatencyP50ms, t.LatencyP99ms)
	}
	b.WriteString("What this does and does not say:\n")
	for _, s := range rep.Statements {
		fmt.Fprintf(&b, " - %s\n", s)
	}
	return b.String()
}
