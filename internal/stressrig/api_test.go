package stressrig_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/stressrig"
	"github.com/riyne/fifty-fifty/internal/testutil"
)

// TestStartAPI shows the api can be started and given real tokens without a
// testing.TB, which is the whole point of loosening authtest: a benchmark program does
// this. The rig's T is used; the *testing.T below is only for the database and for
// reporting.
func TestStartAPI(t *testing.T) {
	pool := testutil.Pool(t)
	api, err := stressrig.StartAPI(context.Background(), pool, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)

	get := func(token string) (int, string) {
		req, _ := http.NewRequest("GET", api.URL+"/v1/me", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	tok, err := api.Token("rig")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := get(tok); code != 200 || !strings.Contains(body, `"id"`) {
		t.Fatalf("/v1/me with a minted token = %d %s", code, body)
	}
	if code, _ := get(""); code != 401 {
		t.Fatalf("/v1/me without a token = %d, want 401", code)
	}
	if code, _ := get(tok + "x"); code != 401 {
		t.Fatalf("/v1/me with a tampered token = %d, want 401", code)
	}
	other, err := api.Token("rig")
	if err != nil || other == tok {
		t.Fatalf("every Token call must be a new user: %v", err)
	}
}
