package mockllm

import "time"

// LatencyFor exposes the latency formula so its distribution can be tested without
// sleeping through tens of thousands of simulated responses.
func LatencyFor(c Config, z float64) time.Duration { return c.latency(z) }
