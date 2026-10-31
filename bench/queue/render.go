package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/riyne/fifty-fifty/internal/benchwork"
)

func render(r report) string {
	var b strings.Builder
	o, e := r.Options, r.Env
	fmt.Fprintf(&b, "Queue benchmarks: real worker processes, Postgres, mock LLM\n")
	fmt.Fprintf(&b, "Workers: %d slots each, poll %s, lease %s, max_attempts %d. Mock LLM latency: log-normal, median %s, sigma %.2f unless a scenario says 0 ms. %d run(s) per configuration.\n",
		o.Concurrency, o.Poll, o.Lease, o.MaxAttempts, o.LatencyMedian, o.LatencySigma, o.Runs)
	fmt.Fprintf(&b, "Commit %s (dirty: %v), %s, %d CPUs, %s bytes RAM, %s, %s\n", e.Commit, e.Dirty, e.Machine, e.CPUs, e.MemoryBytes, e.OS, e.GoVersion)
	fmt.Fprintf(&b, "Postgres: %s\nDocker postgres container: %s\nDocker engine: %s\nLoad average at start: %s\nRows in the jobs table at start: %d\n\n",
		e.Postgres, e.Docker, e.DockerInfo, e.LoadAvg, e.JobsRows)

	for _, scen := range o.Scenarios {
		var cs []configRun
		for _, c := range r.Configs {
			if c.Scenario == scen {
				cs = append(cs, c)
			}
		}
		if len(cs) == 0 {
			continue
		}
		switch scen {
		case "throughput-llm":
			fmt.Fprintf(&b, "== throughput-llm: closed loop, %d jobs per slot preloaded, mock LLM with simulated latency of %s median (sigma %.2f)\n", o.JobsPerSlot, o.LatencyMedian, o.LatencySigma)
			fmt.Fprintf(&b, "   jobs/s = (succeeded-1) / (first success to last success). Theoretical max = slots / mean latency.\n")
			for _, c := range cs {
				th := perRun(c, func(x benchwork.LoadResult) float64 { return x.ThroughputPerSec })
				d := benchwork.Distribution(th)
				fmt.Fprintf(&b, "   %-10s jobs/s per run %s: median %.1f (min %.1f, max %.1f); theoretical max %.1f; median/theoretical %.0f%%; %s\n",
					c.Label, floats(th), d.Median, d.Min, d.Max, c.TheoryRate, 100*d.Median/c.TheoryRate, outcome(c))
			}
		case "throughput-queue":
			fmt.Fprintf(&b, "== throughput-queue: closed loop, %d jobs preloaded, mock LLM with simulated latency of 0 ms (the handler still makes one local HTTP call and writes one row)\n", o.QueueBoundJobs)
			for _, c := range cs {
				th := perRun(c, func(x benchwork.LoadResult) float64 { return x.ThroughputPerSec })
				d := benchwork.Distribution(th)
				fmt.Fprintf(&b, "   %-10s jobs/s per run %s: median %.0f (min %.0f, max %.0f); %s\n", c.Label, floats(th), d.Median, d.Min, d.Max, outcome(c))
			}
		case "latency":
			fmt.Fprintf(&b, "== latency: open loop, Poisson arrivals at %.0f%% of theoretical capacity for %s, mock LLM with simulated latency of %s median (sigma %.2f)\n",
				100*o.Utilization, o.LatencyRun, o.LatencyMedian, o.LatencySigma)
			latencyTable(&b, cs)
		case "latency-low":
			fmt.Fprintf(&b, "== latency-low: open loop, Poisson arrivals at %.1f jobs/s for %s (workers mostly idle), mock LLM with simulated latency of %s median (sigma %.2f)\n",
				o.LowRate, o.LowRun, o.LatencyMedian, o.LatencySigma)
			latencyTable(&b, cs)
		case "retry":
			fmt.Fprintf(&b, "== retry: closed loop, %d jobs per run on %d workers, mock LLM with simulated latency of %s median (sigma %.2f), max_attempts %d, backoff: full jitter, base 2 s, cap 5 min\n",
				o.RetryJobs, o.RetryWorkers, o.LatencyMedian, o.LatencySigma, o.MaxAttempts)
			fmt.Fprintf(&b, "   Faults are drawn per request, mutually exclusive; the 429 retry-after header is not used by the worker (it backs off on its own schedule).\n")
			for _, c := range cs {
				var enq, ok, dead, attempts int
				var reqs, n429, n5, nbad int64
				hist := map[int]int{}
				for _, x := range c.Runs {
					enq += x.Enqueued
					ok += x.Succeeded
					dead += x.Dead
					attempts += x.TotalAttempts
					reqs += x.Mock.Requests
					n429 += x.Mock.RateLimited
					n5 += x.Mock.ServerError
					nbad += x.Mock.Malformed
					for a, k := range x.AttemptsHist {
						hist[a] += k
					}
				}
				pf := c.Faults["429"] + c.Faults["5xx"] + c.Faults["malformed_200"]
				fmt.Fprintf(&b, "   %-10s configured faults 429 %.0f%%, 5xx %.0f%%, malformed %.0f%% (any: %.0f%%)\n", c.Label,
					100*c.Faults["429"], 100*c.Faults["5xx"], 100*c.Faults["malformed_200"], 100*pf)
				fmt.Fprintf(&b, "              over %d runs: %d jobs, %d succeeded (%.2f%%), %d dead-lettered (%.2f%%; if every attempt failed independently with p=%.2f: %.2f%%)\n",
					len(c.Runs), enq, ok, 100*float64(ok)/float64(enq), dead, 100*float64(dead)/float64(enq), pf, 100*math.Pow(pf, float64(o.MaxAttempts)))
				fmt.Fprintf(&b, "              observed at the mock: %d requests, 429 %.1f%%, 5xx %.1f%%, malformed %.1f%%; %.3f attempts per job\n",
					reqs, pct(n429, reqs), pct(n5, reqs), pct(nbad, reqs), float64(attempts)/float64(enq))
				fmt.Fprintf(&b, "              jobs by number of attempts: %s\n", histString(hist, o.MaxAttempts))
				e2e99 := perRun(c, func(x benchwork.LoadResult) float64 { return x.EndToEnd.P99 })
				span := perRun(c, func(x benchwork.LoadResult) float64 { return x.SpanSecs })
				fmt.Fprintf(&b, "              succeeded jobs, enqueue to success: p99 per run %s s; first to last success per run %s s; %s\n", floats(e2e99), floats(span), outcome(c))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func perRun(c configRun, f func(benchwork.LoadResult) float64) []float64 {
	out := make([]float64, len(c.Runs))
	for i, r := range c.Runs {
		out[i] = f(r)
	}
	return out
}

func median(xs []float64) float64 { return benchwork.Distribution(xs).Median }

func floats(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		switch {
		case x >= 100:
			parts[i] = fmt.Sprintf("%.0f", x)
		case x >= 10:
			parts[i] = fmt.Sprintf("%.1f", x)
		default:
			parts[i] = fmt.Sprintf("%.3f", x)
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// outcome sums the integrity counts of a configuration's runs.
func outcome(c configRun) string {
	var enq, ok, dead, unfinished, dup, miss int
	for _, r := range c.Runs {
		enq += r.Enqueued
		ok += r.Succeeded
		dead += r.Dead
		unfinished += r.Unfinished
		dup += r.DuplicateFx
		miss += r.MissingFx
	}
	return fmt.Sprintf("all runs: %d jobs, %d succeeded, %d dead, %d unfinished, %d duplicate effects, %d missing effects", enq, ok, dead, unfinished, dup, miss)
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

func histString(h map[int]int, max int) string {
	var parts []string
	for a := 1; a <= max; a++ {
		parts = append(parts, fmt.Sprintf("%d: %d", a, h[a]))
	}
	return strings.Join(parts, ", ")
}

func latencyTable(b *strings.Builder, cs []configRun) {
	fmt.Fprintf(b, "   end-to-end = enqueue to success; queue wait = enqueue to the handler's start. Seconds; per-run p50/p99, then the median of those across runs.\n")
	for _, c := range cs {
		e2e50 := perRun(c, func(x benchwork.LoadResult) float64 { return x.EndToEnd.Median })
		e2e99 := perRun(c, func(x benchwork.LoadResult) float64 { return x.EndToEnd.P99 })
		w50 := perRun(c, func(x benchwork.LoadResult) float64 { return x.QueueWait.Median })
		w99 := perRun(c, func(x benchwork.LoadResult) float64 { return x.QueueWait.P99 })
		n := perRun(c, func(x benchwork.LoadResult) float64 { return float64(x.Enqueued) })
		fmt.Fprintf(b, "   %-10s offered %.2f jobs/s of a theoretical max %.1f (jobs per run %s); %s\n", c.Label, c.OfferedRate, c.TheoryRate, floats(n), outcome(c))
		fmt.Fprintf(b, "              end-to-end p50 %s -> %.3f   p99 %s -> %.3f\n", floats(e2e50), median(e2e50), floats(e2e99), median(e2e99))
		fmt.Fprintf(b, "              queue wait p50 %s -> %.3f   p99 %s -> %.3f\n", floats(w50), median(w50), floats(w99), median(w99))
	}
}
