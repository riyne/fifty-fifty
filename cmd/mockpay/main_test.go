package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/riyne/fifty-fifty/internal/mockpay"
)

func TestParse_Defaults(t *testing.T) {
	addr, cfg, err := parse(nil, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:8090" {
		t.Errorf("addr %q: the default must be loopback, the server has no authentication", addr)
	}
	if cfg.LatencyMedian != 300*time.Millisecond || cfg.LatencySigma != 0.4 || cfg.Seed != 1 || cfg.WebhookURL != "" ||
		cfg.RateDecline+cfg.RateTimeoutBefore+cfg.Rate5xxBefore+cfg.RateTimeoutAfter+cfg.Rate5xxAfter+cfg.RateReadError != 0 {
		t.Errorf("defaults: %+v", cfg)
	}
}

func TestParse_FlagsAndSecretFromEnvironment(t *testing.T) {
	_, cfg, err := parse([]string{"--latency-median", "50ms", "--latency-sigma", "0", "--rate-decline", "0.1",
		"--rate-timeout-before", "0.02", "--rate-5xx-before", "0.03", "--rate-timeout-after", "0.04", "--rate-5xx-after", "0.05",
		"--rate-read-error", "0.2", "--timeout-hold", "3s", "--seed", "9", "--webhook-url", "http://x/hook",
		"--webhook-delay-max", "2s", "--webhook-drop", "0.1", "--webhook-duplicate", "0.2", "--webhook-out-of-order", "0.3"}, io.Discard, "from-env")
	if err != nil {
		t.Fatal(err)
	}
	want := mockpay.Config{LatencyMedian: 50 * time.Millisecond, RateDecline: 0.1, RateTimeoutBefore: 0.02, Rate5xxBefore: 0.03,
		RateTimeoutAfter: 0.04, Rate5xxAfter: 0.05, RateReadError: 0.2, TimeoutHold: 3 * time.Second, Seed: 9,
		WebhookURL: "http://x/hook", WebhookSecret: "from-env", WebhookDelayMax: 2 * time.Second,
		WebhookRateDrop: 0.1, WebhookRateDuplicate: 0.2, WebhookRateOutOfOrder: 0.3}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
	// The flag wins over the environment.
	_, cfg, _ = parse([]string{"--webhook-secret", "from-flag"}, io.Discard, "from-env")
	if cfg.WebhookSecret != "from-flag" {
		t.Errorf("secret = %q, want the flag's", cfg.WebhookSecret)
	}
}

func TestRun_RejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":               {"--nope"},
		"positional":                 {"extra"},
		"create rates over one":      {"--rate-decline", "0.6", "--rate-5xx-after", "0.6"},
		"negative rate":              {"--rate-decline", "-0.1"},
		"sigma out of range":         {"--latency-sigma", "5"},
		"webhook fault without url":  {"--webhook-drop", "0.5"},
		"webhook url without secret": {"--webhook-url", "http://x"},
		"help":                       {"-h"},
	} {
		var stderr bytes.Buffer
		err := run(context.Background(), args, &stderr)
		if !errors.Is(err, errUsage) || stderr.Len() == 0 {
			t.Errorf("%s: err=%v stderr=%q, want a usage error with a message", name, err, stderr.String())
		}
	}
}

// The server answers, and stops promptly on cancel even with a connection held by a
// timeout fault whose hold is an hour.
func TestServe_AnswersAndStopsWithTheContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := mockpay.Config{RateTimeoutBefore: 1, TimeoutHold: time.Hour}
	s, err := mockpay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, s, cfg) }()

	base := "http://" + ln.Addr().String()
	resp, err := http.Get(base + "/stats")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stats: %v %v", resp, err)
	}
	resp.Body.Close()

	held := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("POST", base+"/v1/payments",
			strings.NewReader(`{"reference":"r","from":"a","to":"b","amount_minor":5,"currency":"CAD"}`))
		req.Header.Set("Idempotency-Key", "k")
		_, err := http.DefaultClient.Do(req)
		held <- err
	}()
	for s.Stats().TimeoutBefore == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	t0 := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after the context was canceled")
	}
	if d := time.Since(t0); d > 8*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	if err := <-held; err == nil {
		t.Error("the held request got a response; a timeout fault must end with no response")
	}
}
