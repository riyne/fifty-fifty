package stressrig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Stats counts what the workload did and saw. Counters are keyed by short names such as
// "claim/200" or "race_mixed"; the full list is in the Result so a reader can see which
// dangerous situations actually occurred, not only that no invariant broke.
type Stats struct {
	mu         sync.Mutex
	m          map[string]int64
	unexpected []string
}

func newStats() *Stats { return &Stats{m: map[string]int64{}} }

func (s *Stats) Inc(key string) { s.Add(key, 1) }

func (s *Stats) Add(key string, n int64) {
	s.mu.Lock()
	s.m[key] += n
	s.mu.Unlock()
}

func (s *Stats) Get(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key]
}

// Unexpected records a response the api should never give in this workload (a status
// outside the documented ones). The first few are kept as examples.
func (s *Stats) Unexpected(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m["unexpected"]++
	if len(s.unexpected) < 20 {
		s.unexpected = append(s.unexpected, fmt.Sprintf(format, args...))
	}
}

// Snapshot returns the counters and the unexpected examples.
func (s *Stats) Snapshot() (map[string]int64, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out, append([]string(nil), s.unexpected...)
}

// Keys returns the counter names sorted, for stable output.
func Keys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Client talks to the api over HTTP.
type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string) *Client {
	return &Client{base: base, hc: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 512, IdleConnTimeout: 30 * time.Second},
	}}
}

// Resp is one response, or the lack of one.
type Resp struct {
	Status   int
	Replayed bool // the api said it replayed a stored response (Idempotent-Replayed)
	Body     []byte
	// Err is set when there is no response: the client gave up (its context ended) or the
	// connection failed. The request may or may not have been carried out.
	Err error
}

// Code is the error code of an error response, "" otherwise.
func (r Resp) Code() string {
	var e struct {
		Code string `json:"code"`
	}
	if r.Err == nil && r.Status >= 400 && json.Unmarshal(r.Body, &e) == nil {
		return e.Code
	}
	return ""
}

func (r Resp) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

// Do sends one request. body is JSON-encoded when not nil; key is the Idempotency-Key
// when not empty.
func (c *Client) Do(ctx context.Context, token, method, path string, body any, key string) Resp {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return Resp{Err: err}
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return Resp{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Resp{Err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Resp{Err: err}
	}
	return Resp{Status: resp.StatusCode, Replayed: resp.Header.Get("Idempotent-Replayed") == "true", Body: b}
}

// User is a member of the workload with a token the api accepts.
type User struct {
	Name  string
	Token string
	ID    uuid.UUID
}

// NewUser mints a token and lets the api provision the user, as a first login does.
func (e *Env) NewUser(ctx context.Context, name string) (User, error) {
	tok, err := e.API.Token(name)
	if err != nil {
		return User{}, err
	}
	r := e.Client.Do(ctx, tok, "GET", "/v1/me", nil, "")
	var me struct {
		ID uuid.UUID `json:"id"`
	}
	if !r.OK() || json.Unmarshal(r.Body, &me) != nil {
		return User{}, fmt.Errorf("GET /v1/me for %s: %d %s %v", name, r.Status, r.Body, r.Err)
	}
	return User{Name: name, Token: tok, ID: me.ID}, nil
}

// NewGroup creates a group owned by members[0] and joins the others through a join code.
func (e *Env) NewGroup(ctx context.Context, members []User) (uuid.UUID, error) {
	owner := members[0]
	r := e.Client.Do(ctx, owner.Token, "POST", "/v1/groups", map[string]string{"name": "stress", "currency": "CAD"}, "")
	var g struct {
		ID uuid.UUID `json:"id"`
	}
	if r.Status != http.StatusCreated || json.Unmarshal(r.Body, &g) != nil {
		return uuid.Nil, fmt.Errorf("create group: %d %s %v", r.Status, r.Body, r.Err)
	}
	if len(members) == 1 {
		return g.ID, nil
	}
	r = e.Client.Do(ctx, owner.Token, "POST", fmt.Sprintf("/v1/groups/%s/join-codes", g.ID), map[string]int{"max_uses": len(members) - 1}, "")
	var jc struct {
		Code string `json:"code"`
	}
	if r.Status != http.StatusCreated || json.Unmarshal(r.Body, &jc) != nil {
		return uuid.Nil, fmt.Errorf("create join code: %d %s %v", r.Status, r.Body, r.Err)
	}
	for _, m := range members[1:] {
		r = e.Client.Do(ctx, m.Token, "POST", "/v1/join", map[string]string{"code": jc.Code}, uuid.NewString())
		if r.Status != http.StatusCreated {
			return uuid.Nil, fmt.Errorf("join: %d %s %v", r.Status, r.Body, r.Err)
		}
	}
	return g.ID, nil
}
