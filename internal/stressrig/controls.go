//go:build !unsafe_controls

package stressrig

import "fmt"

// UnsafeControlsBuilt reports whether this binary can switch protections off. Never in
// this build.
const UnsafeControlsBuilt = false

// applyControls refuses: a production build cannot disable a protection, and a run that
// asked for it must not silently go ahead with everything on.
func applyControls(names []string) (workerEnv []string, undo func(), err error) {
	if len(names) > 0 {
		return nil, func() {}, fmt.Errorf("controls %v requested, but this binary was not built with the unsafe_controls tag", names)
	}
	return nil, func() {}, nil
}
