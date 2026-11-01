//go:build unsafe_controls

package e2e_test

import (
	"os"
	"sync"
	"testing"

	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/stressrig"
)

// These are the negative controls at smoke size (make test-controls): the stress workload
// again, with one protection switched off, in the api and in the workers, which are built
// with the unsafe_controls tag. The cases and what each must show are
// stressrig.ControlCases (the bench/stress program holds its recorded runs to the same).
// Settlement is simulated: the provider is a mock with simulated latency of 100 ms.

var (
	unsafeBinsOnce sync.Once
	unsafeBins     settlerig.Bins
	unsafeBinsErr  error
)

func tagged(t *testing.T) settlerig.Bins {
	t.Helper()
	unsafeBinsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fifty-unsafe-bin-")
		if err != nil {
			unsafeBinsErr = err
			return
		}
		unsafeBins, unsafeBinsErr = settlerig.BuildWithTags(ctx, dir, "unsafe_controls")
	})
	if unsafeBinsErr != nil {
		t.Fatal(unsafeBinsErr)
	}
	return unsafeBins
}

func TestStressControls(t *testing.T) {
	if !enabled {
		t.Skip("set E2E=1")
	}
	for _, c := range stressrig.ControlCases {
		t.Run(c.Name, func(t *testing.T) {
			p := c.Apply(stressParams(t))
			p.Bins = tagged(t)
			res, err := stressrig.Run(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("controls off: %v, posting index dropped: %v\n%s", c.Off, c.DropIndex, res.Summary())
			for _, problem := range c.Problems(res, p) {
				t.Error(problem)
			}
		})
	}
}
