package payclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/payclient"
)

func payment(status string) string {
	code := "null"
	if status == "failed" {
		code = `"card_declined"`
	}
	return `{"id":"pay_1","idempotency_key":"k","reference":"r","amount_minor":700,"currency":"CAD","status":"` + status + `","failure_code":` + code + `}`
}

// serve answers every request with status and body, and records the last request.
func serve(t *testing.T, status int, resp string) (*payclient.Client, *http.Request, *string) {
	t.Helper()
	var last http.Request
	var lastBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last, lastBody = *r.Clone(r.Context()), string(b)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(ts.Close)
	c, err := payclient.New(ts.URL, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c, &last, &lastBody
}

var req = payclient.Request{Reference: "r", From: "a", To: "b", AmountMinor: 700, Currency: "CAD"}

func TestCreate_DefiniteAnswers(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
		code   string
	}{
		{201, payment("succeeded"), "succeeded", ""},
		{200, payment("succeeded"), "succeeded", ""},
		{201, payment("failed"), "failed", "card_declined"},
	} {
		c, _, _ := serve(t, tc.status, tc.body)
		p, err := c.Create(context.Background(), "k", req)
		if err != nil || p.Status != tc.want || (tc.code != "" && (p.FailureCode == nil || *p.FailureCode != tc.code)) {
			t.Errorf("%d %s: %+v, %v", tc.status, tc.want, p, err)
		}
	}
}

func TestCreate_SendsTheKeyAndTheRequest(t *testing.T) {
	c, last, lastBody := serve(t, 201, payment("succeeded"))
	if _, err := c.Create(context.Background(), "k", req); err != nil {
		t.Fatal(err)
	}
	if last.Method != "POST" || last.URL.Path != "/v1/payments" || last.Header.Get("Idempotency-Key") != "k" ||
		last.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request: %s %s %v", last.Method, last.URL.Path, last.Header)
	}
	if want := `{"reference":"r","from":"a","to":"b","amount_minor":700,"currency":"CAD"}`; *lastBody != want {
		t.Errorf("body %s, want %s", *lastBody, want)
	}
}

// What matters most: which results mean "we do not know" and which mean "no".
func TestCreate_Classification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"500", 500, `{"error":{}}`, payclient.ErrAmbiguous},
		{"503", 503, ``, payclient.ErrAmbiguous},
		{"529", 529, ``, payclient.ErrAmbiguous},
		{"429: not processed, but not a payment answer either", 429, ``, payclient.ErrAmbiguous},
		{"408", 408, ``, payclient.ErrAmbiguous},
		{"400", 400, `{"error":{"message":"bad"}}`, payclient.ErrRejected},
		{"422 key reuse", 422, `{"error":{"type":"idempotency_key_reuse"}}`, payclient.ErrRejected},
		{"401", 401, ``, payclient.ErrRejected},
		{"404", 404, ``, payclient.ErrRejected},
		{"200 with a body that is not JSON", 200, `<html>`, payclient.ErrAmbiguous},
		{"200 with an empty body", 200, ``, payclient.ErrAmbiguous},
		{"200 for another key", 200, strings.Replace(payment("succeeded"), `"k"`, `"other"`, 1), payclient.ErrAmbiguous},
		{"200 with a non-final status", 200, payment("processing"), payclient.ErrAmbiguous},
		{"200 with an unknown status", 200, payment("refunded"), payclient.ErrAmbiguous},
		{"200 with no status", 200, `{"idempotency_key":"k"}`, payclient.ErrAmbiguous},
	} {
		c, _, _ := serve(t, tc.status, tc.body)
		_, err := c.Create(context.Background(), "k", req)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if tc.want == payclient.ErrAmbiguous && errors.Is(err, payclient.ErrRejected) {
			t.Errorf("%s: both ambiguous and rejected", tc.name)
		}
	}
}

func TestCreate_TransportFailuresAreAmbiguous(t *testing.T) {
	// A connection the server drops without answering.
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }))
	defer drop.Close()
	// A server slower than the client's timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	// Nothing listening.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	for name, url := range map[string]string{"dropped connection": drop.URL, "timeout": slow.URL, "nothing listening": gone.URL} {
		c, _ := payclient.New(url, 100*time.Millisecond)
		if _, err := c.Create(context.Background(), "k", req); !errors.Is(err, payclient.ErrAmbiguous) {
			t.Errorf("%s: err = %v, want ErrAmbiguous", name, err)
		}
		if _, err := c.Get(context.Background(), "k"); !errors.Is(err, payclient.ErrAmbiguous) {
			t.Errorf("%s (get): err = %v, want ErrAmbiguous", name, err)
		}
	}
}

// The caller's own cancellation says nothing about the payment: it must not be
// reported as an unknown outcome, or a shutdown would turn into reconciliation work.
func TestCancellationIsNotAmbiguous(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer slow.Close()
	c, _ := payclient.New(slow.URL, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := c.Create(ctx, "k", req)
	if !errors.Is(err, context.Canceled) || errors.Is(err, payclient.ErrAmbiguous) {
		t.Errorf("Create: err = %v, want context.Canceled and not ErrAmbiguous", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.Get(ctx, "k")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, payclient.ErrAmbiguous) {
		t.Errorf("Get: err = %v, want DeadlineExceeded and not ErrAmbiguous", err)
	}
}

func TestGet(t *testing.T) {
	c, last, _ := serve(t, 200, payment("succeeded"))
	p, err := c.Get(context.Background(), "k")
	if err != nil || p.Status != "succeeded" {
		t.Fatalf("%+v, %v", p, err)
	}
	if last.Method != "GET" || last.URL.Path != "/v1/payments" || last.URL.Query().Get("idempotency_key") != "k" {
		t.Errorf("request: %s %s", last.Method, last.URL)
	}
	// A key with characters that need escaping reaches the provider intact.
	c, last, _ = serve(t, 404, ``)
	_, _ = c.Get(context.Background(), "a b&c=d")
	if got := last.URL.Query().Get("idempotency_key"); got != "a b&c=d" {
		t.Errorf("key arrived as %q", got)
	}

	for _, tc := range []struct {
		status int
		want   error
	}{{404, payclient.ErrNotFound}, {503, payclient.ErrAmbiguous}, {500, payclient.ErrAmbiguous}, {400, payclient.ErrRejected}} {
		c, _, _ := serve(t, tc.status, ``)
		if _, err := c.Get(context.Background(), "k"); !errors.Is(err, tc.want) {
			t.Errorf("get %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	c, _, _ = serve(t, 200, payment("processing"))
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, payclient.ErrAmbiguous) {
		t.Errorf("non-final status: err = %v", err)
	}
}

func TestNew(t *testing.T) {
	for _, bad := range []string{"", "localhost:8090", "ftp://x", "http://", "://x", "not a url"} {
		if _, err := payclient.New(bad, time.Second); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
	c, err := payclient.New("http://127.0.0.1:8090/", 0)
	if err != nil || c.BaseURL != "http://127.0.0.1:8090" || c.HTTP.Timeout != 10*time.Second {
		t.Errorf("%+v, %v", c, err)
	}
}
