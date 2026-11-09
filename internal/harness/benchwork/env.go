package benchwork

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Env is the environment a benchmark ran in, recorded with its results.
type Env struct {
	Machine     string `json:"machine"`
	CPUs        int    `json:"cpus"`
	MemoryBytes string `json:"memory_bytes"`
	OS          string `json:"os"`
	GoVersion   string `json:"go_version"`
	Postgres    string `json:"postgres_version"`
	Docker      string `json:"docker_postgres_container"`
	DockerInfo  string `json:"docker_engine"`
	LoadAvg     string `json:"load_average_at_start"`
	JobsRows    int64  `json:"jobs_table_rows_at_start"`
	Commit      string `json:"commit"`
	Dirty       bool   `json:"working_tree_dirty"`
}

// CaptureEnv reads what it can about the machine, Docker, Postgres and git. A
// field it cannot read says "unavailable" rather than failing the run.
func CaptureEnv(ctx context.Context, ownerURL string) (Env, error) {
	sh := func(name string, args ...string) string {
		out, err := exec.Command(name, args...).Output()
		if err != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(out))
	}
	e := Env{
		CPUs: runtime.NumCPU(), GoVersion: runtime.Version(), OS: sh("uname", "-srm"),
		Machine: sh("sysctl", "-n", "machdep.cpu.brand_string"), MemoryBytes: sh("sysctl", "-n", "hw.memsize"),
		LoadAvg: sh("uptime"), Commit: sh("git", "rev-parse", "HEAD"),
		// bench/results is where the benchmarks write, so it must not count as dirty.
		Dirty: sh("git", "status", "--porcelain", "--", ".", ":(exclude)bench/results") != "",
	}
	pool, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		return e, err
	}
	defer pool.Close()
	if err := pool.QueryRow(ctx, `SELECT version()`).Scan(&e.Postgres); err != nil {
		return e, fmt.Errorf("postgres version: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&e.JobsRows); err != nil {
		return e, fmt.Errorf("count jobs: %w", err)
	}
	// Docker Desktop runs containers in a VM; the engine's totals bound them.
	name := sh("docker", "ps", "--filter", "publish=5433", "--format", "{{.Names}}")
	if name != "" && name != "unavailable" {
		e.Docker = sh("docker", "inspect", "-f", "{{.Name}} image={{.Config.Image}} memory_limit={{.HostConfig.Memory}} nano_cpus={{.HostConfig.NanoCpus}} (0 means no limit)", name)
	} else {
		e.Docker = "unavailable"
	}
	e.DockerInfo = sh("docker", "info", "-f", "cpus={{.NCPU}} memory_bytes={{.MemTotal}} server={{.ServerVersion}}")
	return e, nil
}
