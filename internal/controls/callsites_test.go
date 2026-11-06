package controls_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCallSitesGoThroughControls fails if production code calls one of the wrapped
// queries directly. A direct call would still work, but a negative-control run could
// not switch that lock off, and the run would report a protection as disabled when it
// was not. Tests and the controls and db packages themselves may call them.
func TestCallSitesGoThroughControls(t *testing.T) {
	wrapped := []string{
		"LockExpenseForClaim(", "LockExpenseForFinalize(", "LockExpenseForVoid(", "LockExpenseForEdit(",
		"LockGroupForSettlement(", "LockSettlement(",
		"MarkSettlementSucceeded(", "MarkSettlementFailed(", "MarkSettlementUnknown(",
	}
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "internal/controls" || rel == "internal/db" || rel == ".git" || rel == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, w := range wrapped {
				if strings.Contains(line, "."+w) && !strings.Contains(line, "controls."+w) {
					t.Errorf("%s:%d calls %s directly; go through internal/controls", rel, i+1, strings.TrimSuffix(w, "("))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
