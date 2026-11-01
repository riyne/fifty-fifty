package controls_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the test file's directory")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// build compiles ./cmd/<name> into a temp dir and returns the binary's bytes and the
// symbol names `go tool nm` finds in it.
func build(t *testing.T, name string, tags string) (bin []byte, symbols []string) {
	t.Helper()
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-o", out}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	cmd := exec.Command("go", append(args, "./cmd/"+name)...)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, b)
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	nm := exec.Command("go", "tool", "nm", out)
	nm.Dir = root
	b, err := nm.Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 3 {
			symbols = append(symbols, f[2])
		}
	}
	return bin, symbols
}

// unsafeMarkers are things that exist only in an unsafe_controls build: the control
// switches, the hand-written unsafe queries, and the environment variable name.
func findUnsafe(bin []byte, symbols []string) []string {
	var found []string
	for _, s := range symbols {
		switch {
		case strings.HasSuffix(s, "internal/controls.Disable"),
			strings.HasSuffix(s, "internal/controls.Enable"),
			strings.HasSuffix(s, "internal/controls.Reset"),
			strings.Contains(s, ").UnsafeLock"),
			strings.Contains(s, ").UnsafeMark"):
			found = append(found, s)
		}
	}
	if bytes.Contains(bin, []byte("FIFTY_UNSAFE_CONTROLS")) {
		found = append(found, "string FIFTY_UNSAFE_CONTROLS")
	}
	return found
}

// TestProductionBinariesHaveNoControls is the proof behind "no runtime flag in the
// production binary": it builds the api and the worker the way `go build` does by
// default and finds none of the negative-control code in them. The second half builds
// the worker with the tag and requires the same search to find it, so the first half
// cannot pass just because the search looks for the wrong names.
func TestProductionBinariesHaveNoControls(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	for _, name := range []string{"api", "worker"} {
		bin, symbols := build(t, name, "")
		if found := findUnsafe(bin, symbols); len(found) > 0 {
			t.Errorf("production %s contains negative-control code: %v", name, found)
		}
	}
	bin, symbols := build(t, "worker", "unsafe_controls")
	if found := findUnsafe(bin, symbols); len(found) < 5 {
		t.Errorf("the unsafe_controls worker should contain the control code, found only %v (the search is broken)", found)
	}
}
