//go:build unsafe_controls

package stressrig

import (
	"strings"

	"github.com/riyne/fifty-fifty/internal/controls"
)

// UnsafeControlsBuilt reports whether this binary can switch protections off.
const UnsafeControlsBuilt = true

// applyControls switches the named protections off in this process (the api and the
// webhook handler run in it) and returns the environment that does the same in the
// worker processes, which must be built with the tag too. undo switches everything back on.
func applyControls(names []string) (workerEnv []string, undo func(), err error) {
	cs, err := controls.Parse(strings.Join(names, ","))
	if err != nil {
		return nil, func() {}, err
	}
	controls.Disable(cs...)
	if len(cs) == 0 {
		return nil, controls.Reset, nil
	}
	return []string{controls.EnvVar + "=" + strings.Join(names, ",")}, controls.Reset, nil
}
