//go:build unsafe_controls

// This is the negative-control build. It must never be shipped: it can switch off the
// locks and guards that keep the ledger correct. See controls.go for the production
// build and why this file cannot end up in it.
package controls

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/db"
)

// EnvVar lists controls to switch off at process start, comma separated. It is read
// only in this build. It exists so a harness can start a real worker process with
// the same protections off as its in-process api.
const EnvVar = "FIFTY_UNSAFE_CONTROLS"

// off holds one bit per control, in the order of All.
var off atomic.Uint64

func bit(c Control) uint64 {
	for i, x := range All {
		if x == c {
			return 1 << i
		}
	}
	panic(fmt.Sprintf("controls: unknown control %q", c))
}

// Disabled reports whether c is switched off.
func Disabled(c Control) bool { return off.Load()&bit(c) != 0 }

// Disable switches the given controls off.
func Disable(cs ...Control) {
	for _, c := range cs {
		off.Or(bit(c))
	}
}

// Enable switches the given controls back on.
func Enable(cs ...Control) {
	for _, c := range cs {
		off.And(^bit(c))
	}
}

// Reset switches every control back on.
func Reset() { off.Store(0) }

// Active lists the controls that are currently off, in the order of All.
func Active() []Control {
	var out []Control
	for _, c := range All {
		if Disabled(c) {
			out = append(out, c)
		}
	}
	return out
}

// Parse reads a comma-separated list of control names. An unknown name is an error,
// so a typo cannot silently leave a protection on in a run that claims it was off.
func Parse(s string) ([]Control, error) {
	var out []Control
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		for _, c := range All {
			if string(c) == name {
				out, found = append(out, c), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("controls: unknown control %q", name)
		}
	}
	return out, nil
}

func init() {
	cs, err := Parse(os.Getenv(EnvVar))
	if err != nil {
		log.Fatalf("%s: %v", EnvVar, err)
	}
	Disable(cs...)
	if len(cs) > 0 {
		log.Printf("WARNING: unsafe_controls build with protections OFF: %v", cs)
	}
}

func LockExpenseForClaim(ctx context.Context, q *db.Queries, arg db.LockExpenseForClaimParams) (string, error) {
	if Disabled(ClaimLock) {
		return q.UnsafeLockExpenseForClaim(ctx, arg)
	}
	return q.LockExpenseForClaim(ctx, arg)
}

func LockExpenseForFinalize(ctx context.Context, q *db.Queries, arg db.LockExpenseForFinalizeParams) (db.LockExpenseForFinalizeRow, error) {
	if Disabled(FinalizeLock) {
		return q.UnsafeLockExpenseForFinalize(ctx, arg)
	}
	return q.LockExpenseForFinalize(ctx, arg)
}

func LockExpenseForVoid(ctx context.Context, q *db.Queries, arg db.LockExpenseForVoidParams) (string, error) {
	if Disabled(VoidLock) {
		return q.UnsafeLockExpenseForVoid(ctx, arg)
	}
	return q.LockExpenseForVoid(ctx, arg)
}

func LockGroupForSettlement(ctx context.Context, q *db.Queries, id uuid.UUID) (uuid.UUID, error) {
	if Disabled(GroupLock) {
		return q.UnsafeLockGroupForSettlement(ctx, id)
	}
	return q.LockGroupForSettlement(ctx, id)
}

func LockSettlement(ctx context.Context, q *db.Queries, id uuid.UUID) (db.LockSettlementRow, error) {
	if Disabled(ResolveLock) {
		return q.UnsafeLockSettlement(ctx, id)
	}
	return q.LockSettlement(ctx, id)
}

func MarkSettlementSucceeded(ctx context.Context, q *db.Queries, arg db.MarkSettlementSucceededParams) (int64, error) {
	if Disabled(ResolveGuards) {
		return q.UnsafeMarkSettlementSucceeded(ctx, arg)
	}
	return q.MarkSettlementSucceeded(ctx, arg)
}

func MarkSettlementFailed(ctx context.Context, q *db.Queries, arg db.MarkSettlementFailedParams) (int64, error) {
	if Disabled(ResolveGuards) {
		return q.UnsafeMarkSettlementFailed(ctx, arg)
	}
	return q.MarkSettlementFailed(ctx, arg)
}

func MarkSettlementUnknown(ctx context.Context, q *db.Queries, id uuid.UUID) (int64, error) {
	if Disabled(ResolveGuards) {
		return q.UnsafeMarkSettlementUnknown(ctx, id)
	}
	return q.MarkSettlementUnknown(ctx, id)
}
