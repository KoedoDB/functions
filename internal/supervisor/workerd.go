package supervisor

// The workerd processes: ONE workerd process for each customer, all of the customer's functions in it.
//
// The unmodified workerd has no CPU time limit and no memory limit of its own, so the limits come from outside the process: a cgroup (memory.max, cpu.max, pids.max) when the supervisor
// has a writable cgroup v2 directory (WD_CGROUP_ROOT), and always the supervisor's own looks every WD_LOOK_MS at the process (limits.go): resident memory above WD_MEMORY_MIB, or nearly
// all of the CPU quota for WD_RUNAWAY_LOOKS looks in a row, and the process is killed. The next request starts a fresh one. doc/workerd.md says what each of the two can and cannot do
// inside a hardened Pod.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
)

// routerJS runs first in a customer's workerd: /<function>/<rest> goes to the function's Worker as /<rest>. The names of the bindings are F0, F1, ... (a function's name is not a
// valid identifier), and MAP says which is which.
const routerJS = `const MAP = __MAP__;
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const cut = url.pathname.indexOf('/', 1);
    const name = cut < 0 ? url.pathname.slice(1) : url.pathname.slice(1, cut);
    const binding = Object.hasOwn(MAP, name) ? MAP[name] : undefined;
    if (!binding) return new Response('no such function', { status: 404 });
    url.pathname = cut < 0 ? '/' : url.pathname.slice(cut);
    return env[binding].fetch(new Request(url, request));
  },
};
`

// wrapperJS turns the bundle of a function (export default (request) => Response, or an object with a fetch method: the contract of a function) into a Worker. The bundle is
// imported when the first request comes, not when workerd starts: a bundle with no default export is an exception of that request (500), not the end of the customer's process.
const wrapperJS = `let loading;
function load() {
  return (loading ??= import('bundle').then((mod) => {
    const d = mod.default;
    if (typeof d === 'function') return d;
    if (d && typeof d.fetch === 'function') return (request, env, ctx) => d.fetch(request, env, ctx);
    return null;
  }));
}
export default {
  async fetch(request, env, ctx) {
    let handler;
    try {
      handler = await load();
    } catch (err) {
      console.error('the bundle does not load:', err && err.stack ? err.stack : String(err));
      return new Response('function error', { status: 500, headers: { 'content-type': 'text/plain' } });
    }
    if (!handler) return new Response('the bundle must export default a function (request) => Response, or an object with a fetch method', { status: 500 });
    try {
      const response = await handler(request, env, ctx);
      if (!(response instanceof Response)) throw new TypeError('the function must return a Response');
      return response;
    } catch (err) {
      console.error('function error:', err && err.stack ? err.stack : String(err));
      return new Response('function error', { status: 500, headers: { 'content-type': 'text/plain' } });
    }
  },
};
`

// WorkerdFunction is one function of a customer.
type WorkerdFunction struct {
	Name   string
	Bundle string // the absolute path of index.mjs
}

// WorkerdSpec is what the configuration of one customer's workerd is made from.
type WorkerdSpec struct {
	Base       string            // the directory of the configuration file: workerd reads "embed" paths relative to it (it does not read an absolute one)
	Socket     string            // the Unix socket workerd listens on
	CompatDate string            // the compatibility date of every function
	Functions  []WorkerdFunction // at least one
	Env        map[string]string // the variables the functions may read (process.env, and env of the handler)
}

// capnpString is s as a Cap'n Proto text literal. Anything that is not printable ASCII or valid UTF-8 text outside the controls is written as \xNN.
func capnpString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c) // printable ASCII, or a byte of a UTF-8 sequence
		}
	}
	b.WriteByte('"')
	return b.String()
}

// BuildWorkerdConfig is the workerd configuration (Cap'n Proto text) of a customer: a router and a Worker for each function, on a Unix socket. It is deterministic (names and variables are sorted).
func BuildWorkerdConfig(sp WorkerdSpec) (string, error) {
	if len(sp.Functions) == 0 {
		return "", errors.New("no function")
	}
	if sp.Socket == "" || sp.CompatDate == "" || !filepath.IsAbs(sp.Base) {
		return "", errors.New("the socket, the compatibility date and the absolute directory of the configuration are needed")
	}
	fns := append([]WorkerdFunction(nil), sp.Functions...)
	sort.Slice(fns, func(i, j int) bool { return fns[i].Name < fns[j].Name })
	keys := make([]string, 0, len(sp.Env))
	for k := range sp.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envNameRE.MatchString(k) {
			return "", fmt.Errorf("%q is not a variable name", k)
		}
	}
	var mapJS strings.Builder
	mapJS.WriteByte('{')
	for i, f := range fns {
		if !nameRE.MatchString(f.Name) {
			return "", fmt.Errorf("%q is not a function name", f.Name)
		}
		if !filepath.IsAbs(f.Bundle) {
			return "", fmt.Errorf("the bundle of %q must be an absolute path", f.Name)
		}
		if _, err := filepath.Rel(sp.Base, f.Bundle); err != nil {
			return "", fmt.Errorf("the bundle of %q: %w", f.Name, err)
		}
		if i > 0 {
			mapJS.WriteString(", ")
		}
		fmt.Fprintf(&mapJS, "%q: %q", f.Name, "F"+strconv.Itoa(i))
	}
	mapJS.WriteByte('}')

	var b strings.Builder
	b.WriteString("using Workerd = import \"/workerd/workerd.capnp\";\n\nconst config :Workerd.Config = (\n  services = [\n")
	fmt.Fprintf(&b, "    (name = \"router\", worker = (\n      modules = [(name = \"worker\", esModule = %s)],\n      compatibilityDate = %s,\n      bindings = [", capnpString(strings.Replace(routerJS, "__MAP__", mapJS.String(), 1)), capnpString(sp.CompatDate))
	for i, f := range fns {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(name = \"F%d\", service = %s)", i, capnpString("fn-"+f.Name))
	}
	b.WriteString("])),\n")
	for _, f := range fns {
		fmt.Fprintf(&b, "    (name = %s, worker = (\n      modules = [(name = \"worker\", esModule = %s), (name = \"bundle\", esModule = embed %s)],\n      compatibilityDate = %s,\n      compatibilityFlags = [\"nodejs_compat_populate_process_env\"],\n      bindings = [",
			capnpString("fn-"+f.Name), capnpString(wrapperJS), capnpString(rel(sp.Base, f.Bundle)), capnpString(sp.CompatDate))
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(name = %s, text = %s)", capnpString(k), capnpString(sp.Env[k]))
		}
		b.WriteString("])),\n")
	}
	fmt.Fprintf(&b, "  ],\n  sockets = [(name = \"http\", address = %s, http = (), service = \"router\")],\n);\n", capnpString("unix:"+sp.Socket))
	return b.String(), nil
}

// bundleKey is a bundle as the file system tells it: a bundle that has changed is checked again.
type bundleKey struct {
	path  string
	size  int64
	mtime int64
}

func keyOf(path string) (bundleKey, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return bundleKey{}, false
	}
	return bundleKey{path: path, size: st.Size(), mtime: st.ModTime().UnixNano()}, true
}

// checkBundles tells which of the bundles workerd would not accept (a syntax error, a module that does not exist): it returns the reason for each. workerd compiles every module when it
// starts and stops at the first one it cannot, so one bad bundle would take the customer's other functions down with it. There is no other parser in the image, so workerd itself is the
// check: a throwaway process with the same configuration, no variables, and no request (the bundles are imported when a request comes, so none of the customer's code runs). One
// process for all the bundles that have not been checked yet; only when it fails, one for each, to find the bad ones. A bundle that passed is not checked again while its file stays the same.
func (l *wlauncher) checkBundles(customer, dir string, names []string) map[string]string {
	bad := map[string]string{}
	var todo []string
	keys := map[string]bundleKey{}
	l.mu.Lock()
	for _, n := range names {
		k, ok := keyOf(filepath.Join(dir, n, "index.mjs"))
		if !ok || !l.good[k] {
			todo = append(todo, n)
			keys[n] = k
		}
	}
	l.mu.Unlock()
	if len(todo) == 0 {
		return bad
	}
	mark := func(n string) {
		l.mu.Lock()
		if l.good == nil {
			l.good = map[bundleKey]bool{}
		}
		l.good[keys[n]] = true
		l.mu.Unlock()
	}
	if err := l.probe(customer, dir, todo); err == nil {
		for _, n := range todo {
			mark(n)
		}
		return bad
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, n := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(n string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := l.probe(customer, dir, []string{n}); err != nil {
				l.log.Warn("a bundle does not load and is left out", "customer", customer, "function", n, "output", err.Error())
				mu.Lock()
				bad[n] = "does not load"
				mu.Unlock()
				return
			}
			mark(n)
		}(n)
	}
	wg.Wait()
	return bad
}

// probe starts a throwaway workerd with the given functions and says whether it came up: it listens on its socket within a few seconds (it is stopped at once), or it stops by itself
// and the first line of what it said is the error.
func (l *wlauncher) probe(customer, dir string, names []string) error {
	l.mu.Lock()
	l.probes++
	id := l.probes
	l.mu.Unlock()
	work := filepath.Join(l.cfg.Control, "wp-"+strconv.Itoa(id))
	sock := work + ".sock"
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	defer os.Remove(sock)
	fns := make([]WorkerdFunction, 0, len(names))
	for _, n := range names {
		fns = append(fns, WorkerdFunction{Name: n, Bundle: filepath.Join(dir, n, "index.mjs")})
	}
	text, err := BuildWorkerdConfig(WorkerdSpec{Base: work, Socket: sock, CompatDate: l.cfg.CompatDate, Functions: fns})
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(work, "config.capnp")
	if err := os.WriteFile(cfgPath, []byte(text), 0o600); err != nil {
		return err
	}
	var out limitedBuffer
	cmd := exec.Command(l.cfg.Workerd, "serve", cfgPath)
	cmd.Dir = work
	cmd.Env = []string{}
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if l.uids != nil { // the same user as the customer's process: the probe reads the bundles as the process will
		uid, err := l.uids.uid(customer)
		if err != nil {
			return err
		}
		_ = os.Chown(work, uid, uid)
		_ = os.Chown(cfgPath, uid, uid)
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
	timeout := l.cfg.StartTimeout
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			first, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
			return errors.New(first)
		default:
		}
		if c, err := net.DialTimeout("unix", sock, 100*time.Millisecond); err == nil {
			_ = c.Close()
			// workerd opens its socket BEFORE it has made the services, and stops (with the error of the bundle) a moment later: ask the router for a name that is no function (404, and no
			// code of the customer runs), then look once more. Only a process that answered and is still there a moment after it has come up.
			client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}, DisableKeepAlives: true}}
			resp, gerr := client.Get("http://probe/__probe__/")
			if gerr == nil {
				_ = resp.Body.Close()
			}
			select {
			case <-exited:
			case <-time.After(300 * time.Millisecond):
				if gerr == nil {
					stop()
					return nil
				}
				stop()
				return gerr
			}
			first, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
			if first == "" && gerr != nil {
				first = gerr.Error()
			}
			return errors.New(first)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	return errors.New("workerd did not come up")
}

// limitedBuffer keeps the first 4 KiB of what a throwaway process says.
type limitedBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := 4096 - len(l.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}

// rel is path relative to base (workerd reads "embed" paths relative to the configuration file: "..", symbolic links and relative paths work; an absolute path does not).
func rel(base, path string) string {
	r, _ := filepath.Rel(base, path) // IsAbs of both was checked
	return r
}

// listFunctions is the functions in dir: the directories with an index.mjs and a valid name.
func listFunctions(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !nameRE.MatchString(e.Name()) {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, e.Name(), "index.mjs")); err == nil && st.Mode().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// WProc is the workerd process of one customer.
type WProc struct {
	Customer string

	cmd       *exec.Cmd
	sock      string
	ready     chan struct{}
	readyErr  error
	exited    chan struct{}
	transport *http.Transport
	cg        *cgroup
	reason    atomic.Value      // why the supervisor killed it ("memory", "runaway", "timeout"), for the log
	bad       map[string]string // the functions left out of the configuration, and why (a bundle that does not parse)
	done      chan struct{}     // closed when the process is gone AND the pool knows it
}

// wlauncher starts, stops and measures the workerd processes; it is what the pool is given.
type wlauncher struct {
	cfg  Config
	pool *residents.Pool[*WProc]
	log  *slog.Logger
	uids *uidAlloc
	// sampler makes the sampler of a process; the tests give their own.
	sampler func(p *WProc) sampler
	mu      sync.Mutex
	starts  int
	probes  int
	good    map[bundleKey]bool // bundles a throwaway workerd has accepted (checkBundles)
}

func (l *wlauncher) customerDir(customer string) string {
	if l.cfg.CustomersDir != "" {
		return filepath.Join(l.cfg.CustomersDir, customer)
	}
	return l.cfg.Functions
}

// env is the variables a customer's functions may read: the supervisor's allowed ones, and when there are customers, the customer's own secrets.json on top.
func (l *wlauncher) env(customer string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range l.cfg.EnvAllow {
		if v := l.cfg.Getenv(k); v != "" {
			out[k] = v
		}
	}
	if l.cfg.CustomersDir != "" {
		more, err := readSecrets(filepath.Join(l.customerDir(customer), "secrets.json"))
		if err != nil {
			return nil, err
		}
		for k, v := range more {
			out[k] = v
		}
	}
	return out, nil
}

func (l *wlauncher) start(customer string) (*WProc, error) {
	dir := l.customerDir(customer)
	names := listFunctions(dir)
	if len(names) == 0 {
		return nil, errors.New("the customer has no function")
	}
	sock := filepath.Join(l.cfg.Control, "wd-"+customer+".sock")
	work := filepath.Join(l.cfg.Control, "wd-"+customer)
	if len(sock) > 100 {
		return nil, fmt.Errorf("the socket path %q is too long (a Unix socket's is 107 bytes at most)", sock)
	}
	env, err := l.env(customer)
	if err != nil {
		return nil, err
	}
	// A bundle that does not load stops workerd at its start, and with it every other function of the customer: it is left out, and answers 502 (checkBundles).
	bad := l.checkBundles(customer, dir, names)
	fns := make([]WorkerdFunction, 0, len(names))
	for _, n := range names {
		if bad[n] == "" {
			fns = append(fns, WorkerdFunction{Name: n, Bundle: filepath.Join(dir, n, "index.mjs")})
		}
	}
	if len(fns) == 0 {
		return nil, errors.New("no function of the customer can be started")
	}
	cfgText, err := BuildWorkerdConfig(WorkerdSpec{Base: work, Socket: sock, CompatDate: l.cfg.CompatDate, Functions: fns, Env: env})
	if err != nil {
		return nil, err
	}
	_ = os.Remove(sock)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(work, "config.capnp")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil { // the secrets are in it: only the supervisor and workerd read it
		return nil, err
	}

	cmd := exec.Command(l.cfg.Workerd, "serve", cfgPath)
	cmd.Dir = work
	cmd.Env = []string{} // nothing: the variables the functions may read are in the configuration
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if l.uids != nil {
		uid, err := l.uids.uid(customer)
		if err != nil {
			return nil, err
		}
		_ = os.Chown(work, uid, uid)
		_ = os.Chown(cfgPath, uid, uid)
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}
	}
	var cg *cgroup
	if l.cfg.CgroupRoot != "" {
		cg, err = newCgroup(l.cfg.CgroupRoot, customer, l.cfg.MemoryMiB, l.cfg.CPUQuotaPct, l.cfg.PidsLimit)
		if err != nil {
			return nil, fmt.Errorf("the cgroup of %s: %w", customer, err)
		}
		if f, err := os.Open(cg.dir); err == nil && isCgroup2(cg.dir) {
			defer f.Close()
			cmd.SysProcAttr.UseCgroupFD = true // clone straight into the cgroup: no moment outside of the limits
			cmd.SysProcAttr.CgroupFD = int(f.Fd())
		}
	}
	if err := cmd.Start(); err != nil {
		if cg != nil {
			cg.remove()
		}
		return nil, err
	}
	p := &WProc{Customer: customer, cmd: cmd, sock: sock, ready: make(chan struct{}), exited: make(chan struct{}), done: make(chan struct{}), cg: cg, bad: bad}
	p.transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
	l.mu.Lock()
	l.starts++
	l.mu.Unlock()
	go func() {
		err := cmd.Wait()
		close(p.exited)
		reason, _ := p.reason.Load().(string)
		l.log.Info("a customer's process exited", "customer", customer, "error", err, "killed_for", reason)
		p.transport.CloseIdleConnections()
		_ = os.Remove(sock)
		_ = os.Remove(cfgPath)
		if p.cg != nil {
			p.cg.remove()
		}
		l.pool.Exited(customer, p)
		close(p.done)
	}()
	go func() {
		p.readyErr = l.waitForSocket(p)
		close(p.ready)
	}()
	go l.watch(p)
	return p, nil
}

func (l *wlauncher) waitForSocket(p *WProc) error {
	deadline := time.Now().Add(l.cfg.StartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			return errors.New("workerd exited while starting") // a configuration it does not accept, or a bundle that does not load
		default:
		}
		if c, err := net.DialTimeout("unix", p.sock, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("workerd did not start")
}

// watch looks at the limits of the process until it exits (limits.go): nearly all of the CPU quota for too long, or too much resident memory, and it is killed with SIGKILL (a looping
// workerd does not see SIGTERM).
func (l *wlauncher) watch(p *WProc) {
	var s sampler
	if l.sampler != nil {
		s = l.sampler(p)
	} else if p.cg != nil && isCgroup2(p.cg.dir) {
		s = cgroupSampler{dir: p.cg.dir}
	} else {
		s = procSampler{pid: p.cmd.Process.Pid}
	}
	lim := limits{MemoryMiB: l.cfg.MemoryMiB, CPUQuotaPct: l.cfg.CPUQuotaPct, Look: l.cfg.Look, RunawayLooks: l.cfg.RunawayLooks}
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

// stop stops the process gently first (SIGTERM), then for certain (SIGKILL).
func (l *wlauncher) stop(p *WProc) {
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

// kill stops the process at once and returns when the pool no longer counts it (the next request starts a fresh process). It must not be called from the pool's own hooks.
func (l *wlauncher) kill(p *WProc) {
	select {
	case <-p.exited:
	default:
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	<-p.done
}

// rss is the resident memory of the process in MiB.
func (l *wlauncher) rss(p *WProc) (float64, bool) {
	if p.cmd.Process == nil {
		return 0, false
	}
	return procSampler{pid: p.cmd.Process.Pid}.RSSMiB()
}

// route splits the path of a request into the customer, the function and the rest. Without CUSTOMERS_DIR the customer is "default" and the path is /<function>/<rest>.
func (s *Server) route(path string) (customer, name, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	rest = "/"
	if s.cfg.CustomersDir == "" {
		customer, name = "default", parts[0]
		if len(parts) >= 2 {
			rest = "/" + strings.Join(parts[1:], "/")
		}
	} else {
		if len(parts) < 2 {
			return "", "", "", false
		}
		customer, name = parts[0], parts[1]
		if len(parts) == 3 {
			rest = "/" + parts[2]
		}
	}
	return customer, name, rest, nameRE.MatchString(customer) && nameRE.MatchString(name)
}

// serveWorkerd is ServeHTTP of the workerd runtime: the answers are in the README ("The supervisor"), the process being the customer's.
func (s *Server) serveWorkerd(w http.ResponseWriter, r *http.Request, t0 time.Time) {
	customer, name, rest, ok := s.route(r.URL.Path)
	if !ok {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	dir := s.wl.customerDir(customer)
	if st, err := os.Stat(filepath.Join(dir, name, "index.mjs")); err != nil || !st.Mode().IsRegular() {
		http.Error(w, "no such function", http.StatusNotFound)
		return
	}
	count := s.counter(customer + "/" + name)
	if int(count.Add(1)) > s.cfg.MaxConcurrent {
		count.Add(-1)
		w.Header().Set("retry-after", "1")
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		return
	}
	defer count.Add(-1)

	lease, err := s.wpool.Acquire(r.Context(), customer)
	if err != nil {
		switch {
		case errors.Is(err, residents.ErrBusy):
			w.Header().Set("retry-after", "1")
			http.Error(w, "the function runtime is busy, try again", http.StatusServiceUnavailable)
		case r.Context().Err() != nil:
		default:
			s.log.Error("a customer's process could not be started", "customer", customer, "error", err)
			http.Error(w, "function failed to start", http.StatusBadGateway)
		}
		return
	}
	defer lease.Release()
	p := lease.Handle

	select {
	case <-p.ready:
	case <-r.Context().Done():
		return
	}
	if p.readyErr != nil {
		s.log.Error("a customer's process did not start", "customer", customer, "error", p.readyErr)
		s.wl.kill(p)
		http.Error(w, "function failed to start", http.StatusBadGateway)
		return
	}

	if why := p.bad[name]; why != "" {
		http.Error(w, "function failed to start", http.StatusBadGateway) // its bundle was left out of the configuration
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	out := r.Clone(ctx)
	out.URL = &url.URL{Path: "/" + name + rest, RawQuery: r.URL.RawQuery}
	out.RequestURI = ""
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host = "http", "function"
			req.Header["X-Forwarded-For"] = nil
		},
		Transport:     p.transport,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			res.Header.Set("x-fn-ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				p.reason.CompareAndSwap(nil, "timeout")
				s.wl.kill(p) // an overrun: the customer's process goes, and with it every request in it; the next request starts a fresh one
				http.Error(w, "function timed out", http.StatusGatewayTimeout)
			case r.Context().Err() != nil:
			default:
				s.log.Warn("a request to a customer's process failed", "customer", customer, "function", name, "error", err)
				s.wl.kill(p)
				http.Error(w, "function failed", http.StatusBadGateway)
			}
		},
	}
	rp.ServeHTTP(w, out)
}

// uidAlloc gives each customer a user id of its own, from UIDBase on. The supervisor must be root to use them (it cannot switch to another user otherwise).
type uidAlloc struct {
	mu    sync.Mutex
	base  int
	count int
	m     map[string]int
}

func newUIDAlloc(base, count int) *uidAlloc {
	return &uidAlloc{base: base, count: count, m: map[string]int{}}
}

func (a *uidAlloc) uid(customer string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.m[customer]; ok {
		return id, nil
	}
	if len(a.m) >= a.count {
		return 0, fmt.Errorf("no user id left for %q (WD_UID_COUNT=%d)", customer, a.count)
	}
	id := a.base + len(a.m)
	a.m[customer] = id
	return id, nil
}
