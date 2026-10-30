package benchwork_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/benchwork"
	"github.com/riyne/fifty-fifty/internal/mockllm"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

func buildWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "benchworker")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/riyne/fifty-fifty/cmd/benchworker").CombinedOutput()
	if err != nil {
		t.Fatalf("build benchworker: %v\n%s", err, out)
	}
	return bin
}

// kill -9 of a real worker process while it holds jobs: the others must finish every
// job, each exactly once. Two kill points: while the victim's jobs wait on the LLM,
// and while an effect row sits in a transaction the victim never commits.
func TestKillNineRecovery(t *testing.T) {
	testutil.Pool(t) // skips without TEST_DATABASE_URL
	testutil.OwnerPool(t)
	bin := buildWorker(t)

	const lease, poll = 2 * time.Second, 100 * time.Millisecond
	mock := mockllm.Config{LatencyMedian: 400 * time.Millisecond, LatencySigma: 0.3, Seed: 1}
	for _, tc := range []struct {
		name        string
		kill        benchwork.KillPoint
		commitDelay time.Duration
	}{
		{"during the LLM call", benchwork.KillDuringCall, 0},
		{"during the completion transaction", benchwork.KillDuringCommit, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := benchwork.RunRecovery(context.Background(), benchwork.RecoveryParams{
				DatabaseURL: os.Getenv("TEST_DATABASE_URL"), OwnerURL: os.Getenv("TEST_OWNER_DATABASE_URL"),
				WorkerBin: bin, Workers: 3, Concurrency: 3, Jobs: 30,
				Lease: lease, Poll: poll, CommitDelay: tc.commitDelay, Kill: tc.kill, Mock: mock,
				DrainTimeout: 30 * time.Second, Logf: t.Logf,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%+v", res)

			if res.KilledBySignal != "SIGKILL" {
				t.Errorf("victim was not killed by SIGKILL: %q", res.KilledBySignal)
			}
			if res.LostJobs != 0 || res.Succeeded != res.Jobs || res.Dead != 0 || res.Unfinished != 0 {
				t.Errorf("lost jobs: succeeded %d dead %d unfinished %d of %d", res.Succeeded, res.Dead, res.Unfinished, res.Jobs)
			}
			if res.DuplicateFx != 0 || res.MissingFx != 0 {
				t.Errorf("effects: %d duplicates, %d missing; want exactly one per job", res.DuplicateFx, res.MissingFx)
			}
			if res.Orphans == 0 {
				t.Fatal("the victim left no orphaned job, so nothing was tested")
			}
			// An orphan is picked up when its lease runs out and a survivor polls, then
			// runs its LLM call (up to ~1.3s at these settings) and commit.
			bound := (lease + poll + 2*time.Second + tc.commitDelay + 3*time.Second).Seconds()
			for i, secs := range res.RecoverySecs {
				if secs < 0 || secs > bound {
					t.Errorf("orphan %d recovered %.2fs after the kill, want within (0, %.1f]", i, secs, bound)
				}
				if res.OrphanAttempts[i] < 2 {
					t.Errorf("orphan %d has %d attempts; a recovered job ran at least twice", i, res.OrphanAttempts[i])
				}
			}
			if res.ExtraAttempts < res.Orphans {
				t.Errorf("%d extra attempts for %d orphans", res.ExtraAttempts, res.Orphans)
			}
		})
	}
}
