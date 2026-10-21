package groups_test

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/joincode"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

type codeInfo struct {
	ID        string     `json:"id"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	RevokedAt *time.Time `json:"revoked_at"`
	Status    string     `json:"status"`
}

func listCodes(t *testing.T, s *teststack.Stack, token, groupID string) []codeInfo {
	t.Helper()
	r := s.Do(t, token, "GET", "/v1/groups/"+groupID+"/join-codes", "")
	if r.Status != 200 {
		t.Fatalf("list codes = %d: %s", r.Status, r.Body)
	}
	var out struct {
		JoinCodes []codeInfo `json:"join_codes"`
	}
	r.JSON(t, &out)
	return out.JoinCodes
}

func codeByID(t *testing.T, codes []codeInfo, id string) codeInfo {
	t.Helper()
	for _, c := range codes {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("code %s not in list", id)
	return codeInfo{}
}

func TestCreateJoinCode_ReturnsPlaintextOnceAndStoresOnlyHash(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")

	id, code := s.CreateJoinCode(t, alice, gid, 0, 0)
	if !regexp.MustCompile(`^[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}$`).MatchString(code) {
		t.Fatalf("code %q is not in ABCD-EFGH-JKMN form", code)
	}

	norm, err := joincode.Normalize(code)
	if err != nil {
		t.Fatal(err)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM join_codes WHERE id = $1 AND code_hash = $2`, id, joincode.Hash(norm)); n != 1 {
		t.Fatal("stored hash does not match the issued code")
	}

	// Defaults applied, and no later response ever contains the plaintext.
	c := codeByID(t, listCodes(t, s, alice, gid), id)
	if c.MaxUses != 5 || c.Uses != 0 || c.Status != "active" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	raw := s.Do(t, alice, "GET", "/v1/groups/"+gid+"/join-codes", "")
	if strings.Contains(string(raw.Body), norm) || strings.Contains(string(raw.Body), code) {
		t.Fatalf("list response leaks the plaintext code: %s", raw.Body)
	}
	audit := s.Do(t, alice, "GET", "/v1/groups/"+gid, "")
	if strings.Contains(string(audit.Body), norm) {
		t.Fatal("group response leaks the plaintext code")
	}
	var data string
	if err := s.Pool.QueryRow(t.Context(), `SELECT data::text FROM audit_events WHERE group_id = $1 AND kind = 'join_code.created'`, gid).Scan(&data); err != nil {
		t.Fatalf("audit event missing: %v", err)
	}
	if strings.Contains(data, norm) {
		t.Fatalf("audit event leaks the plaintext code: %s", data)
	}
}

func TestCreateJoinCode_Validation(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")

	for name, body := range map[string]string{
		"zero uses":       `{"max_uses":0}`,
		"negative uses":   `{"max_uses":-1}`,
		"too many uses":   `{"max_uses":51}`,
		"zero ttl":        `{"expires_in_hours":0}`,
		"ttl over a week": `{"expires_in_hours":169}`,
		"unknown field":   `{"max_uses":2,"group_id":"x"}`,
		"string not int":  `{"max_uses":"5"}`,
		"not json":        `max_uses=5`,
	} {
		t.Run(name, func(t *testing.T) {
			if r := s.Do(t, alice, "POST", "/v1/groups/"+gid+"/join-codes", body); r.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", r.Status, r.Body)
			}
		})
	}
	// An entirely empty body means "all defaults".
	if r := s.Do(t, alice, "POST", "/v1/groups/"+gid+"/join-codes", ""); r.Status != http.StatusCreated {
		t.Fatalf("empty body = %d, want 201: %s", r.Status, r.Body)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM join_codes WHERE group_id = $1`, gid); n != 1 {
		t.Fatalf("rejected requests created codes: %d rows, want 1", n)
	}
}

func TestJoin_HappyPath(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 0, 0)

	r := s.Join(t, bob, code)
	if r.Status != http.StatusCreated {
		t.Fatalf("join = %d: %s", r.Status, r.Body)
	}
	var g struct{ ID, Name, Role string }
	r.JSON(t, &g)
	if g.ID != gid || g.Name != "Trip" || g.Role != "member" {
		t.Fatalf("join response = %+v", g)
	}

	// Bob can now see the group; both are members; one use was spent; one audit event.
	var detail struct {
		Members []struct{ Role string } `json:"members"`
	}
	s.Do(t, bob, "GET", "/v1/groups/"+gid, "").JSON(t, &detail)
	if len(detail.Members) != 2 {
		t.Fatalf("members = %+v, want 2", detail.Members)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Uses != 1 {
		t.Fatalf("uses = %d, want 1", c.Uses)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = 'member.joined'`, gid); n != 1 {
		t.Fatalf("member.joined audit events = %d, want 1", n)
	}
}

func TestJoin_AlreadyMemberSucceedsWithoutSpendingAUse(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 0, 0)

	if r := s.Join(t, alice, code); r.Status != http.StatusOK { // the owner is already a member
		t.Fatalf("owner re-join = %d, want 200: %s", r.Status, r.Body)
	}
	if r := s.Join(t, bob, code); r.Status != http.StatusCreated {
		t.Fatalf("first join = %d", r.Status)
	}
	if r := s.Join(t, bob, code); r.Status != http.StatusOK { // new idempotency key, already a member
		t.Fatalf("second join = %d, want 200: %s", r.Status, r.Body)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Uses != 1 {
		t.Fatalf("uses = %d, want 1 (only bob's first join counts)", c.Uses)
	}
}

// Unknown, expired, revoked and used-up codes must be indistinguishable, or an
// attacker could learn which codes once existed.
func TestJoin_UnusableCodesAreIndistinguishable(t *testing.T) {
	s := teststack.New(t)
	alice, mallory := s.Token(t, "alice"), s.Token(t, "mallory")
	gid := s.CreateGroup(t, alice, "Trip")

	_, unknown := "", joincode.Display(joincode.Generate())

	expiredID, expired := s.CreateJoinCode(t, alice, gid, 0, 0)
	s.Exec(t, `UPDATE join_codes SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID)

	revokedID, revoked := s.CreateJoinCode(t, alice, gid, 0, 0)
	if r := s.Do(t, alice, "DELETE", "/v1/groups/"+gid+"/join-codes/"+revokedID, ""); r.Status != http.StatusNoContent {
		t.Fatalf("revoke = %d", r.Status)
	}

	exhaustedID, exhausted := s.CreateJoinCode(t, alice, gid, 1, 0)
	s.Exec(t, `UPDATE join_codes SET uses = max_uses WHERE id = $1`, exhaustedID)

	var bodies [][]byte
	for name, code := range map[string]string{"unknown": unknown, "expired": expired, "revoked": revoked, "exhausted": exhausted} {
		r := s.Join(t, mallory, code)
		if r.Status != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", name, r.Status)
		}
		bodies = append(bodies, r.Body)
	}
	for _, b := range bodies[1:] {
		if !bytes.Equal(b, bodies[0]) {
			t.Fatalf("unusable-code responses differ:\n%s\n%s", bodies[0], b)
		}
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM memberships WHERE group_id = $1`, gid); n != 1 {
		t.Fatalf("memberships = %d, want only the owner", n)
	}
}

func TestJoin_CodeInputIsNormalized(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")
	_, code := s.CreateJoinCode(t, alice, gid, 10, 0)

	for name, variant := range map[string]string{
		"lowercase":  strings.ToLower(code),
		"no hyphens": strings.ReplaceAll(code, "-", ""),
		"spaces":     strings.ReplaceAll(code, "-", " "),
	} {
		if r := s.Join(t, s.Token(t, "u"), variant); r.Status != http.StatusCreated {
			t.Errorf("%s: join = %d, want 201: %s", name, r.Status, r.Body)
		}
	}
	for name, bad := range map[string]string{"empty": "", "short": "ABCD", "punctuation": "ABCD-EFGH-JK!M", "long": code + "X"} {
		if r := s.Join(t, s.Token(t, "u"), bad); r.Status != http.StatusBadRequest {
			t.Errorf("%s: join = %d, want 400", name, r.Status)
		}
	}
}

func TestJoin_IdempotencyKeyValidation(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	_, code := s.CreateJoinCode(t, alice, gid, 0, 0)
	body := `{"code":"` + code + `"}`

	if r := s.Do(t, bob, "POST", "/v1/join", body); r.Status != http.StatusBadRequest {
		t.Errorf("missing key = %d, want 400", r.Status)
	}
	for name, key := range map[string]string{"space": "a b", "too long": strings.Repeat("k", 256), "control char": "a\tb"} {
		r := s.Do(t, bob, "POST", "/v1/join", body, "Idempotency-Key", key)
		if r.Status != http.StatusBadRequest {
			t.Errorf("%s key = %d, want 400", name, r.Status)
		}
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM memberships WHERE group_id = $1`, gid); n != 1 {
		t.Fatalf("rejected requests changed membership: %d", n)
	}
}

func TestJoin_ReplayReturnsFirstResponseWithoutRerunning(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 0, 0)
	key := uuid.NewString()

	first := s.JoinWithKey(t, bob, code, key)
	if first.Status != http.StatusCreated || first.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("first: %d replayed=%q", first.Status, first.Header.Get("Idempotent-Replayed"))
	}
	// A real re-run would answer 200 ("already a member"). A replay is 201 again.
	second := s.JoinWithKey(t, bob, code, key)
	if second.Status != http.StatusCreated || second.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("second: %d replayed=%q, want 201 replayed", second.Status, second.Header.Get("Idempotent-Replayed"))
	}
	if !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("replay body differs:\n%s\n%s", first.Body, second.Body)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Uses != 1 {
		t.Fatalf("uses = %d after replay, want 1", c.Uses)
	}
}

func TestJoin_KeyReuseWithDifferentRequestIs422(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	_, codeA := s.CreateJoinCode(t, alice, gid, 0, 0)
	_, codeB := s.CreateJoinCode(t, alice, gid, 0, 0)
	key := uuid.NewString()

	if r := s.JoinWithKey(t, bob, codeA, key); r.Status != http.StatusCreated {
		t.Fatalf("first = %d", r.Status)
	}
	r := s.JoinWithKey(t, bob, codeB, key)
	if r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("reused key with other code = %d, want 422: %s", r.Status, r.Body)
	}
	// Same code in a different format is the same request, not a conflict.
	if r := s.JoinWithKey(t, bob, strings.ToLower(strings.ReplaceAll(codeA, "-", "")), key); r.Status != http.StatusCreated {
		t.Fatalf("same code, different format = %d, want replayed 201", r.Status)
	}
}

func TestJoin_KeysAreScopedPerUser(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")
	_, code := s.CreateJoinCode(t, alice, gid, 5, 0)
	key := uuid.NewString()

	for _, name := range []string{"bob", "carol"} {
		r := s.JoinWithKey(t, s.Token(t, name), code, key)
		if r.Status != http.StatusCreated || r.Header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("%s with shared key: %d replayed=%q", name, r.Status, r.Header.Get("Idempotent-Replayed"))
		}
	}
}

// Stripe-style: the first outcome is final for a key, even when it was a
// failure. A different key gets a fresh evaluation.
func TestJoin_FailureIsReplayedForSameKey(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 1, 0)
	s.Exec(t, `UPDATE join_codes SET uses = max_uses WHERE id = $1`, codeID) // exhausted
	key := uuid.NewString()

	if r := s.JoinWithKey(t, bob, code, key); r.Status != http.StatusNotFound {
		t.Fatalf("exhausted = %d, want 404", r.Status)
	}
	s.Exec(t, `UPDATE join_codes SET max_uses = 5 WHERE id = $1`, codeID) // now usable again

	r := s.JoinWithKey(t, bob, code, key)
	if r.Status != http.StatusNotFound || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("same key after the code became usable: %d replayed=%q, want replayed 404", r.Status, r.Header.Get("Idempotent-Replayed"))
	}
	if r := s.Join(t, bob, code); r.Status != http.StatusCreated {
		t.Fatalf("fresh key = %d, want 201", r.Status)
	}
}

func TestJoin_ExpiredKeyIsReusable(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	_, codeA := s.CreateJoinCode(t, alice, gid, 0, 0)
	_, codeB := s.CreateJoinCode(t, alice, gid, 0, 0)
	key := uuid.NewString()

	if r := s.JoinWithKey(t, bob, codeA, key); r.Status != http.StatusCreated {
		t.Fatalf("first = %d", r.Status)
	}
	s.Exec(t, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = $1`, key)

	// Past the 24 h window the key is forgotten: a different request is no longer a conflict.
	r := s.JoinWithKey(t, bob, codeB, key)
	if r.Status == http.StatusUnprocessableEntity || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("expired key still enforced: %d replayed=%q", r.Status, r.Header.Get("Idempotent-Replayed"))
	}
}

// Ten simultaneous requests with the same key must run the join exactly once;
// the other nine wait on the key's unique index and replay.
func TestJoin_ConcurrentDuplicatesRunOnce(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 0, 0)
	key := uuid.NewString()

	const n = 10
	resps := make([]teststack.Response, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i] = s.JoinWithKey(t, bob, code, key)
		}()
	}
	wg.Wait()

	replays := 0
	for i, r := range resps {
		if r.Status != http.StatusCreated {
			t.Fatalf("request %d = %d, want 201 (original or replay): %s", i, r.Status, r.Body)
		}
		if !bytes.Equal(r.Body, resps[0].Body) {
			t.Fatalf("request %d body differs", i)
		}
		if r.Header.Get("Idempotent-Replayed") == "true" {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("replays = %d, want %d (exactly one request actually ran)", replays, n-1)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Uses != 1 {
		t.Fatalf("uses = %d, want 1", c.Uses)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = 'member.joined'`, gid); n != 1 {
		t.Fatalf("member.joined events = %d, want 1", n)
	}
}

// Twelve different users race for the last three uses of one code.
func TestJoin_UseLimitHoldsUnderRace(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 3, 0)

	const users = 12
	statuses := make([]int, users)
	var wg sync.WaitGroup
	for i := range users {
		tok := s.Token(t, "racer")
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = s.Join(t, tok, code).Status
		}()
	}
	wg.Wait()

	got := map[int]int{}
	for _, st := range statuses {
		got[st]++
	}
	if got[http.StatusCreated] != 3 || got[http.StatusNotFound] != users-3 {
		t.Fatalf("statuses = %v, want 3x201 and %dx404", got, users-3)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Uses != 3 || c.Status != "exhausted" {
		t.Fatalf("code = %+v, want uses=3 exhausted", c)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM memberships WHERE group_id = $1 AND role = 'member'`, gid); n != 3 {
		t.Fatalf("members = %d, want 3", n)
	}
}

func TestJoin_RateLimited(t *testing.T) {
	s := teststack.New(t, teststack.WithJoinLimit(rate.Every(time.Hour), 3))
	alice, mallory, bob := s.Token(t, "alice"), s.Token(t, "mallory"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	_, real := s.CreateJoinCode(t, alice, gid, 0, 0)

	// Mallory burns her burst on wrong guesses...
	for i := range 3 {
		if r := s.Join(t, mallory, joincode.Display(joincode.Generate())); r.Status != http.StatusNotFound {
			t.Fatalf("guess %d = %d, want 404", i+1, r.Status)
		}
	}
	// ...and is then blocked, even holding the real code.
	key := uuid.NewString()
	r := s.JoinWithKey(t, mallory, real, key)
	if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("4th attempt = %d retry-after=%q, want 429 with Retry-After", r.Status, r.Header.Get("Retry-After"))
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, key); n != 0 {
		t.Fatal("a rate-limited request must not claim an idempotency key")
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM memberships WHERE group_id = $1`, gid); n != 1 {
		t.Fatal("rate-limited request changed membership")
	}
	// The limit is per user: Bob is unaffected.
	if r := s.Join(t, bob, real); r.Status != http.StatusCreated {
		t.Fatalf("bob = %d, want 201", r.Status)
	}
}

func TestRevokeJoinCode(t *testing.T) {
	s := teststack.New(t)
	alice, bob := s.Token(t, "alice"), s.Token(t, "bob")
	gid := s.CreateGroup(t, alice, "Trip")
	codeID, code := s.CreateJoinCode(t, alice, gid, 0, 0)

	path := "/v1/groups/" + gid + "/join-codes/" + codeID
	if r := s.Do(t, alice, "DELETE", path, ""); r.Status != http.StatusNoContent {
		t.Fatalf("revoke = %d", r.Status)
	}
	if r := s.Do(t, alice, "DELETE", path, ""); r.Status != http.StatusNoContent {
		t.Fatalf("second revoke = %d, want 204 (idempotent)", r.Status)
	}
	if r := s.Join(t, bob, code); r.Status != http.StatusNotFound {
		t.Fatalf("join with revoked code = %d, want 404", r.Status)
	}
	if c := codeByID(t, listCodes(t, s, alice, gid), codeID); c.Status != "revoked" || c.RevokedAt == nil {
		t.Fatalf("code = %+v, want revoked", c)
	}
	if n := s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE group_id = $1 AND kind = 'join_code.revoked'`, gid); n != 1 {
		t.Fatalf("join_code.revoked events = %d, want 1 (the repeat is a no-op)", n)
	}
	if r := s.Do(t, alice, "DELETE", "/v1/groups/"+gid+"/join-codes/"+uuid.NewString(), ""); r.Status != http.StatusNotFound {
		t.Fatalf("unknown code id = %d, want 404", r.Status)
	}
}

func TestListJoinCodes_Statuses(t *testing.T) {
	s := teststack.New(t)
	alice := s.Token(t, "alice")
	gid := s.CreateGroup(t, alice, "Trip")

	activeID, _ := s.CreateJoinCode(t, alice, gid, 0, 0)
	exhaustedID, _ := s.CreateJoinCode(t, alice, gid, 1, 0)
	expiredID, _ := s.CreateJoinCode(t, alice, gid, 0, 0)
	revokedID, _ := s.CreateJoinCode(t, alice, gid, 0, 0)
	s.Exec(t, `UPDATE join_codes SET uses = max_uses WHERE id = $1`, exhaustedID)
	s.Exec(t, `UPDATE join_codes SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID)
	s.Do(t, alice, "DELETE", "/v1/groups/"+gid+"/join-codes/"+revokedID, "")

	codes := listCodes(t, s, alice, gid)
	for id, want := range map[string]string{activeID: "active", exhaustedID: "exhausted", expiredID: "expired", revokedID: "revoked"} {
		if got := codeByID(t, codes, id).Status; got != want {
			t.Errorf("code %s status = %q, want %q", id, got, want)
		}
	}
}
