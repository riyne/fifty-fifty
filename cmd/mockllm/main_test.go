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

	"github.com/riyne/fifty-fifty/internal/mockllm"
)

func TestParse_Defaults(t *testing.T) {
	addr, cfg, err := parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:8089" {
		t.Errorf("addr %q: the default must be loopback, the server has no authentication", addr)
	}
	if cfg.LatencyMedian != 800*time.Millisecond || cfg.LatencySigma != 0.4 || cfg.Seed != 1 ||
		cfg.RateTimeout+cfg.Rate429+cfg.Rate5xx+cfg.RateMalformed != 0 {
		t.Errorf("defaults: %+v", cfg)
	}
}

func TestParse_Flags(t *testing.T) {
	_, cfg, err := parse([]string{"--latency-median", "50ms", "--latency-sigma", "0", "--rate-429", "0.1", "--rate-5xx", "0.2",
		"--rate-malformed", "0.3", "--rate-timeout", "0.05", "--timeout-hold", "3s", "--retry-after", "2s", "--seed", "9"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := mockllm.Config{LatencyMedian: 50 * time.Millisecond, Rate429: 0.1, Rate5xx: 0.2, RateMalformed: 0.3, RateTimeout: 0.05,
		TimeoutHold: 3 * time.Second, RetryAfter: 2 * time.Second, Seed: 9}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestRun_RejectsBadInput(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":       {"--nope"},
		"positional":         {"extra"},
		"rates over one":     {"--rate-429", "0.7", "--rate-5xx", "0.7"},
		"negative rate":      {"--rate-429", "-0.1"},
		"sigma out of range": {"--latency-sigma", "5"},
		"help":               {"-h"},
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
	cfg := mockllm.Config{RateTimeout: 1, TimeoutHold: time.Hour}
	s, err := mockllm.New(cfg)
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
		_, err := http.Post(base+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{}]}`))
		held <- err
	}()
	for s.Stats().Timeouts == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop with its context while a connection was held")
	}
	if err := <-held; err == nil {
		t.Fatal("the held request got a response")
	}
}
