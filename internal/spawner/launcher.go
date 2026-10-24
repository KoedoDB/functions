package spawner

// The workerd processes: one for each customer (and generation of its definition), started as the customer's own user.
//
// What a process gets, and from whom (docs/shared-vm.md):
//   - its user and group (UIDBase+n, no supplementary groups), set by the kernel at fork from the spawner's root, and no_new_privs and no capabilities (child.go, before workerd runs);
//   - a run directory it can only READ (root:<its group>, mode 0750; the files 0440): the configuration and the bundles. Both are deleted as soon as workerd answers, because workerd
//     has compiled them by then; the directory it keeps is empty;
//   - the listening socket as file descriptor 3, made by the spawner in the directory shared with the router (root:router's group, 0750: no customer can even look into it). A customer
//     cannot connect to another customer's socket, because it cannot reach it, and cannot take a socket path away, because it cannot write there;
//   - a clean environment: the secrets, under another name (FNSECRET_<NAME>), and nothing else. Only the process's own user can read /proc/<pid>/environ;
//   - limits applied from outside (limits.go): a cgroup when the spawner has one, and always the spawner's looks at the process, and RLIMIT_NPROC (per user: the customer's own).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
	"github.com/koedodb/functions/internal/wdconfig"
)

// errStale is a start for a generation that a put or a delete has replaced since the request chose it: the request chooses again.
var errStale = errors.New("the customer was changed or deleted")

// WProc is the workerd process of one customer's generation.
type WProc struct {
	Key      string // <customer>.<generation>
	Customer string
	UID      int
	Socket   string // what the router connects to

	cmd      *exec.Cmd
	runDir   string
	ready    chan struct{}
	readyErr error
	exited   chan struct{}
	done     chan struct{} // closed when the process is gone AND the pool knows it
	cg       *cgroup
	reason   atomic.Value // why the spawner killed it ("memory", "runaway", "timeout"), for the log
}

// launcher starts, stops and measures the workerd processes; it is what the pool is given.
type launcher struct {
	cfg    Config
	log    *slog.Logger
	pool   *residents.Pool[*WProc]
	lookup func(id string) *customer // the current definition of a customer, or nil

	// sampler makes the sampler of a process; the tests give their own.
	sampler func(p *WProc) sampler

	mu     sync.Mutex
	starts int
	seq    int
	live   map[string]int // processes of a customer that are running (real ones and probes): when it reaches 0 the user has nothing left
}

func newLauncher(cfg Config, log *slog.Logger, lookup func(string) *customer) *launcher {
	return &launcher{cfg: cfg, log: log, lookup: lookup, live: map[string]int{}}
}

func (l *launcher) child() string {
	if l.cfg.Child != "" {
		return l.cfg.Child
	}
	exe, err := os.Executable()
	if err != nil {
		return "/sup/supervisor"
	}
	return exe
}

// runSpec is what one start needs.
type runSpec struct {
	c        *customer
	name     string   // the name of the run directory and of the process in the log: unique among the live ones
	sock     string   // the socket workerd is given as file descriptor 3
	probe    bool     // a throwaway process that checks the bundles: no secrets, the socket is the spawner's own (root only)
	fns      []string // the functions in the configuration
	extraOut io.Writer
}

// begun is a process that has been started: wait for cmd with Wait, remove with cleanup (after Wait).
type begun struct {
	cmd     *exec.Cmd
	runDir  string
	cg      *cgroup
	cleanup func()
}

// begin makes the run directory, the configuration and the listening socket, and starts the process. The process may not be answering yet.
func (l *launcher) begin(rs runSpec) (*begun, error) {
	c := rs.c
	runDir := filepath.Join(l.cfg.State, "run", rs.name)
	if err := os.Mkdir(runDir, 0o750); err != nil {
		return nil, err
	}
	b := &begun{runDir: runDir}
	b.cleanup = func() {
		_ = os.RemoveAll(runDir)
		_ = os.Remove(rs.sock)
		if b.cg != nil {
			b.cg.remove()
		}
	}
	fail := func(err error) (*begun, error) { b.cleanup(); return nil, err }
	if err := os.Chown(runDir, 0, c.uid); err != nil {
		return fail(err)
	}
	if err := os.Chmod(runDir, 0o750); err != nil {
		return fail(err)
	}
	fns := make([]wdconfig.Function, 0, len(rs.fns))
	for _, name := range rs.fns {
		body, ok := c.fns[name]
		if !ok {
			return fail(fmt.Errorf("no function %q", name))
		}
		dir := filepath.Join(runDir, name)
		if err := os.Mkdir(dir, 0o750); err != nil {
			return fail(err)
		}
		if err := os.Chown(dir, 0, c.uid); err != nil {
			return fail(err)
		}
		if err := os.Chmod(dir, 0o750); err != nil { // not at the mercy of the umask
			return fail(err)
		}
		file := filepath.Join(dir, "index.mjs")
		if err := writeFile(file, body, c.uid); err != nil {
			return fail(err)
		}
		fns = append(fns, wdconfig.Function{Name: name, Bundle: file, Limits: c.fnLimits[name]})
	}
	var secretNames []string
	if !rs.probe {
		secretNames = c.secretNames()
	}
	text, err := wdconfig.Build(wdconfig.Spec{Base: runDir, CompatDate: l.cfg.Compat, Functions: fns, Secrets: secretNames, V8Flags: l.cfg.V8Flags})
	if err != nil {
		return fail(err)
	}
	cfgPath := filepath.Join(runDir, "config.capnp")
	if err := writeFile(cfgPath, []byte(text), c.uid); err != nil {
		return fail(err)
	}

	// The listening socket, with the permissions of its place: umask 077 (set at start) makes it 0700 root until it is chowned, so that nobody can connect in between.
	_ = os.Remove(rs.sock)
	ln, err := net.Listen("unix", rs.sock)
	if err != nil {
		return fail(err)
	}
	ul := ln.(*net.UnixListener)
	ul.SetUnlinkOnClose(false) // the process holds the socket from now on; its file is removed by cleanup
	if !rs.probe {
		if err := os.Chown(rs.sock, 0, l.cfg.RouterGID); err != nil {
			_ = ul.Close()
			return fail(err)
		}
		if err := os.Chmod(rs.sock, 0o660); err != nil {
			_ = ul.Close()
			return fail(err)
		}
	}
	lf, err := ul.File()
	_ = ul.Close()
	if err != nil {
		return fail(err)
	}
	defer lf.Close()

	// The environment: the secrets, under a prefix, and nothing else.
	env := make([]string, 0, len(secretNames)+1)
	for _, k := range secretNames {
		env = append(env, wdconfig.SecretEnvPrefix+k+"="+c.secrets[k])
	}
	cmd := exec.Command(l.child(), "child", "--nproc", strconv.Itoa(l.cfg.PidsLimit*4), l.cfg.Workerd, cfgPath)
	cmd.Dir = runDir
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{lf} // file descriptor 3
	if rs.extraOut != nil {
		cmd.Stdout, cmd.Stderr = rs.extraOut, rs.extraOut
	} else {
		w := newLineWriter(l.log, c.id)
		cmd.Stdout, cmd.Stderr = w, w
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: &syscall.Credential{Uid: uint32(c.uid), Gid: uint32(c.uid), Groups: []uint32{}}, // groups: none, set explicitly (setgroups)
	}
	if l.cfg.CgroupRoot != "" {
		cg, err := newCgroup(l.cfg.CgroupRoot, rs.name, c.limits.MemoryMiB, c.limits.CPUQuotaPct, c.limits.Pids)
		if err != nil {
			return fail(fmt.Errorf("the cgroup of %s: %w", c.id, err))
		}
		b.cg = cg
		if f, err := os.Open(cg.dir); err == nil && isCgroup2(cg.dir) {
			defer f.Close()
			cmd.SysProcAttr.UseCgroupFD = true // clone straight into the cgroup: no moment outside of the limits
			cmd.SysProcAttr.CgroupFD = int(f.Fd())
		}
	}
	l.addLive(c.id, 1)
	if err := cmd.Start(); err != nil {
		l.addLive(c.id, -1)
		return fail(err)
	}
	b.cmd = cmd
	return b, nil
}

func writeFile(path string, data []byte, gid int) error {
	if err := os.WriteFile(path, data, 0o440); err != nil {
		return err
	}
	if err := os.Chown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o440) // not at the mercy of the umask
}

// addLive counts the live processes of a customer; when the last one has gone it kills whatever else runs as the customer's user (a process a function managed to leave behind).
func (l *launcher) addLive(id string, d int) {
	l.mu.Lock()
	l.live[id] += d
	last := l.live[id] <= 0
	if last {
		delete(l.live, id)
	}
	l.mu.Unlock()
	if last && d < 0 {
		if c := l.lookup(id); c != nil {
			killAllOf(c.uid)
		}
	}
}

func (l *launcher) liveCount(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.live[id]
}

// start is the pool's hook: it starts the process of a generation of a customer.
func (l *launcher) start(key string) (*WProc, error) {
	id, gen, ok := splitKey(key)
	if !ok {
		return nil, fmt.Errorf("bad key %q", key)
	}
	c := l.lookup(id)
	if c == nil || c.gen != gen {
		return nil, errStale
	}
	l.mu.Lock()
	l.starts++
	l.seq++
	n := l.seq
	l.mu.Unlock()
	name := fmt.Sprintf("%s.%d", key, n) // the socket path has the number too: the router never reuses a connection to a dead process
	sock := filepath.Join(l.cfg.Control, "w-"+name+".sock")
	if len(sock) > 100 {
		return nil, fmt.Errorf("the socket path %q is too long (a Unix socket's is 107 bytes at most)", sock)
	}
	b, err := l.begin(runSpec{c: c, name: name, sock: sock, fns: c.names()})
	if err != nil {
		return nil, err
	}
	p := &WProc{Key: key, Customer: id, UID: c.uid, Socket: sock, cmd: b.cmd, runDir: b.runDir, ready: make(chan struct{}), exited: make(chan struct{}), done: make(chan struct{}), cg: b.cg}
	go func() {
		err := b.cmd.Wait()
		close(p.exited)
		reason, _ := p.reason.Load().(string)
		l.log.Info("a customer's process exited", "customer", id, "generation", gen, "error", err, "killed_for", reason)
		b.cleanup()
		l.pool.Exited(key, p)
		l.addLive(id, -1)
		close(p.done)
	}()
	go func() {
		p.readyErr = l.waitReady(sock, p.exited)
		if p.readyErr == nil {
			// workerd has compiled the configuration and the bundles: they are not needed on disk any more, and a customer's process has nothing to read.
			clearDir(p.runDir)
		}
		close(p.ready)
	}()
	go l.watch(p, c)
	return p, nil
}

// clearDir removes what is in dir and leaves it.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// waitReady waits until workerd answers. The socket exists from the start (the spawner made it), so connecting proves nothing: a request for a name that is no function is asked of the
// router Worker (404, and none of the customer's code runs). A process that stops by itself (a configuration or a bundle it does not accept) is an error.
func (l *launcher) waitReady(sock string, exited <-chan struct{}) error {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}, DisableKeepAlives: true}}
	deadline := time.Now().Add(l.cfg.StartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("workerd exited while starting") // a configuration it does not accept, or a bundle that does not load
		default:
		}
		resp, err := client.Get("http://ready/__ready__/")
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		select {
		case <-exited:
			return errors.New("workerd exited while starting")
		case <-time.After(20 * time.Millisecond):
		}
	}
	return errors.New("workerd did not start")
}

// watch looks at the limits of the process until it exits (limits.go): nearly all of the CPU quota for too long, or too much resident memory, and it is killed with SIGKILL (a looping
// workerd does not see SIGTERM).
func (l *launcher) watch(p *WProc, c *customer) {
	var s sampler
	if l.sampler != nil {
		s = l.sampler(p)
	} else if p.cg != nil && isCgroup2(p.cg.dir) {
		s = cgroupSampler{dir: p.cg.dir}
	} else {
		s = procSampler{pid: p.cmd.Process.Pid}
	}
	lim := limits{MemoryMiB: c.limits.MemoryMiB, CPUQuotaPct: c.limits.CPUQuotaPct, Look: l.cfg.Look, RunawayLooks: l.cfg.RunawayLooks}
	var st watchState
	// Memory is looked at every MemoryLook, the CPU every Look: the first is quick because a bomb fills a customer's memory in a second, the second is slow because a look at the CPU is
	// one step of the runaway rule ("nearly all of the quota for RunawayLooks looks in a row").
	step := l.cfg.MemoryLook
	if l.cfg.Look < step {
		step = l.cfg.Look
	}
	tick := time.NewTicker(step)
	defer tick.Stop()
	window := time.Now()
	for {
		select {
		case <-p.exited:
			return
		case <-tick.C:
		}
		cpu := int64(-1) // not a CPU look
		if time.Since(window) >= l.cfg.Look-step/2 {
			window = time.Now()
			if v, ok := s.CPUUsec(); ok {
				cpu = v
			}
		}
		rss, okRSS := s.RSSMiB()
		if !okRSS {
			rss = 0
		}
		if reason := lim.check(&st, cpu, rss); reason != "" {
			l.log.Warn("a customer's process is killed", "customer", p.Customer, "reason", reason, "rss_mib", rss)
			p.reason.Store(reason)
			l.kill(p)
			return
		}
	}
}

// stop stops the process gently first (SIGTERM), then for certain (SIGKILL): the whole process group, which is the process and what it started.
func (l *launcher) stop(p *WProc) {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
	}
}

// kill stops the process at once and returns when the pool no longer counts it (the next request starts a fresh process). It must not be called from the pool's own hooks.
func (l *launcher) kill(p *WProc) {
	select {
	case <-p.exited:
	default:
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
	}
	<-p.done
}

// rss is the resident memory of the process in MiB.
func (l *launcher) rss(p *WProc) (float64, bool) {
	if p.cmd.Process == nil {
		return 0, false
	}
	return procSampler{pid: p.cmd.Process.Pid}.RSSMiB()
}

// splitKey is "<customer>.<generation>".
func splitKey(key string) (id string, gen uint64, ok bool) {
	i := strings.LastIndexByte(key, '.')
	if i < 1 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(key[i+1:], 10, 64)
	return key[:i], n, err == nil && wdconfig.NameRE.MatchString(key[:i])
}

func makeKey(id string, gen uint64) string { return id + "." + strconv.FormatUint(gen, 10) }

// killAllOf kills every process that runs as uid (real user id), for the user ids of customers: when a customer has no process left that the spawner knows of, nothing of the user may
// stay (a user id is given to another customer later).
func killAllOf(uid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		if procUID(b) == uid {
			if syscall.Kill(pid, syscall.SIGKILL) == nil {
				killed++
			}
		}
	}
	return killed
}

// countOf is the number of processes (not zombies) that run as uid.
func countOf(uid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil || bytes.Contains(b, []byte("\nState:\tZ")) {
			continue
		}
		if procUID(b) == uid {
			n++
		}
	}
	return n
}

// procUID is the real user id of a /proc/<pid>/status text, or -1.
func procUID(status []byte) int {
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) >= 1 {
				if n, err := strconv.Atoi(f[0]); err == nil {
					return n
				}
			}
		}
	}
	return -1
}

// lineWriter turns what a customer's process writes (console.log, workerd's own messages) into log lines of the spawner: one record for each line, the customer named, a line cut at 2 KiB,
// at most 200 lines a second for a process (the rest are counted and dropped). The text is the customer's: slog quotes it, so a line cannot pass for another record.
type lineWriter struct {
	log      *slog.Logger
	customer string

	mu      sync.Mutex
	buf     []byte
	second  int64
	count   int
	dropped int
}

func newLineWriter(log *slog.Logger, customer string) *lineWriter {
	return &lineWriter{log: log, customer: customer}
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 2048 { // a line without an end: the start of it is the line
		w.line(string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) line(s string) {
	now := time.Now().Unix()
	if now != w.second {
		if w.dropped > 0 {
			w.log.Warn("function output dropped", "customer", w.customer, "lines", w.dropped)
		}
		w.second, w.count, w.dropped = now, 0, 0
	}
	if w.count >= 200 {
		w.dropped++
		return
	}
	w.count++
	if len(s) > 2048 {
		s = s[:2048]
	}
	w.log.Info("function output", "customer", w.customer, "line", s)
}
