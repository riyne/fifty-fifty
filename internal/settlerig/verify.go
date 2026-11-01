package settlerig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/riyne/fifty-fifty/internal/mockpay"
)

// Querier is the part of a connection pool (or a transaction) that Verify reads through.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Report is what Verify found for a set of settlements.
type Report struct {
	Settlements int
	ByStatus    map[string]int
	// Postings is the number of settlement postings on the ledger for these settlements.
	Postings int
	// ProviderCharges is the number of succeeded payments the provider holds for them.
	ProviderCharges int
	// InFlightCharged counts settlements the provider charged that are not final yet
	// (pending or unknown): not an anomaly while they are being resolved, a lost
	// payment if they stay that way.
	InFlightCharged int
	// Anomalies is empty when every invariant held.
	Anomalies []string
}

// FetchProvider reads the mock provider's own record of every payment it holds. This is
// the ground truth the ledger is compared with: it is what the provider thinks it took.
func FetchProvider(ctx context.Context, baseURL string) ([]mockpay.Ledger, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/debug/payments", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Payments []mockpay.Ledger `json:"payments"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("provider /debug/payments: %v: %s", err, b)
	}
	return out.Payments, nil
}

// Verify checks the invariants that settlement must keep, for the given settlements,
// against the provider's payments (from FetchProvider):
//
//   - a succeeded settlement has exactly one ledger posting and the provider holds a
//     succeeded payment for it; any other status has none;
//   - the provider holds at most one payment per settlement (no double create), and at
//     most one succeeded one (no double charge);
//   - a settlement recorded as failed was not charged by the provider (money taken
//     while the app says it was not);
//   - the payment's amount, currency, sender and payee are the settlement's;
//   - every group's ledger sums to zero.
//
// It does not require the settlements to be final: ones still being resolved are counted
// in the report, and the caller decides whether that is acceptable.
func Verify(ctx context.Context, pool Querier, provider []mockpay.Ledger, ids []uuid.UUID) (Report, error) {
	rep := Report{ByStatus: map[string]int{}}
	byRef := map[string][]mockpay.Ledger{}
	for _, p := range provider {
		byRef[p.Reference] = append(byRef[p.Reference], p)
	}

	rows, err := pool.Query(ctx, `
		SELECT s.id, s.status, s.amount_minor, s.from_member::text, s.to_member::text, g.currency,
		       (SELECT count(*) FROM ledger_transactions t WHERE t.settlement_id = s.id)::int
		FROM settlements s JOIN groups g ON g.id = s.group_id
		WHERE s.id = ANY($1) ORDER BY s.id`, ids)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	bad := func(format string, args ...any) { rep.Anomalies = append(rep.Anomalies, fmt.Sprintf(format, args...)) }

	for rows.Next() {
		var (
			id                    uuid.UUID
			status, from, to, cur string
			amount                int64
			postings              int
		)
		if err := rows.Scan(&id, &status, &amount, &from, &to, &cur, &postings); err != nil {
			return rep, err
		}
		rep.Settlements++
		rep.ByStatus[status]++
		rep.Postings += postings
		pays := byRef[id.String()]

		succeeded := 0
		for _, p := range pays {
			if p.Status == mockpay.StatusSucceeded {
				succeeded++
			}
			if p.AmountMinor != amount || p.Currency != cur || p.From != from || p.To != to {
				bad("settlement %s: the provider's payment differs from it (amount %d vs %d, currency %s vs %s)", id, p.AmountMinor, amount, p.Currency, cur)
			}
		}
		rep.ProviderCharges += succeeded

		switch {
		case postings > 1:
			bad("settlement %s has %d ledger postings", id, postings)
		case status == "succeeded" && postings != 1:
			bad("settlement %s is succeeded but has %d ledger postings", id, postings)
		case status != "succeeded" && postings != 0:
			bad("settlement %s is %s but has %d ledger postings", id, status, postings)
		}
		if len(pays) > 1 {
			bad("settlement %s: the provider holds %d payments for it (a double create)", id, len(pays))
		}
		if succeeded > 1 {
			bad("settlement %s: the provider charged it %d times", id, succeeded)
		}
		switch {
		case status == "succeeded" && succeeded != 1:
			bad("settlement %s is succeeded but the provider holds %d succeeded payments for it", id, succeeded)
		case status == "failed" && succeeded > 0:
			bad("settlement %s is failed but the provider charged it", id)
		case (status == "pending" || status == "unknown") && succeeded > 0:
			rep.InFlightCharged++
		}
	}
	if err := rows.Err(); err != nil {
		return rep, err
	}

	// Every group's ledger must sum to zero (the database enforces it per posting; this
	// is the same property read from the other side).
	grows, err := pool.Query(ctx, `
		SELECT e.group_id::text, SUM(e.amount_minor)::text FROM ledger_entries e
		WHERE e.group_id IN (SELECT group_id FROM settlements WHERE id = ANY($1))
		GROUP BY e.group_id HAVING SUM(e.amount_minor) <> 0`, ids)
	if err != nil {
		return rep, err
	}
	defer grows.Close()
	for grows.Next() {
		var g, sum string
		if err := grows.Scan(&g, &sum); err != nil {
			return rep, err
		}
		bad("group %s: the ledger sums to %s, not zero", g, sum)
	}
	sort.Strings(rep.Anomalies)
	return rep, grows.Err()
}

// Unresolved is how many of the settlements are not final (pending or unknown).
func (r Report) Unresolved() int { return r.ByStatus["pending"] + r.ByStatus["unknown"] }

// WaitResolved polls until none of the settlements is pending or unknown, or the timeout
// passes. It returns how many were still unresolved.
func WaitResolved(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM settlements WHERE id = ANY($1) AND status IN ('pending', 'unknown')`, ids).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 || time.Now().After(deadline) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
