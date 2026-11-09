// Package ocr reads text from a receipt image by running the Tesseract command-line
// program. It implements receiptparse.OCR.
//
// How it is run, and why:
//
//   - The image goes in on stdin and the text comes out on stdout (`tesseract stdin stdout`),
//     so no temporary file with receipt content is ever written.
//   - No shell: the program and its arguments are fixed, and nothing from the image or the
//     request is ever part of the command line.
//   - The image has already been hardened (re-encoded from its pixels by
//     receipts.HardenImage), so Tesseract's image readers only ever see an image this
//     program wrote. That is the main defence against a hostile file; the process is also
//     killed when the context ends, its output is bounded, and its environment is minimal.
//   - Page segmentation mode 6 ("a single uniform block of text"): chosen over the default
//     automatic mode because the automatic mode split price columns away from their item
//     lines on the synthetic receipts, which is the one thing the extractor needs kept
//     together. It was chosen by looking at a handful of
//     synthetic receipts, not by a measurement, and real receipts may prefer another mode.
//   - One OpenMP thread per process: the worker already runs several jobs at once, and an
//     OCR process that spreads over every core makes their timing unpredictable.
//
// Errors never include the image or the recognized text; Tesseract's own stderr (a few
// lines about the image format or a missing language file) is kept, truncated.
package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// MaxOutputBytes bounds what is read from Tesseract's stdout. A receipt is a few
// hundred characters; an image of noise can make Tesseract print pages of nonsense.
const MaxOutputBytes = 1 << 20

// Config configures a Tesseract.
type Config struct {
	// Path is the Tesseract executable; empty means "tesseract" on the PATH.
	Path string
	// Lang is the Tesseract language; empty means "eng".
	Lang string
	// PSM is the page segmentation mode; 0 means 6.
	PSM int
}

// Tesseract runs the tesseract program. Safe for concurrent use.
type Tesseract struct {
	path string
	lang string
	psm  string
}

// ErrNotInstalled means the tesseract program was not found.
var ErrNotInstalled = errors.New("ocr: tesseract is not installed (or not on PATH)")

// New finds the program and returns a Tesseract. It fails if the program is missing, so a
// worker that cannot read receipts says so at startup, not on the first one.
func New(cfg Config) (*Tesseract, error) {
	path := cfg.Path
	if path == "" {
		path = "tesseract"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotInstalled, err)
	}
	lang := cfg.Lang
	if lang == "" {
		lang = "eng"
	}
	for _, c := range lang {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '+') {
			return nil, errors.New("ocr: language must be letters, '_' or '+'")
		}
	}
	psm := cfg.PSM
	if psm == 0 {
		psm = 6
	}
	if psm < 0 || psm > 13 {
		return nil, errors.New("ocr: page segmentation mode must be 0-13")
	}
	return &Tesseract{path: resolved, lang: lang, psm: fmt.Sprint(psm)}, nil
}

// Version is the first line of `tesseract --version`, for recording what produced a result.
func (t *Tesseract) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ocr: tesseract --version: %w", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

// Text implements receiptparse.OCR. contentType is not needed: Tesseract's image library
// recognizes the format from the bytes.
func (t *Tesseract) Text(ctx context.Context, image []byte, _ string) (string, error) {
	if len(image) == 0 {
		return "", errors.New("ocr: empty image")
	}
	cmd := exec.CommandContext(ctx, t.path, "stdin", "stdout", "-l", t.lang, "--psm", t.psm)
	cmd.Stdin = bytes.NewReader(image)
	// A minimal environment: no inherited variables that could change where Tesseract
	// looks for data or how many threads it uses.
	cmd.Env = []string{"OMP_THREAD_LIMIT=1", "LC_ALL=C"}
	cmd.WaitDelay = 2 * time.Second // after the context ends: kill, then give up on pipes
	var stderr limitedBuffer
	stderr.max = 512
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("ocr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("ocr: start tesseract: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, MaxOutputBytes+1))
	if len(out) > MaxOutputBytes {
		_ = cmd.Process.Kill() // it is printing without end
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return "", fmt.Errorf("ocr: %w", ctx.Err())
	case len(out) > MaxOutputBytes:
		return "", errors.New("ocr: output is too large")
	case readErr != nil:
		return "", fmt.Errorf("ocr: read output: %w", readErr)
	case waitErr != nil:
		return "", fmt.Errorf("ocr: tesseract failed: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
