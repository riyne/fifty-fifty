package receiptapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/riyne/fifty-fifty/internal/db"
	"github.com/riyne/fifty-fifty/internal/objstore"
	"github.com/riyne/fifty-fifty/internal/receiptapi"
	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
	"github.com/riyne/fifty-fifty/internal/settlerig"
	"github.com/riyne/fifty-fifty/internal/teststack"
)

// ---- helpers ----

type part struct {
	name string
	data []byte
	file bool // a file part (with a filename) or a plain field
}

func filePart(data []byte) part { return part{name: "file", data: data, file: true} }
func descPart(s string) part    { return part{name: "description", data: []byte(s)} }
func namedPart(n string) part   { return part{name: n, data: []byte("x")} }
func form(parts ...part) (body string, contentType string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.file {
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename="receipt.png"`, p.name))
			h.Set("Content-Type", "image/png") // ignored by the server on purpose
			w, _ := mw.CreatePart(h)
			w.Write(p.data)
		} else {
			mw.WriteField(p.name, string(p.data))
		}
	}
	mw.Close()
	return buf.String(), mw.FormDataContentType()
}

func toJPEG(t testing.TB, pngBytes []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tinyPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngOf(t testing.TB, ref int) []byte {
	t.Helper()
	b, err := synth.Generate(ref).Render(synth.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type world struct {
	s                       *teststack.Stack
	owner, member, outsider string
	ownerID, memberID       uuid.UUID
	group, otherGroup       string
}

func newWorld(t *testing.T, opts ...teststack.Option) *world {
	t.Helper()
	return newWorldOn(t, teststack.New(t, opts...))
}

func newWorldOn(t *testing.T, s *teststack.Stack) *world {
	t.Helper()
	w := &world{s: s, owner: s.Token(t, "owner"), member: s.Token(t, "member"), outsider: s.Token(t, "outsider")}
	w.group = s.CreateGroup(t, w.owner, "G")
	w.otherGroup = s.CreateGroup(t, w.outsider, "Other")
	_, code := s.CreateJoinCode(t, w.owner, w.group, 0, 0)
	if r := s.Join(t, w.member, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d: %s", r.Status, r.Body)
	}
	w.ownerID, w.memberID = s.UserID(t, w.owner), s.UserID(t, w.member)
	return w
}

func (w *world) upload(t *testing.T, token, group, key string, parts ...part) teststack.Response {
	t.Helper()
	body, ct := form(parts...)
	return w.uploadRaw(t, token, group, key, body, ct)
}

func (w *world) uploadRaw(t *testing.T, token, group, key, body, contentType string) teststack.Response {
	t.Helper()
	h := []string{"Content-Type", contentType}
	if key != "" {
		h = append(h, "Idempotency-Key", key)
	}
	return w.s.Do(t, token, "POST", "/v1/groups/"+group+"/receipts", body, h...)
}

type uploaded struct {
	ExpenseID   uuid.UUID `json:"expense_id"`
	ReceiptID   uuid.UUID `json:"receipt_id"`
	Status      string    `json:"status"`
	Version     int       `json:"version"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width_px"`
	Height      int       `json:"height_px"`
	SizeBytes   int       `json:"size_bytes"`
}

func (w *world) mustUpload(t *testing.T, token, group string, parts ...part) uploaded {
	t.Helper()
	r := w.upload(t, token, group, uuid.NewString(), parts...)
	if r.Status != http.StatusAccepted {
		t.Fatalf("upload = %d: %s", r.Status, r.Body)
	}
	var u uploaded
	r.JSON(t, &u)
	return u
}

func (w *world) counts(t *testing.T, group string) (expenses, receiptRows int) {
	t.Helper()
	return w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE group_id = $1`, group),
		w.s.QueryInt(t, `SELECT count(*) FROM receipts WHERE group_id = $1`, group)
}

func (w *world) quotaUsed(t *testing.T, user uuid.UUID) int {
	t.Helper()
	return w.s.QueryInt(t, `SELECT coalesce(sum(used), 0) FROM receipt_quota WHERE user_id = $1`, user)
}

// parseJobs counts the receipt.parse jobs of the group's receipts, in any state.
func (w *world) parseJobs(t *testing.T, group string) int {
	t.Helper()
	return w.s.QueryInt(t, `SELECT count(*) FROM jobs j JOIN receipts r ON r.id::text = j.payload->>'receipt_id'
		WHERE j.kind = 'receipt.parse' AND r.group_id = $1`, group)
}

func objects(t *testing.T, s *teststack.Stack) int {
	t.Helper()
	entries, err := os.ReadDir(s.ReceiptDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

type errBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func expectErr(t *testing.T, r teststack.Response, status int, code string) {
	t.Helper()
	var e errBody
	_ = json.Unmarshal(r.Body, &e)
	if r.Status != status || e.Code != code {
		t.Fatalf("got %d %s, want %d %s", r.Status, r.Body, status, code)
	}
}

// ---- the happy path ----

func TestUpload_CreatesAParsingExpenseAndStoresTheImage(t *testing.T) {
	w := newWorld(t)
	img := pngOf(t, 1)
	r := w.upload(t, w.member, w.group, "k1", filePart(img), descPart("  Friday dinner  "))
	if r.Status != http.StatusAccepted || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("upload = %d: %s", r.Status, r.Body)
	}
	var u uploaded
	r.JSON(t, &u)
	if u.Status != "parsing" || u.Version != 1 || u.ContentType != "image/png" || u.Width < 400 || u.Height < 400 || u.SizeBytes <= 0 {
		t.Fatalf("response %+v", u)
	}

	// The expense: payer is the uploader, trimmed description, no items, parsing.
	var e struct {
		Status      string `json:"status"`
		PayerID     string `json:"payer_id"`
		Description string `json:"description"`
		Items       []any  `json:"items"`
		Version     int    `json:"version"`
	}
	g := w.s.Do(t, w.owner, "GET", "/v1/groups/"+w.group+"/expenses/"+u.ExpenseID.String(), "") // another member can read it
	if g.Status != 200 {
		t.Fatalf("get = %d: %s", g.Status, g.Body)
	}
	g.JSON(t, &e)
	if e.Status != "parsing" || e.PayerID != w.memberID.String() || e.Description != "Friday dinner" || len(e.Items) != 0 || e.Version != 1 {
		t.Fatalf("expense %+v", e)
	}

	// The receipt row points at an object whose bytes are the hardened image.
	var key, ct string
	var size, wpx, hpx int
	err := w.s.Pool.QueryRow(context.Background(),
		`SELECT object_key, content_type, size_bytes, width_px, height_px FROM receipts WHERE id = $1 AND expense_id = $2 AND uploader_id = $3`,
		u.ReceiptID, u.ExpenseID, w.memberID).Scan(&key, &ct, &size, &wpx, &hpx)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := objstore.NewFS(w.s.ReceiptDir)
	stored, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := receipts.HardenImage(img, receipts.DefaultImageLimits)
	if !bytes.Equal(stored, want.Data) || size != len(stored) || ct != "image/png" || wpx != u.Width || hpx != u.Height {
		t.Errorf("stored %d bytes (row says %d, %s, %dx%d)", len(stored), size, ct, wpx, hpx)
	}
	if objects(t, w.s) != 1 {
		t.Errorf("%d objects, want 1", objects(t, w.s))
	}
	if n := w.quotaUsed(t, w.memberID); n != 1 {
		t.Errorf("quota used %d, want 1", n)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'receipt.uploaded' AND actor_user_id = $1 AND group_id = $2`, w.memberID, w.group); n != 1 {
		t.Errorf("%d audit events", n)
	}
	// The parse job was written in the same transaction: one queued job naming this receipt.
	if n := w.s.QueryInt(t, `SELECT count(*) FROM jobs WHERE kind = 'receipt.parse' AND state = 'queued' AND max_attempts = $2 AND payload = jsonb_build_object('receipt_id', $1::text)`,
		u.ReceiptID, receiptparse.ParseMaxAttempts); n != 1 {
		t.Errorf("%d queued parse jobs for the receipt, want 1", n)
	}

	// A parsing expense cannot be edited, confirmed, claimed or finalized yet.
	ep := "/v1/groups/" + w.group + "/expenses/" + u.ExpenseID.String()
	for name, resp := range map[string]teststack.Response{
		"edit":     w.s.Do(t, w.member, "PATCH", ep, `{"version":1,"add":[{"description":"x","price_minor":100}]}`, "Idempotency-Key", uuid.NewString()),
		"confirm":  w.s.Do(t, w.member, "POST", ep+"/confirm", `{"version":1}`, "Idempotency-Key", uuid.NewString()),
		"finalize": w.s.Do(t, w.member, "POST", ep+"/finalize", `{}`, "Idempotency-Key", uuid.NewString()),
	} {
		if resp.Status < 400 || resp.Status >= 500 {
			t.Errorf("%s on a parsing expense = %d: %s", name, resp.Status, resp.Body)
		}
	}
	if st := w.s.QueryInt(t, `SELECT count(*) FROM expenses WHERE id = $1 AND status = 'parsing' AND version = 1`, u.ExpenseID); st != 1 {
		t.Error("the expense changed")
	}
}

func TestUpload_DescriptionDefaultsAndJPEG(t *testing.T) {
	w := newWorld(t)
	// The server decides the format from the bytes, not from the part header (which
	// form() labels image/png).
	jp := toJPEG(t, pngOf(t, 2))
	u := w.mustUpload(t, w.owner, w.group, filePart(jp))
	if u.ContentType != "image/jpeg" {
		t.Errorf("content type %q", u.ContentType)
	}
	var desc string
	if err := w.s.Pool.QueryRow(context.Background(), `SELECT description FROM expenses WHERE id = $1`, u.ExpenseID).Scan(&desc); err != nil || desc != receiptapi.DefaultReceiptTitle {
		t.Errorf("description %q (%v)", desc, err)
	}
}

// ---- who may upload, and in what order things are checked ----

func TestUpload_NonMembersAndBadGroupsGet404BeforeAnythingElse(t *testing.T) {
	w := newWorld(t)
	img := pngOf(t, 3)
	for name, tc := range map[string]struct {
		token, group string
	}{
		"outsider":            {w.outsider, w.group},
		"member of the other": {w.member, w.otherGroup},
		"unknown group":       {w.owner, uuid.NewString()},
		"malformed group":     {w.owner, "nope"},
	} {
		// No key, a body that is not multipart: a non-member must learn nothing about
		// either, so the answer is the same 404.
		r := w.uploadRaw(t, tc.token, tc.group, "", "garbage", "text/plain")
		expectErr(t, r, 404, "not_found")
		r = w.upload(t, tc.token, tc.group, uuid.NewString(), filePart(img))
		expectErr(t, r, 404, "not_found")
		_ = name
	}
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 {
		t.Errorf("%d expenses and %d receipts in the group", e, rc)
	}
	if objects(t, w.s) != 0 {
		t.Error("an object was stored for a refused upload")
	}
	if n := w.quotaUsed(t, w.s.UserID(t, w.outsider)); n != 0 {
		t.Errorf("quota used %d by a non-member", n)
	}
	r := w.s.Do(t, "", "POST", "/v1/groups/"+w.group+"/receipts", "", "Idempotency-Key", "k")
	if r.Status != http.StatusUnauthorized {
		t.Errorf("no token = %d", r.Status)
	}
}

// ---- the request shape ----

func TestUpload_RejectsMalformedRequestsWithoutSideEffects(t *testing.T) {
	w := newWorld(t)
	img := pngOf(t, 4)
	good, goodCT := form(filePart(img))
	long := strings.Repeat("é", 201)
	cases := []struct {
		name         string
		key          string
		body, ct     string
		status       int
		code         string
		skipNotFound bool
	}{
		{name: "no key", key: "", body: good, ct: goodCT, status: 400, code: "invalid_request"},
		{name: "not multipart", key: "k", body: string(img), ct: "image/png", status: 415, code: "unsupported_media_type"},
		{name: "json body", key: "k", body: `{"file":"x"}`, ct: "application/json", status: 415, code: "unsupported_media_type"},
		{name: "no content type", key: "k", body: good, ct: "", status: 415, code: "unsupported_media_type"},
		{name: "multipart without a boundary", key: "k", body: good, ct: "multipart/form-data", status: 400, code: "invalid_request"},
	}
	for _, p := range []struct {
		name  string
		parts []part
		code  string
	}{
		{"no file part", []part{descPart("x")}, "invalid_request"},
		{"empty form", nil, "invalid_request"},
		{"two file parts", []part{filePart(img), filePart(img)}, "invalid_request"},
		{"two description parts", []part{filePart(img), descPart("a"), descPart("b")}, "invalid_request"},
		{"unknown part", []part{filePart(img), namedPart("note")}, "invalid_request"},
		{"more parts than the form has", []part{filePart(img), descPart("a"), namedPart("b"), namedPart("c"), namedPart("d")}, "invalid_request"},
		{"empty description", []part{filePart(img), descPart("")}, "invalid_request"},
		{"blank description", []part{filePart(img), descPart("   \t ")}, "invalid_request"},
		{"description over 200 characters", []part{filePart(img), descPart(long)}, "invalid_request"},
		{"description over 1 KiB", []part{filePart(img), descPart(strings.Repeat("a", 1025))}, "invalid_request"},
		{"description not UTF-8", []part{filePart(img), descPart("caf\xe9")}, "invalid_request"},
		// Short once trimmed, but over 1 KiB as sent: refused, not silently cut.
		{"padded description over 1 KiB", []part{filePart(img), descPart(strings.Repeat(" ", 1000) + "abc" + strings.Repeat("x", 500))}, "invalid_request"},
	} {
		body, ct := form(p.parts...)
		cases = append(cases, struct {
			name         string
			key          string
			body, ct     string
			status       int
			code         string
			skipNotFound bool
		}{name: p.name, key: "k", body: body, ct: ct, status: 400, code: p.code})
	}
	// A body cut off in the middle of the file part.
	cut, cutCT := form(filePart(img))
	cases = append(cases, struct {
		name         string
		key          string
		body, ct     string
		status       int
		code         string
		skipNotFound bool
	}{name: "truncated multipart", key: "k", body: cut[:len(cut)/2], ct: cutCT, status: 400, code: "invalid_request"})

	for _, tc := range cases {
		r := w.uploadRaw(t, w.member, w.group, tc.key, tc.body, tc.ct)
		var e errBody
		_ = json.Unmarshal(r.Body, &e)
		if r.Status != tc.status || e.Code != tc.code {
			t.Errorf("%s: got %d %s, want %d %s", tc.name, r.Status, r.Body, tc.status, tc.code)
		}
	}
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 {
		t.Errorf("refused requests created %d expenses and %d receipts", e, rc)
	}
	if objects(t, w.s) != 0 || w.quotaUsed(t, w.memberID) != 0 {
		t.Errorf("refused requests left %d objects and used %d quota", objects(t, w.s), w.quotaUsed(t, w.memberID))
	}
	// A validation failure does not burn the key: the same key works for a good request.
	if r := w.upload(t, w.member, w.group, "k", filePart(img)); r.Status != http.StatusAccepted {
		t.Errorf("key burned by validation errors: %d %s", r.Status, r.Body)
	}
}

func TestUpload_TheDeclaredTypeAndFileNameDoNotMatter(t *testing.T) {
	w := newWorld(t)
	// A text file called receipt.png with an image/png label is refused; a PNG named
	// like a script is accepted. The bytes decide.
	r := w.upload(t, w.member, w.group, "k1", filePart([]byte("<?php echo 1; ?>")))
	expectErr(t, r, 400, "invalid_image")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="../../evil.php"`)
	h.Set("Content-Type", "text/x-php")
	pw, _ := mw.CreatePart(h)
	pw.Write(pngOf(t, 5))
	mw.Close()
	r = w.uploadRaw(t, w.member, w.group, "k2", buf.String(), mw.FormDataContentType())
	if r.Status != http.StatusAccepted {
		t.Fatalf("a PNG with an odd name and label = %d: %s", r.Status, r.Body)
	}
	// The object store uses only the content hash, never the client's name.
	entries, _ := os.ReadDir(w.s.ReceiptDir)
	if len(entries) != 1 || len(entries[0].Name()) != 64 {
		t.Errorf("store entries %v", entries)
	}
}

// ---- the image ----

func TestUpload_RejectsImagesThatAreNotAcceptable(t *testing.T) {
	w := newWorld(t)
	good := pngOf(t, 6)
	for name, data := range map[string][]byte{
		"text":      []byte("just some text"),
		"empty":     {},
		"truncated": good[:len(good)/2],
		"gif":       []byte("GIF89a" + strings.Repeat("\x00", 100)),
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"pdf":       []byte("%PDF-1.4 " + strings.Repeat("x", 100)),
		"tiny":      tinyPNG(t, 10, 10),
	} {
		r := w.upload(t, w.member, w.group, uuid.NewString(), filePart(data))
		var e errBody
		_ = json.Unmarshal(r.Body, &e)
		if r.Status != 400 || e.Code != "invalid_image" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
		// The message is fixed text: it must not echo the upload.
		if strings.Contains(e.Message, "alert") || strings.Contains(e.Message, "PDF") {
			t.Errorf("%s: message quotes the upload: %q", name, e.Message)
		}
	}
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 || objects(t, w.s) != 0 || w.quotaUsed(t, w.memberID) != 0 {
		t.Errorf("rejected images left %d expenses, %d receipts, %d objects, %d quota", e, rc, objects(t, w.s), w.quotaUsed(t, w.memberID))
	}
}

func TestUpload_SizeLimits(t *testing.T) {
	lim := receipts.DefaultImageLimits
	lim.MaxUploadBytes = 20_000
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{Limits: lim}))

	// Just over the limit: 413, nothing stored, no quota used.
	over := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20_000)...)
	r := w.upload(t, w.member, w.group, "k1", filePart(over))
	expectErr(t, r, 413, "too_large")
	if objects(t, w.s) != 0 || w.quotaUsed(t, w.memberID) != 0 {
		t.Error("an oversize upload left something behind")
	}

	// Exactly the limit is not too large for the size check (it is not a real PNG, so
	// it is refused as an image, not as too large).
	atLimit := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 20_000-8)...)
	expectErr(t, w.upload(t, w.member, w.group, "k2", filePart(atLimit)), 400, "invalid_image")

	// Far over: the server stops reading at the limit plus the form overhead, so a huge
	// body cannot be pushed through. The request ends in 413 (or the connection is cut
	// while the client is still sending, which is the same refusal seen from the wire).
	huge := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 3<<20)...)
	body, ct := form(filePart(huge))
	req, _ := http.NewRequest("POST", w.s.Server.URL+"/v1/groups/"+w.group+"/receipts", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+w.member)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "k3")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Errorf("a 3 MiB body = %d, want 413", resp.StatusCode)
		}
	}
	if objects(t, w.s) != 0 || w.quotaUsed(t, w.memberID) != 0 {
		t.Error("refused uploads left something behind")
	}
}

// ---- idempotency ----

func TestUpload_Idempotency(t *testing.T) {
	w := newWorld(t)
	img := pngOf(t, 7)
	first := w.upload(t, w.member, w.group, "same", filePart(img), descPart("lunch"))
	if first.Status != http.StatusAccepted {
		t.Fatalf("first = %d: %s", first.Status, first.Body)
	}
	for i := 0; i < 3; i++ {
		again := w.upload(t, w.member, w.group, "same", filePart(img), descPart("lunch"))
		if again.Status != http.StatusAccepted || again.Header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.Body, first.Body) {
			t.Fatalf("replay %d = %d replayed=%q\n%s\nwant %s", i, again.Status, again.Header.Get("Idempotent-Replayed"), again.Body, first.Body)
		}
	}
	if e, rc := w.counts(t, w.group); e != 1 || rc != 1 {
		t.Errorf("%d expenses and %d receipts after 4 identical requests", e, rc)
	}
	if n := w.quotaUsed(t, w.memberID); n != 1 {
		t.Errorf("replays consumed quota: %d", n)
	}
	if n := w.parseJobs(t, w.group); n != 1 {
		t.Errorf("%d parse jobs after 4 identical requests", n)
	}

	// The same key with a different image, or a different description, is a client bug.
	expectErr(t, w.upload(t, w.member, w.group, "same", filePart(pngOf(t, 8)), descPart("lunch")), 422, "idempotency_key_reuse")
	expectErr(t, w.upload(t, w.member, w.group, "same", filePart(img), descPart("dinner")), 422, "idempotency_key_reuse")
	expectErr(t, w.upload(t, w.member, w.group, "same", filePart(img)), 422, "idempotency_key_reuse")

	// Keys are per user: another member's identical key is their own request.
	if r := w.upload(t, w.owner, w.group, "same", filePart(img), descPart("lunch")); r.Status != http.StatusAccepted || r.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("another user with the same key = %d replayed=%q", r.Status, r.Header.Get("Idempotent-Replayed"))
	}

	// Refused and replayed requests wrote nothing: only the first image is on disk.
	if objects(t, w.s) != 1 {
		t.Errorf("%d objects after replays and refusals, want 1", objects(t, w.s))
	}

	// A new key with the same image is a new expense, and the two share one stored object.
	if r := w.upload(t, w.member, w.group, "other-key", filePart(img), descPart("lunch")); r.Status != http.StatusAccepted {
		t.Fatalf("second upload = %d", r.Status)
	}
	if e, rc := w.counts(t, w.group); e != 3 || rc != 3 {
		t.Errorf("%d expenses and %d receipts, want 3 and 3", e, rc)
	}
	if objects(t, w.s) != 1 {
		t.Errorf("%d objects for one image", objects(t, w.s))
	}
}

func TestUpload_ConcurrentDuplicatesCreateOneExpense(t *testing.T) {
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{MaxHardenings: 32}))
	img := pngOf(t, 9)
	var wg sync.WaitGroup
	results := make([]teststack.Response, 12)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = w.upload(t, w.member, w.group, "dup", filePart(img))
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.Status != http.StatusAccepted || !bytes.Equal(r.Body, results[0].Body) {
			t.Fatalf("request %d = %d: %s", i, r.Status, r.Body)
		}
	}
	if e, rc := w.counts(t, w.group); e != 1 || rc != 1 || w.quotaUsed(t, w.memberID) != 1 {
		t.Errorf("%d expenses, %d receipts, quota %d", e, rc, w.quotaUsed(t, w.memberID))
	}
}

// ---- quota ----

func TestQuota_StopsAtTheLimitAndReplaysAreFree(t *testing.T) {
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: 3}))
	imgs := [][]byte{pngOf(t, 10), pngOf(t, 11), pngOf(t, 12), pngOf(t, 13)}
	for i := 0; i < 3; i++ {
		if r := w.upload(t, w.member, w.group, fmt.Sprintf("k%d", i), filePart(imgs[i])); r.Status != 202 {
			t.Fatalf("upload %d = %d: %s", i, r.Status, r.Body)
		}
	}
	r := w.upload(t, w.member, w.group, "k3", filePart(imgs[3]))
	expectErr(t, r, 429, "quota_exceeded")
	if ra, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || ra < 1 || ra > 86400 {
		t.Errorf("Retry-After %q, want 1 to 86400 seconds", r.Header.Get("Retry-After"))
	}
	if e, _ := w.counts(t, w.group); e != 3 || w.quotaUsed(t, w.memberID) != 3 {
		t.Errorf("%d expenses, quota %d after the refusal", e, w.quotaUsed(t, w.memberID))
	}
	if objects(t, w.s) != 3 {
		t.Errorf("%d objects: the refused upload must not have written its image", objects(t, w.s))
	}
	if n := w.parseJobs(t, w.group); n != 3 {
		t.Errorf("%d parse jobs, want one per accepted upload", n)
	}

	// A replay of an earlier success still answers, at the limit, and uses nothing.
	if r := w.upload(t, w.member, w.group, "k0", filePart(imgs[0])); r.Status != 202 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay at the limit = %d %q", r.Status, r.Header.Get("Idempotent-Replayed"))
	}

	// The quota is per user: the owner has their own.
	if r := w.upload(t, w.owner, w.group, "k3", filePart(imgs[3])); r.Status != 202 {
		t.Errorf("another user at the limit's key = %d: %s", r.Status, r.Body)
	}

	// The 429 was not stored under the key: once the quota has room, the same key works.
	w.s.Exec(t, `UPDATE receipt_quota SET used = 0 WHERE user_id = $1`, w.memberID)
	if r := w.upload(t, w.member, w.group, "k3", filePart(imgs[3])); r.Status != 202 || r.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("retry of a refused upload with room = %d %q: %s", r.Status, r.Header.Get("Idempotent-Replayed"), r.Body)
	}
}

func TestQuota_ResetsWithTheUTCDay(t *testing.T) {
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: 1}))
	if r := w.upload(t, w.member, w.group, "a", filePart(pngOf(t, 14))); r.Status != 202 {
		t.Fatal(r.Status)
	}
	expectErr(t, w.upload(t, w.member, w.group, "b", filePart(pngOf(t, 15))), 429, "quota_exceeded")
	// Yesterday's row is no longer today's.
	w.s.Exec(t, `UPDATE receipt_quota SET day = day - 1 WHERE user_id = $1`, w.memberID)
	if r := w.upload(t, w.member, w.group, "b", filePart(pngOf(t, 15))); r.Status != 202 {
		t.Fatalf("next day = %d: %s", r.Status, r.Body)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM receipt_quota WHERE user_id = $1`, w.memberID); n != 2 {
		t.Errorf("%d quota rows, want one per day", n)
	}
}

// The counter is conditional in the database, so a burst cannot take it past the limit.
func TestQuota_ConcurrentUploadsNeverExceedTheLimit(t *testing.T) {
	const limit, attempts = 5, 24
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{DailyQuota: limit, MaxHardenings: attempts}))
	var wg sync.WaitGroup
	statuses := make([]int, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = w.upload(t, w.member, w.group, fmt.Sprintf("burst-%d", i), filePart(pngOf(t, 100+i))).Status
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, s := range statuses {
		switch s {
		case 202:
			ok++
		case 429:
			refused++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	e, rc := w.counts(t, w.group)
	if ok != limit || refused != attempts-limit || e != limit || rc != limit || w.quotaUsed(t, w.memberID) != limit {
		t.Fatalf("%d accepted, %d refused, %d expenses, %d receipts, quota %d; want exactly %d accepted", ok, refused, e, rc, w.quotaUsed(t, w.memberID), limit)
	}
}

// ---- rate limit and processing capacity ----

func TestUpload_RateLimitIsPerUserAndAfterMembership(t *testing.T) {
	w := newWorld(t, teststack.WithUploadLimit(rate.Every(time.Hour), 2))
	img := pngOf(t, 20)
	for i := 0; i < 2; i++ {
		if r := w.upload(t, w.member, w.group, fmt.Sprintf("r%d", i), filePart(img)); r.Status != 202 {
			t.Fatalf("upload %d = %d", i, r.Status)
		}
	}
	r := w.upload(t, w.member, w.group, "r2", filePart(img))
	expectErr(t, r, 429, "rate_limited")
	if r.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Another user is unaffected; a non-member gets 404, not 429, and spends nothing.
	if r := w.upload(t, w.owner, w.group, "r0", filePart(img)); r.Status != 202 {
		t.Errorf("another user = %d", r.Status)
	}
	for i := 0; i < 4; i++ {
		expectErr(t, w.upload(t, w.outsider, w.group, "x", filePart(img)), 404, "not_found")
	}
	if r := w.upload(t, w.outsider, w.otherGroup, "x", filePart(img)); r.Status != 202 {
		t.Errorf("the non-member's own group = %d: the 404s must not have spent their tokens", r.Status)
	}
}

// gateStore blocks Put until released, to hold an upload inside the hardening slot.
type gateStore struct {
	objstore.Store
	entered chan struct{}
	release chan struct{}
}

func (g gateStore) Put(ctx context.Context, data []byte) (string, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.Store.Put(ctx, data)
}

func TestUpload_WhenAllProcessingSlotsAreBusyTheAnswerIs503(t *testing.T) {
	inner, err := objstore.NewFS(filepath.Join(t.TempDir(), "r"))
	if err != nil {
		t.Fatal(err)
	}
	gate := gateStore{Store: inner, entered: make(chan struct{}, 8), release: make(chan struct{})}
	w := newWorld(t, teststack.WithReceiptStore(gate), teststack.WithReceiptConfig(receiptapi.Config{MaxHardenings: 1}))

	first := make(chan teststack.Response, 1)
	go func() { first <- w.upload(t, w.member, w.group, "a", filePart(pngOf(t, 21))) }()
	<-gate.entered // the first upload is inside the slot

	r := w.upload(t, w.owner, w.group, "b", filePart(pngOf(t, 22)))
	expectErr(t, r, 503, "busy")
	if r.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on 503")
	}
	if n := w.quotaUsed(t, w.ownerID); n != 0 {
		t.Errorf("the refused upload used %d quota", n)
	}

	close(gate.release)
	if got := <-first; got.Status != 202 {
		t.Fatalf("first upload = %d: %s", got.Status, got.Body)
	}
	// The slot is free again, including after a request that failed validation.
	expectErr(t, w.upload(t, w.owner, w.group, "c", filePart([]byte("not an image"))), 400, "invalid_image")
	if r := w.upload(t, w.owner, w.group, "b", filePart(pngOf(t, 22))); r.Status != 202 {
		t.Errorf("after release = %d: %s", r.Status, r.Body)
	}
}

type failingStore struct{ objstore.Store }

func (failingStore) Put(context.Context, []byte) (string, error) { return "", errors.New("disk full") }

func TestUpload_AStoreFailureIsA500AndCreatesNothing(t *testing.T) {
	inner, _ := objstore.NewFS(filepath.Join(t.TempDir(), "r"))
	w := newWorld(t, teststack.WithReceiptStore(failingStore{inner}))
	r := w.upload(t, w.member, w.group, "k", filePart(pngOf(t, 23)))
	expectErr(t, r, 500, "internal")
	if strings.Contains(string(r.Body), "disk full") {
		t.Error("the internal error was shown to the client")
	}
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 || w.quotaUsed(t, w.memberID) != 0 {
		t.Errorf("%d expenses, %d receipts, quota %d", e, rc, w.quotaUsed(t, w.memberID))
	}
}

// ---- atomicity ----

// If anything in the transaction fails after the quota is taken, the quota, the
// expense, the receipt and the idempotency key all roll back, and a retry with the same
// key succeeds. The stored object stays behind (decided, harmless). The failure is a
// trigger on a scratch database that makes the receipts insert raise.
func TestUpload_ATransactionFailureRollsBackEverything(t *testing.T) {
	ownerURL, appURL := os.Getenv("TEST_OWNER_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if ownerURL == "" || appURL == "" {
		t.Skip("TEST_OWNER_DATABASE_URL and TEST_DATABASE_URL are required")
	}
	ctx := context.Background()
	sc, err := settlerig.NewScratch(ctx, ownerURL, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sc.Drop(context.Background()); err != nil {
			t.Errorf("drop %s: %v", sc.Name, err)
		}
	})
	w := newWorldOn(t, teststack.NewOn(t, sc.Pool))

	admin, err := pgx.Connect(ctx, sc.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_receipts() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`,
		`CREATE TRIGGER refuse BEFORE INSERT ON receipts FOR EACH ROW EXECUTE FUNCTION refuse_receipts()`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	img := pngOf(t, 30)
	expectErr(t, w.upload(t, w.member, w.group, "retry-me", filePart(img)), 500, "internal")
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 {
		t.Errorf("failed upload left %d expenses and %d receipts", e, rc)
	}
	if n := w.quotaUsed(t, w.memberID); n != 0 {
		t.Errorf("failed upload kept %d quota", n)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM idempotency_keys WHERE user_id = $1 AND key = 'retry-me'`, w.memberID); n != 0 {
		t.Errorf("failed upload stored %d idempotency keys", n)
	}
	if n := w.s.QueryInt(t, `SELECT count(*) FROM audit_events WHERE kind = 'receipt.uploaded' AND group_id = $1`, w.group); n != 0 {
		t.Errorf("failed upload left %d audit events", n)
	}
	if objects(t, w.s) != 1 {
		t.Errorf("%d objects: the blob stored before the transaction is expected to remain", objects(t, w.s))
	}
	if n := w.parseJobs(t, w.group); n != 0 {
		t.Errorf("failed upload left %d parse jobs", n)
	}

	if _, err := admin.Exec(ctx, `DROP TRIGGER refuse ON receipts`); err != nil {
		t.Fatal(err)
	}
	if r := w.upload(t, w.member, w.group, "retry-me", filePart(img)); r.Status != 202 || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("retry with the same key = %d %q: %s", r.Status, r.Header.Get("Idempotent-Replayed"), r.Body)
	}
	if e, rc := w.counts(t, w.group); e != 1 || rc != 1 || w.quotaUsed(t, w.memberID) != 1 || objects(t, w.s) != 1 || w.parseJobs(t, w.group) != 1 {
		t.Errorf("after the retry: %d expenses, %d receipts, quota %d, %d objects, %d parse jobs", e, rc, w.quotaUsed(t, w.memberID), objects(t, w.s), w.parseJobs(t, w.group))
	}
}

// ---- database constraints (what holds whatever the handler does) ----

func TestReceiptsTable_RefusesWhatTheHandlerNeverDoes(t *testing.T) {
	w := newWorld(t)
	u := w.mustUpload(t, w.member, w.group, filePart(pngOf(t, 40)))
	ctx := context.Background()
	exec := func(sql string, args ...any) error {
		_, err := w.s.Pool.Exec(ctx, sql, args...)
		return err
	}
	key := strings.Repeat("a", 64)
	outsiderID := w.s.UserID(t, w.outsider)
	other := w.mustUpload(t, w.member, w.group, filePart(pngOf(t, 41)))

	// One receipt per expense.
	if err := exec(`INSERT INTO receipts (expense_id, group_id, uploader_id, object_key, content_type, size_bytes, width_px, height_px)
		VALUES ($1, $2, $3, $4, 'image/png', 1, 1, 1)`, u.ExpenseID, w.group, w.memberID, key); err == nil {
		t.Error("a second receipt for one expense was accepted")
	}
	// A receipt cannot name an expense of another group.
	if err := exec(`INSERT INTO receipts (expense_id, group_id, uploader_id, object_key, content_type, size_bytes, width_px, height_px)
		SELECT e.id, $2, $3, $4, 'image/png', 1, 1, 1 FROM expenses e WHERE e.id = $1`, other.ExpenseID, w.otherGroup, outsiderID, key); err == nil {
		t.Error("a receipt in the wrong group was accepted")
	}
	// Its uploader must be a member of the group.
	if err := exec(`UPDATE receipts SET uploader_id = $2 WHERE id = $1`, u.ReceiptID, outsiderID); err == nil {
		t.Error("a non-member uploader was accepted")
	}
	for name, sql := range map[string]string{
		"key not hex":     `UPDATE receipts SET object_key = '../../etc/passwd' WHERE id = $1`,
		"key uppercase":   `UPDATE receipts SET object_key = '` + strings.Repeat("A", 64) + `' WHERE id = $1`,
		"key too short":   `UPDATE receipts SET object_key = 'abc' WHERE id = $1`,
		"content type":    `UPDATE receipts SET content_type = 'image/gif' WHERE id = $1`,
		"zero size":       `UPDATE receipts SET size_bytes = 0 WHERE id = $1`,
		"zero width":      `UPDATE receipts SET width_px = 0 WHERE id = $1`,
		"negative height": `UPDATE receipts SET height_px = -1 WHERE id = $1`,
	} {
		if err := exec(sql, u.ReceiptID); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := exec(`UPDATE receipt_quota SET used = -1 WHERE user_id = $1`, w.memberID); err == nil {
		t.Error("a negative quota was accepted")
	}
}

// A body made of endless preamble (text before the first boundary, which a multipart
// reader skips) must be cut off by the body limit, not read to the end.
func TestUpload_AGiantPreambleIsCutOff(t *testing.T) {
	img := pngOf(t, 50)
	lim := receipts.DefaultImageLimits
	lim.MaxUploadBytes = len(img) + 1000 // the file itself is within the limit
	w := newWorld(t, teststack.WithReceiptConfig(receiptapi.Config{Limits: lim}))
	body, ct := form(filePart(img))
	preamble := strings.Repeat("ignore me\r\n", 3<<20/11) // about 3 MiB: over the limit plus the 1 MiB form overhead
	req, _ := http.NewRequest("POST", w.s.Server.URL+"/v1/groups/"+w.group+"/receipts", strings.NewReader(preamble+body))
	req.Header.Set("Authorization", "Bearer "+w.member)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "pre")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted {
			t.Fatal("an upload behind a 3 MiB preamble was accepted")
		}
	}
	if e, rc := w.counts(t, w.group); e != 0 || rc != 0 || objects(t, w.s) != 0 {
		t.Errorf("%d expenses, %d receipts, %d objects", e, rc, objects(t, w.s))
	}
}

// The quota statement on its own: exactly the limit's worth of units, none at limit 0,
// and one row per user and day.
func TestConsumeReceiptQuota_Statement(t *testing.T) {
	w := newWorld(t)
	q := db.New(w.s.Pool)
	ctx := context.Background()
	take := func(user uuid.UUID, limit int32) bool {
		_, err := q.ConsumeReceiptQuota(ctx, db.ConsumeReceiptQuotaParams{UserID: user, DailyLimit: limit})
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}
	if take(w.ownerID, 0) || take(w.ownerID, -3) {
		t.Error("a limit of zero or less allowed an upload")
	}
	if w.quotaUsed(t, w.ownerID) != 0 {
		t.Error("a refused first take created a row")
	}
	for i := 1; i <= 3; i++ {
		if !take(w.ownerID, 3) {
			t.Fatalf("take %d of 3 refused", i)
		}
	}
	if take(w.ownerID, 3) || w.quotaUsed(t, w.ownerID) != 3 {
		t.Errorf("a fourth take of 3 was allowed or counted (used %d)", w.quotaUsed(t, w.ownerID))
	}
	// Raising the limit makes room; lowering it below what is used refuses.
	if !take(w.ownerID, 4) || take(w.ownerID, 2) {
		t.Error("the limit is not read from the argument")
	}
	if !take(w.memberID, 1) || take(w.memberID, 1) {
		t.Error("another user's quota is not independent")
	}
}

// The same key in another group is another request: the group is part of the fingerprint.
func TestUpload_KeyIsBoundToTheGroup(t *testing.T) {
	w := newWorld(t)
	_, code := w.s.CreateJoinCode(t, w.outsider, w.otherGroup, 0, 0)
	if r := w.s.Join(t, w.member, code); r.Status != http.StatusCreated {
		t.Fatalf("join = %d", r.Status)
	}
	img := pngOf(t, 60)
	if r := w.upload(t, w.member, w.group, "k", filePart(img)); r.Status != 202 {
		t.Fatalf("first = %d", r.Status)
	}
	// Replaying the first group's response for a request aimed at the other group would
	// hand back an expense id that is not in that group.
	expectErr(t, w.upload(t, w.member, w.otherGroup, "k", filePart(img)), 422, "idempotency_key_reuse")
	if e, _ := w.counts(t, w.otherGroup); e != 0 {
		t.Errorf("%d expenses in the other group", e)
	}
}

// The server-wide read timeout (cmd/api sets 15 s) is too short for a photo over a slow
// connection, so the route extends its own deadline. Here the server's timeout is 300 ms
// and the body takes about 900 ms to arrive.
func TestUpload_ASlowBodyOutlivesTheServerReadTimeout(t *testing.T) {
	w := newWorld(t, teststack.WithServerReadTimeout(300*time.Millisecond))
	body, ct := form(filePart(pngOf(t, 61)))
	pr, pw := io.Pipe()
	go func() {
		third := len(body) / 3
		for i := 0; i < 3; i++ {
			end := (i + 1) * third
			if i == 2 {
				end = len(body)
			}
			_, _ = pw.Write([]byte(body[i*third : end]))
			time.Sleep(300 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", w.s.Server.URL+"/v1/groups/"+w.group+"/receipts", pr)
	req.Header.Set("Authorization", "Bearer "+w.member)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "slow")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow upload failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow upload = %d: %s", resp.StatusCode, b)
	}
	// Control: the same server cuts a slow body on a route that does not extend its deadline.
	r2, w2 := io.Pipe()
	go func() {
		w2.Write([]byte(`{"description":`))
		time.Sleep(900 * time.Millisecond)
		w2.Write([]byte(`"x","items":[{"description":"a","price_minor":1}]}`))
		w2.Close()
	}()
	req2, _ := http.NewRequest("POST", w.s.Server.URL+"/v1/groups/"+w.group+"/expenses", r2)
	req2.Header.Set("Authorization", "Bearer "+w.member)
	req2.Header.Set("Idempotency-Key", "slow-json")
	if resp2, err := http.DefaultClient.Do(req2); err == nil {
		defer resp2.Body.Close()
		if resp2.StatusCode == http.StatusCreated {
			t.Error("the control request succeeded: the server's read timeout is not in effect, so the test above proves nothing")
		}
	}
}
