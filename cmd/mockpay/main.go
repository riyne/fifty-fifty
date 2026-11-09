// Command mockpay is a mock payment provider with idempotency keys, simulated latency,
// injected faults and signed webhooks, for building and benchmarking settlement.
// Settlement in this project is SIMULATED: nothing here moves money. See internal/harness/mockpay
// for what it does and does not simulate. It has no authentication: bind it to loopback
// or a private network only.
//
//	mockpay --latency-median 300ms --rate-timeout-after 0.05 --webhook-url http://127.0.0.1:8080/v1/webhooks/payments --seed 1
//
// Every benchmark result that uses it must say "mock payment provider with simulated
// latency of X ms" and give the fault rates; the startup line it logs records the exact
// configuration (never the webhook secret).
package main

import (
	"context"
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

	"github.com/riyne/fifty-fifty/internal/harness/mockpay"
)

const usage = `usage: mockpay [flags]

POST /v1/payments (Idempotency-Key required) creates a payment; GET /v1/payments?idempotency_key=K
reads one; GET /stats returns counters and GET /debug/payments what the provider holds, as JSON.
There is no authentication.

The webhook secret is read from the flag or, if the flag is empty, from the environment
variable MOCKPAY_WEBHOOK_SECRET (a flag value is visible in the process list).

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
		slog.Error("mockpay exited", "err", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stderr io.Writer) error {
	addr, cfg, err := parse(args, stderr, os.Getenv("MOCKPAY_WEBHOOK_SECRET"))
	if err != nil {
		return err
	}
	srv, err := mockpay.New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	defer srv.Close()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(ctx, ln, srv, cfg)
}

// serve answers on ln until ctx is canceled. Shutdown first cancels the requests in
// flight (a held timeout fault would otherwise keep the server up for as long as its
// hold) and the pending webhooks, then stops listening.
func serve(ctx context.Context, ln net.Listener, s *mockpay.Server, cfg mockpay.Config) error {
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	defer cancelReqs()
	hs := &http.Server{
		Handler:           s.Handler(),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No WriteTimeout: it would cut off the simulated delays and timeout holds.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 14,
	}
	slog.Info("mockpay listening", "addr", ln.Addr().String(),
		"latency_median", cfg.LatencyMedian.String(), "latency_sigma", cfg.LatencySigma,
		"rate_decline", cfg.RateDecline, "rate_timeout_before", cfg.RateTimeoutBefore, "rate_5xx_before", cfg.Rate5xxBefore,
		"rate_timeout_after", cfg.RateTimeoutAfter, "rate_5xx_after", cfg.Rate5xxAfter, "rate_read_error", cfg.RateReadError,
		"timeout_hold", cfg.TimeoutHold.String(), "seed", cfg.Seed,
		"webhook_url", cfg.WebhookURL, "webhook_delay_max", cfg.WebhookDelayMax.String(),
		"webhook_drop", cfg.WebhookRateDrop, "webhook_duplicate", cfg.WebhookRateDuplicate, "webhook_out_of_order", cfg.WebhookRateOutOfOrder)

	go func() {
		<-ctx.Done()
		cancelReqs()
		s.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdownCtx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("mockpay stopped", "stats", s.Stats())
	return nil
}

func parse(args []string, stderr io.Writer, envSecret string) (addr string, cfg mockpay.Config, err error) {
	fs := flag.NewFlagSet("mockpay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&addr, "addr", "127.0.0.1:8090", "listen address")
	fs.DurationVar(&cfg.LatencyMedian, "latency-median", 300*time.Millisecond, "median response time of processed requests")
	fs.Float64Var(&cfg.LatencySigma, "latency-sigma", 0.4, "log-normal sigma of the latency; 0 is a fixed delay")
	fs.Float64Var(&cfg.RateDecline, "rate-decline", 0, "fraction of new payments created as failed (a definite decline)")
	fs.Float64Var(&cfg.RateTimeoutBefore, "rate-timeout-before", 0, "fraction of create calls held then aborted BEFORE the payment is created")
	fs.Float64Var(&cfg.Rate5xxBefore, "rate-5xx-before", 0, "fraction of create calls answered 500 BEFORE the payment is created")
	fs.Float64Var(&cfg.RateTimeoutAfter, "rate-timeout-after", 0, "fraction of create calls where the payment IS created, then the connection is held and aborted")
	fs.Float64Var(&cfg.Rate5xxAfter, "rate-5xx-after", 0, "fraction of create calls where the payment IS created, then 500 is returned")
	fs.Float64Var(&cfg.RateReadError, "rate-read-error", 0, "fraction of GET by key answered 503")
	fs.DurationVar(&cfg.TimeoutHold, "timeout-hold", time.Minute, "how long a timeout fault holds the connection before aborting it")
	fs.Uint64Var(&cfg.Seed, "seed", 1, "seed; faults are a function of (seed, idempotency key, call number)")
	fs.StringVar(&cfg.WebhookURL, "webhook-url", "", "where to send signed webhooks; empty means none")
	fs.StringVar(&cfg.WebhookSecret, "webhook-secret", "", "HMAC secret for webhooks (default: $MOCKPAY_WEBHOOK_SECRET)")
	fs.DurationVar(&cfg.WebhookDelayMax, "webhook-delay-max", 0, "each webhook is delayed by a uniform draw in [0, max)")
	fs.Float64Var(&cfg.WebhookRateDrop, "webhook-drop", 0, "fraction of webhook events never sent")
	fs.Float64Var(&cfg.WebhookRateDuplicate, "webhook-duplicate", 0, "fraction of webhook events sent twice")
	fs.Float64Var(&cfg.WebhookRateOutOfOrder, "webhook-out-of-order", 0, "fraction of payments whose terminal event overtakes the processing event (needs --webhook-delay-max)")
	if err := fs.Parse(args); err != nil {
		return "", cfg, errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return "", cfg, errUsage
	}
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = envSecret
	}
	return addr, cfg, nil
}
