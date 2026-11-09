// Command mockllm is a mock of the Anthropic Messages API with simulated latency and
// injected faults, for benchmarks and tests. See internal/harness/mockllm for what it does and
// does not simulate. It has no authentication and ignores API keys: bind it to
// loopback or a private network only.
//
//	mockllm --latency-median 800ms --latency-sigma 0.4 --rate-429 0.05 --seed 1
//
// Every benchmark result that uses it must say "mock LLM with simulated latency of
// X ms" (with the median and sigma), and the startup line it logs records the exact
// configuration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/riyne/fifty-fifty/internal/harness/mockllm"
)

const usage = `usage: mockllm [flags]

POST /v1/messages answers like the Anthropic Messages API; GET /stats returns
request and fault counters as JSON. There is no authentication.

flags:
`

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		slog.Error("mockllm exited", "err", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stderr io.Writer) error {
	addr, cfg, err := parse(args, stderr)
	if err != nil {
		return err
	}
	srv, err := mockllm.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, srv, cfg)
}

// serve answers on ln until ctx is canceled. Shutdown first cancels the requests in
// flight (a held timeout fault or a long simulated delay would otherwise keep the
// server up for as long as its hold), then stops listening.
func serve(ctx context.Context, ln net.Listener, s *mockllm.Server, cfg mockllm.Config) error {
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	defer cancelReqs()
	hs := &http.Server{
		Handler:           s.Handler(),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No WriteTimeout: it would cut off the simulated delays and timeout holds,
		// which are the point of this server.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 14,
	}
	slog.Info("mockllm listening", "addr", ln.Addr().String(),
		"latency_median", cfg.LatencyMedian.String(), "latency_sigma", cfg.LatencySigma,
		"rate_timeout", cfg.RateTimeout, "rate_429", cfg.Rate429, "rate_5xx", cfg.Rate5xx, "rate_malformed", cfg.RateMalformed,
		"timeout_hold", cfg.TimeoutHold.String(), "retry_after", cfg.RetryAfter.String(), "seed", cfg.Seed,
		"fixtures", len(cfg.Fixtures))

	go func() {
		<-ctx.Done()
		cancelReqs()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("mockllm stopped", "stats", s.Stats())
	return nil
}

func parse(args []string, stderr io.Writer) (addr string, cfg mockllm.Config, err error) {
	fs := flag.NewFlagSet("mockllm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&addr, "addr", "127.0.0.1:8089", "listen address")
	fs.DurationVar(&cfg.LatencyMedian, "latency-median", 800*time.Millisecond, "median response time of successful responses")
	fs.Float64Var(&cfg.LatencySigma, "latency-sigma", 0.4, "log-normal sigma of the latency; 0 is a fixed delay, 0.4 gives p99 about 2.5x the median")
	fs.Float64Var(&cfg.RateTimeout, "rate-timeout", 0, "fraction of requests held, then aborted with no response")
	fs.Float64Var(&cfg.Rate429, "rate-429", 0, "fraction of requests answered 429 with retry-after")
	fs.Float64Var(&cfg.Rate5xx, "rate-5xx", 0, "fraction of requests answered 500 or 529")
	fs.Float64Var(&cfg.RateMalformed, "rate-malformed", 0, "fraction of requests answered 200 with a truncated, unparseable body")
	fs.DurationVar(&cfg.TimeoutHold, "timeout-hold", time.Minute, "how long a timeout fault holds the connection before aborting it")
	fs.DurationVar(&cfg.RetryAfter, "retry-after", time.Second, "retry-after on a 429, rounded up to whole seconds")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "seed for the random draws")
	fixtures := fs.String("fixtures", "", "JSON file mapping a receipt reference number to the reply for a prompt containing \"REF <number>\" (see cmd/receiptgen); other prompts get the canned reply")
	if err := fs.Parse(args); err != nil {
		return "", cfg, errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return "", cfg, errUsage
	}
	if *fixtures != "" {
		f, err := loadFixtures(*fixtures)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return "", cfg, errUsage
		}
		cfg.Fixtures = f
	}
	return addr, cfg, nil
}

// maxFixtureFileBytes bounds the fixtures file; a few thousand receipts are well under it.
const maxFixtureFileBytes = 64 << 20

func loadFixtures(path string) (map[string]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxFixtureFileBytes {
		return nil, fmt.Errorf("fixtures file %s is larger than %d bytes", path, maxFixtureFileBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("fixtures file %s must be a JSON object of strings: %w", path, err)
	}
	return m, nil
}
