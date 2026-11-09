//go:build unsafe_controls

package stressrig

import (
	"testing"

	"github.com/riyne/fifty-fifty/internal/controls"
)

func TestApplyControls(t *testing.T) {
	env, undo, err := applyControls([]string{"claim_lock", "resolve_lock"})
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != "FIFTY_UNSAFE_CONTROLS=claim_lock,resolve_lock" {
		t.Fatalf("worker environment = %v", env)
	}
	if !controls.Disabled(controls.ClaimLock) || !controls.Disabled(controls.ResolveLock) || controls.Disabled(controls.GroupLock) {
		t.Fatalf("in-process controls off: %v", controls.Active())
	}
	undo()
	if len(controls.Active()) != 0 {
		t.Fatalf("undo left controls off: %v", controls.Active())
	}
	// A misspelled name must be an error, never "everything stayed on".
	if _, undo, err := applyControls([]string{"claim_lok"}); err == nil {
		t.Fatal("a misspelled control was accepted")
	} else {
		undo()
	}
}
