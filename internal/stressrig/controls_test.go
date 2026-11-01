package stressrig

import "testing"

// A production build must refuse a run that asks for a protection to be off, rather
// than run with everything on and report it as a negative control.
func TestApplyControlsRefusedWithoutTag(t *testing.T) {
	if UnsafeControlsBuilt {
		t.Skip("this is the unsafe_controls build")
	}
	if _, _, err := applyControls([]string{"claim_lock"}); err == nil {
		t.Fatal("a build without the tag accepted a control")
	}
	env, undo, err := applyControls(nil)
	if err != nil || len(env) != 0 {
		t.Fatalf("no controls: env %v err %v", env, err)
	}
	undo()
}
