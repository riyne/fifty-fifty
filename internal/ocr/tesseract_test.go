package ocr_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/ocr"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

var ctx = context.Background()

// stub writes an executable shell script and returns its path: a stand-in for tesseract
// that lets the tests see exactly how the program is run.
func stub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tesseract")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func newStub(t *testing.T, body string) *ocr.Tesseract {
	t.Helper()
	o, err := ocr.New(ocr.Config{Path: stub(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestText_RunsTheProgramWithFixedArgumentsAndAMinimalEnvironment(t *testing.T) {
	// Print the arguments, one per line, then the environment.
	o := newStub(t, `printf '%s\n' "$@"; echo ---; /usr/bin/env | sort`)
	out, err := o.Text(ctx, []byte("image bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	args, env, _ := strings.Cut(out, "---\n")
	if args != "stdin\nstdout\n-l\neng\n--psm\n6\n" {
		t.Errorf("arguments:\n%s", args)
	}
	for _, line := range strings.Split(strings.TrimSpace(env), "\n") {
		if line != "OMP_THREAD_LIMIT=1" && line != "LC_ALL=C" && !strings.HasPrefix(line, "PWD=") && !strings.HasPrefix(line, "SHLVL=") && !strings.HasPrefix(line, "_=") {
			t.Errorf("unexpected environment variable %q", line)
		}
	}
	if !strings.Contains(env, "OMP_THREAD_LIMIT=1") {
		t.Error("OMP_THREAD_LIMIT is not set")
	}
}

func TestText_TheImageGoesInOnStdinAndTheTextComesBackWhole(t *testing.T) {
	// Echo the number of bytes and a checksum of what arrived on stdin.
	o := newStub(t, `n=$(wc -c | tr -d ' '); echo "got $n bytes"; echo "line two"`)
	img := bytes.Repeat([]byte{0, 1, 2, 255}, 100_000)
	out, err := o.Text(ctx, img, "image/jpeg")
	if err != nil || out != "got 400000 bytes\nline two\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := o.Text(ctx, nil, ""); err == nil {
		t.Error("an empty image was accepted")
	}
}

func TestText_Failures(t *testing.T) {
	// A non-zero exit: the error carries the exit status and tesseract's own stderr, and nothing else.
	o := newStub(t, `cat >/dev/null; echo "Error: could not read image" >&2; echo "partial text"; exit 1`)
	_, err := o.Text(ctx, []byte("x"), "")
	if err == nil || !strings.Contains(err.Error(), "could not read image") || strings.Contains(err.Error(), "partial text") {
		t.Errorf("err = %v", err)
	}
	// A long stderr is cut.
	o = newStub(t, `cat >/dev/null; head -c 100000 /dev/zero | tr '\0' 'e' >&2; exit 2`)
	if _, err := o.Text(ctx, []byte("x"), ""); err == nil || len(err.Error()) > 700 {
		t.Errorf("err length %d", len(errString(err)))
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestText_AnEndlessOutputIsCutAndTheProcessKilled(t *testing.T) {
	o := newStub(t, `cat >/dev/null; exec yes "lorem ipsum dolor sit amet"`)
	start := time.Now()
	_, err := o.Text(ctx, []byte("x"), "")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestText_TheContextKillsTheProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// Record the pid, then become `sleep` (exec: no shell left behind, so no child can hold
	// the output pipe open and delay Wait).
	o := newStub(t, `cat >/dev/null; echo $$ > `+pidFile+`; exec sleep 30`)
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := o.Text(cctx, []byte("x"), "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("returned after %v, want shortly after the 300 ms deadline", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the stub never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil { // still there (a zombie is reaped by Wait, so this means running)
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d was still running after the context ended", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cctx2, cancel2 := context.WithCancel(ctx)
	cancel2()
	if _, err := o.Text(cctx2, []byte("x"), ""); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := ocr.New(ocr.Config{Path: filepath.Join(t.TempDir(), "no-such-program")}); !errors.Is(err, ocr.ErrNotInstalled) {
		t.Errorf("missing program: %v", err)
	}
	ok := stub(t, "exit 0")
	for name, cfg := range map[string]ocr.Config{
		"lang with a space":   {Path: ok, Lang: "en g"},
		"lang with a slash":   {Path: ok, Lang: "../eng"},
		"lang with an option": {Path: ok, Lang: "eng;rm"},
		"psm too large":       {Path: ok, PSM: 14},
		"psm negative":        {Path: ok, PSM: -1},
	} {
		if _, err := ocr.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, cfg := range []ocr.Config{{Path: ok}, {Path: ok, Lang: "eng+osd", PSM: 4}, {Path: ok, Lang: "deu_frak", PSM: 13}} {
		if _, err := ocr.New(cfg); err != nil {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
	o, _ := ocr.New(ocr.Config{Path: stub(t, `echo "tesseract 9.9.9"; echo "second line"`)})
	if v, err := o.Version(ctx); err != nil || v != "tesseract 9.9.9" {
		t.Errorf("version %q %v", v, err)
	}
}

func TestNew_UsesTheConfiguredPSMAndLanguage(t *testing.T) {
	p := stub(t, `printf '%s\n' "$@" | tr '\n' ' '`)
	o, _ := ocr.New(ocr.Config{Path: p, Lang: "eng+osd", PSM: 4})
	out, err := o.Text(ctx, []byte("x"), "")
	if err != nil || strings.TrimSpace(out) != "stdin stdout -l eng+osd --psm 4" {
		t.Errorf("%q %v", out, err)
	}
}

// ---- the real program ----

func real(t *testing.T) *ocr.Tesseract {
	t.Helper()
	o, err := ocr.New(ocr.Config{})
	if errors.Is(err, ocr.ErrNotInstalled) {
		if os.Getenv("REQUIRE_TESSERACT") == "1" {
			t.Fatalf("REQUIRE_TESSERACT=1 but tesseract is not installed: %v", err)
		}
		t.Skip("tesseract is not installed (set REQUIRE_TESSERACT=1 to make that a failure)")
	}
	if err != nil {
		t.Fatal(err)
	}
	return o
}

var spaces = regexp.MustCompile(`\s+`)

// What the pipeline needs from OCR on a clean, upright rendering of a synthetic receipt:
// the REF line (the mock LLM's fixture key), and the printed total. The thresholds are
// loose on purpose: Tesseract's versions differ, and these tests are a guard that the
// program is wired up and reads the images we render, not a measurement of its accuracy.
func TestRealTesseract_ReadsRenderedReceipts(t *testing.T) {
	o := real(t)
	if v, err := o.Version(ctx); err != nil || !strings.HasPrefix(v, "tesseract") {
		t.Fatalf("version %q %v", v, err)
	}
	const n = 24
	var refOK, totalOK, itemsOK, itemsAll int
	for ref := 1; ref <= n; ref++ {
		truth := synth.Generate(ref)
		img, err := truth.Render(synth.RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		text, err := o.Text(ctx, img, "image/png")
		if err != nil {
			t.Fatalf("ref %d: %v", ref, err)
		}
		flat := spaces.ReplaceAllString(text, " ")
		if strings.Contains(flat, "REF "+strconv.Itoa(ref)) {
			refOK++
		}
		if strings.Contains(flat, money(truth.TotalMinor)) {
			totalOK++
		}
		for _, l := range truth.Lines {
			itemsAll++
			if strings.Contains(flat, money(l.TotalMinor)) && strings.Contains(flat, l.Description) {
				itemsOK++
			}
		}
	}
	t.Logf("clean renders, %d receipts: REF line read %d, total read %d, item lines read %d of %d", n, refOK, totalOK, itemsOK, itemsAll)
	if refOK < n*9/10 || totalOK < n*9/10 || itemsOK < itemsAll*9/10 {
		t.Errorf("OCR is not reading clean renders (REF %d/%d, total %d/%d, items %d/%d)", refOK, n, totalOK, n, itemsOK, itemsAll)
	}
}

func money(minor int64) string { return strconv.FormatInt(minor/100, 10) + "." + pad2(minor%100) }

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

func TestRealTesseract_ABlankImageGivesNoTextNotAnError(t *testing.T) {
	o := real(t)
	// A white page: Tesseract prints nothing and exits 0.
	img := whitePNG(t)
	text, err := o.Text(ctx, img, "image/png")
	if err != nil || strings.TrimSpace(text) != "" {
		t.Errorf("%q %v", text, err)
	}
}

func TestRealTesseract_GarbageIsAnErrorNotAHang(t *testing.T) {
	o := real(t)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := o.Text(cctx, []byte("this is not an image"), ""); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestRealTesseract_ConcurrentCallsAreIndependent(t *testing.T) {
	o := real(t)
	errs := make(chan error, 8)
	for i := 1; i <= 8; i++ {
		go func() {
			img, _ := synth.Generate(i).Render(synth.RenderOptions{})
			text, err := o.Text(ctx, img, "image/png")
			if err == nil && !strings.Contains(spaces.ReplaceAllString(text, " "), "REF "+strconv.Itoa(i)) {
				err = errors.New("receipt " + strconv.Itoa(i) + ": its REF line is missing from its own text")
			}
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
