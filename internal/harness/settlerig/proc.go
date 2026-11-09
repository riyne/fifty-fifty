package settlerig

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Bins are the built binaries the rig runs.
type Bins struct{ Worker, Mockpay, Mockllm string }

// Build compiles the worker, the mock provider and the mock LLM into dir.
func Build(ctx context.Context, dir string) (Bins, error) { return BuildWithTags(ctx, dir, "") }

// BuildWithTags is Build with go build tags (the negative-control runs build the worker
// with unsafe_controls). Never use it for anything that ships.
func BuildWithTags(ctx context.Context, dir, tags string) (Bins, error) {
	root, err := RepoRoot()
	if err != nil {
		return Bins{}, err
	}
	b := Bins{Worker: filepath.Join(dir, "worker"), Mockpay: filepath.Join(dir, "mockpay"), Mockllm: filepath.Join(dir, "mockllm")}
	for bin, pkg := range map[string]string{b.Worker: "./cmd/worker", b.Mockpay: "./cmd/mockpay", b.Mockllm: "./cmd/mockllm"} {
		args := []string{"build", "-o", bin}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		cmd := exec.CommandContext(ctx, "go", append(args, pkg)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			return Bins{}, fmt.Errorf("build %s: %v\n%s", pkg, err, out)
		}
	}
	return b, nil
}

// Proc is a running child process with its output captured.
type Proc struct {
	Name string
	cmd  *exec.Cmd
	done chan struct{}
	err  error

	mu  sync.Mutex
	log bytes.Buffer
}

type logWriter struct{ p *Proc }

func (w logWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.log.Write(b)
}

// Start runs bin with args and extra environment variables.
func Start(name, bin string, args, env []string) (*Proc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	p := &Proc{Name: name, cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = logWriter{p}, logWriter{p}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	return p, nil
}

// Log returns what the process has written so far (for a failing test to show).
func (p *Proc) Log() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.String()
}

// Done is closed when the process has exited.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Kill sends SIGKILL (no chance to clean up, like a crash or the OOM killer) and checks
// the process really died by that signal, so a test cannot pass by accident on a process
// that had already exited some other way.
func (p *Proc) Kill() error {
	select {
	case <-p.done:
		return fmt.Errorf("%s had already exited: %v", p.Name, p.err)
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	<-p.done
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return fmt.Errorf("%s did not die from SIGKILL: %v", p.Name, p.cmd.ProcessState)
	}
	return nil
}

// Stop asks the process to shut down (SIGTERM) and kills it if it does not.
func (p *Proc) Stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// FreeAddr returns a loopback address that was free a moment ago.
func FreeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close() //nolint:errcheck // a listener opened only to learn a free port
	return ln.Addr().String(), nil
}

// WorkerSpec is how to run one `worker run` process.
type WorkerSpec struct {
	Bin, DatabaseURL, PaymentsURL, Name string
	Concurrency                         int
	Lease, Poll                         time.Duration
	Env                                 []string // extra environment variables
}

// StartWorker runs a real worker binary against the database and provider.
func StartWorker(w WorkerSpec) (*Proc, error) {
	return Start(w.Name, w.Bin,
		[]string{"run", "--concurrency", strconv.Itoa(w.Concurrency), "--lease", w.Lease.String(), "--poll", w.Poll.String(), "--grace", "2s"},
		append([]string{"DATABASE_URL=" + w.DatabaseURL, "PAYMENTS_URL=" + w.PaymentsURL}, w.Env...))
}

// StartMockpay runs the mock provider on a free port and waits until it answers.
// flags are mockpay's own flags (rates, latency, webhook url ...); the webhook secret
// goes in the environment, not on the command line.
func StartMockpay(bin, webhookSecret string, flags ...string) (p *Proc, baseURL string, err error) {
	addr, err := FreeAddr()
	if err != nil {
		return nil, "", err
	}
	p, err = Start("mockpay", bin, append([]string{"--addr", addr}, flags...), []string{"MOCKPAY_WEBHOOK_SECRET=" + webhookSecret})
	if err != nil {
		return nil, "", err
	}
	baseURL = "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return nil, "", fmt.Errorf("mockpay exited at startup: %v\n%s", p.err, p.Log())
		default:
		}
		if resp, err := http.Get(baseURL + "/stats"); err == nil {
			resp.Body.Close()
			return p, baseURL, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.Stop()
	return nil, "", fmt.Errorf("mockpay did not start on %s\n%s", addr, p.Log())
}

// StartMockllm runs the mock LLM on a free port and waits until it answers. flags are
// mockllm's own (latency, fault rates, --fixtures ...).
func StartMockllm(bin string, flags ...string) (p *Proc, baseURL string, err error) {
	addr, err := FreeAddr()
	if err != nil {
		return nil, "", err
	}
	p, err = Start("mockllm", bin, append([]string{"--addr", addr}, flags...), nil)
	if err != nil {
		return nil, "", err
	}
	baseURL = "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return nil, "", fmt.Errorf("mockllm exited at startup: %v\n%s", p.err, p.Log())
		default:
		}
		if resp, err := http.Get(baseURL + "/stats"); err == nil {
			resp.Body.Close()
			return p, baseURL, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.Stop()
	return nil, "", fmt.Errorf("mockllm did not start on %s\n%s", addr, p.Log())
}
