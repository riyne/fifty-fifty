package controls

// Control names one protection that a negative-control build can switch off.
// The names exist in every build, because the stress harness and the tests refer
// to them; only a build with the unsafe_controls tag can act on them.
type Control string

const (
	// ClaimLock: the FOR SHARE lock a claim or unclaim takes on the expense row.
	ClaimLock Control = "claim_lock"
	// FinalizeLock: the FOR UPDATE lock finalize takes on the expense row.
	FinalizeLock Control = "finalize_lock"
	// VoidLock: the FOR UPDATE lock void takes on the expense row.
	VoidLock Control = "void_lock"
	// IdempotencyCheck: claiming the Idempotency-Key row, which makes a retry
	// replay the stored response instead of running again.
	IdempotencyCheck Control = "idempotency_check"
	// GroupLock: the FOR NO KEY UPDATE lock settlement creation takes on the
	// group row, which serializes the capacity check.
	GroupLock Control = "group_lock"
	// ResolveLock: the FOR UPDATE lock settlements.Resolve takes on the settlement row.
	ResolveLock Control = "resolve_lock"
	// ResolveGuards: the status conditions on the three MarkSettlement* updates
	// (the layer behind ResolveLock; the unique posting index sits behind both).
	ResolveGuards Control = "resolve_guards"
)

// All lists every control. The order fixes each control's bit in the unsafe build.
var All = []Control{ClaimLock, FinalizeLock, VoidLock, IdempotencyCheck, GroupLock, ResolveLock, ResolveGuards}
