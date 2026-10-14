package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
)

// Proc is the process of one function.
type Proc struct {
	Name string

	cmd       *exec.Cmd
	sock      string
	ready     chan struct{} // closed when the process listens, or has failed to
	readyErr  error
	exited    chan struct{}
	transport *http.Transport
}

// launcher starts, stops and measures the processes; it is what the pool is given (residents.Hooks).
type launcher struct {
	cfg  Config
	pool *residents.Pool[*Proc]
	logf func(format string, args ...any)
}

// bundlePath is the file a function is served from.
func (l *launcher) bundlePath(name string) string {
	return filepath.Join(l.cfg.Functions, name, "index.mjs")
}

func (l *launcher) start(name string) (*Proc, error) {
	dir := filepath.Join(l.cfg.Functions, name)
	sock := filepath.Join(l.cfg.Control, "fn-"+name+".sock")
	scratch := filepath.Join(l.cfg.Control, "fn-"+name)
	if len(sock) > 100 {
		return nil, fmt.Errorf("the socket path %q is too long (a Unix socket's is 107 bytes at most)", sock)
	}
	_ = os.Remove(sock)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	// The process gets permission to read its own directory (the bundle, and what it imports next to it), the runner, /tmp and its scratch directory, and to write /tmp and its
	// scratch directory. Nothing else of the file system; no child process, no worker, no native addon (Node denies them under --permission).
	args := []string{
		"--permission",
		"--allow-fs-read=" + l.cfg.Runner,
		"--allow-fs-read=" + dir,
		"--allow-fs-read=" + scratch,
		"--allow-fs-read=/tmp",
		"--allow-fs-write=" + scratch,
		"--allow-fs-write=/tmp",
	}
	if l.cfg.HeapMB > 0 {
		args = append(args, "--max-old-space-size="+strconv.Itoa(l.cfg.HeapMB))
	}
	args = append(args, l.cfg.Runner, l.bundlePath(name))

	env := []string{"FN_SOCKET=" + sock, "NODE_ENV=production", "NO_COLOR=1"}
	for _, k := range l.cfg.EnvAllow {
		if v := l.cfg.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd := exec.Command(l.cfg.Node, args...)
	cmd.Env = env // a clean environment: the socket, and the variables the function is allowed to read
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proc{Name: name, cmd: cmd, sock: sock, ready: make(chan struct{}), exited: make(chan struct{})}
	p.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true, // the function's answer goes on to the client as it is
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
		p.transport.CloseIdleConnections()
		_ = os.Remove(sock)
		l.pool.Exited(name, p)
	}()
	go func() {
		p.readyErr = l.waitForSocket(p)
		close(p.ready)
	}()
	return p, nil
}

func (l *launcher) waitForSocket(p *Proc) error {
	deadline := time.Now().Add(l.cfg.StartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			return errors.New("the function exited while starting") // a bundle that does not load, or does not export a handler
		default:
		}
		if c, err := net.DialTimeout("unix", p.sock, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("the function did not start")
}

// stop stops the process gently first (SIGTERM: finish what is in flight, a moment), then for certain (SIGKILL), and returns when it is gone.
func (l *launcher) stop(p *Proc) {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

// kill stops the process at once (a request that overran: every request running in it fails).
func (l *launcher) kill(p *Proc) {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.exited
}

// rss is the resident memory of the process in MiB (VmRSS of /proc/<pid>/status), when it can be read.
func (l *launcher) rss(p *Proc) (float64, bool) {
	if p.cmd.Process == nil {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p.cmd.Process.Pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			f := strings.Fields(rest)
			if len(f) >= 1 {
				if kb, err := strconv.ParseFloat(f[0], 64); err == nil {
					return kb / 1024, true
				}
			}
		}
	}
	return 0, false
}
