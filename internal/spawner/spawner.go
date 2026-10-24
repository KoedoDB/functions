package spawner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
	"github.com/koedodb/functions/internal/wdconfig"
)

// Server is the spawner.
type Server struct {
	cfg Config
	log *slog.Logger

	pool *residents.Pool[*WProc] // a workerd process for each generation of each customer
	wl   *launcher
	uids *uidAlloc

	mu         sync.Mutex
	customers  map[string]*customer
	locks      map[string]*sync.Mutex // one put or delete of a customer at a time
	stateBytes int64                  // what the customers' definitions hold in memory
	nextGen    uint64
	leases     map[string]*lease
	probeSem   chan struct{}
	putSem     chan struct{} // puts being read at once: a body is held in memory
}

type lease struct {
	l     *residents.Lease[*WProc]
	p     *WProc
	timer *time.Timer
}

// New makes a spawner. It must run as root with the capabilities it needs and no others that matter (caps.go), and it makes the directories of the settings: Control is root:RouterGID 0750,
// State and State/run are 0711 (a customer's process can reach its own run directory below it and cannot list it).
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("the spawner must run as root: it starts each customer's process as a user of its own, and a process cannot switch to another user otherwise")
	}
	if err := checkCaps(logger); err != nil {
		return nil, err
	}
	if st, err := os.Stat(cfg.Workerd); err != nil || st.Mode()&0o111 == 0 || st.Mode()&0o001 == 0 {
		return nil, fmt.Errorf("WORKERD_BIN=%q is not an executable file that every user may run", cfg.Workerd)
	}
	// Every file and socket the spawner makes is private until it says otherwise.
	syscall.Umask(0o077)
	// A process that runs out of memory aborts, and a core file of its whole address space would be written into the VM's memory. The limit is inherited by the workerd processes, and with
	// the hard limit at 0 they cannot raise it.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return nil, err
	}
	// The directories start empty: what is left from an earlier run of the spawner (the volumes outlive a container) belongs to processes that are gone.
	// (The directories themselves are mount points in a Pod, so what is inside is removed, not them.)
	for _, d := range []string{cfg.Control, filepath.Join(cfg.State, "run")} {
		if entries, err := os.ReadDir(d); err == nil {
			for _, e := range entries {
				if err := os.RemoveAll(filepath.Join(d, e.Name())); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := os.MkdirAll(cfg.Control, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chown(cfg.Control, 0, cfg.RouterGID); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Control, 0o750); err != nil {
		return nil, err
	}
	for _, d := range []string{cfg.State, filepath.Join(cfg.State, "run")} {
		if err := os.MkdirAll(d, 0o711); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o711); err != nil {
			return nil, err
		}
	}
	return newServer(cfg, logger), nil
}

// newServer is the spawner's state and pool, with nothing privileged done (the tests of the rules use it as an ordinary user).
func newServer(cfg Config, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: logger, customers: map[string]*customer{}, locks: map[string]*sync.Mutex{}, leases: map[string]*lease{}, probeSem: make(chan struct{}, cfg.MaxProbes), putSem: make(chan struct{}, cfg.MaxProbes),
		uids: newUIDAlloc(cfg.UIDBase, cfg.UIDCount)}
	s.wl = newLauncher(cfg, logger, s.get)
	opts := residents.Options{BudgetMiB: cfg.BudgetMiB, EstimateMiB: cfg.EstimateMiB, MinResidency: cfg.MinResidency, Wait: cfg.Wait, MaxWaiters: cfg.MaxWaiters}
	s.pool = residents.New(residents.Hooks[*WProc]{Start: s.wl.start, Stop: s.wl.stop, RSS: s.wl.rss}, opts)
	s.wl.pool = s.pool
	return s
}

// get is the current definition of a customer, or nil.
func (s *Server) get(id string) *customer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.customers[id]
}

func (s *Server) lockOf(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

// current says whether a process key is of the customer's current generation.
func (s *Server) current(key string) bool {
	id, gen, ok := splitKey(key)
	if !ok {
		return false
	}
	c := s.get(id)
	return c != nil && c.gen == gen
}

// Run serves the control API and the internal socket until ctx ends, stops what is idle every second, and at the end stops every process.
func (s *Server) Run(ctx context.Context) error {
	ctl, err := s.controlServer()
	if err != nil {
		return err
	}
	ctlLn, err := net.Listen("tcp", s.cfg.ControlListen)
	if err != nil {
		return err
	}
	inLn, err := s.internalListener()
	if err != nil {
		_ = ctlLn.Close()
		return err
	}
	in := &http.Server{Handler: s.internalHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if stopped := s.pool.ReapIdle(s.cfg.IdleTimeout, nil); len(stopped) > 0 {
					s.log.Info("idle processes stopped", "keys", stopped)
				}
				// A process of an old generation drains: when nothing is served by it any more it goes (keepAlive says which are kept).
				s.pool.ReapIdle(0, s.current)
			}
		}
	}()
	errc := make(chan error, 2)
	go func() { errc <- ctl.ServeTLS(ctlLn, "", "") }()
	go func() { errc <- in.Serve(inLn) }()
	select {
	case err = <-errc:
	case <-ctx.Done():
		err = nil
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctl.Shutdown(sctx)
	_ = in.Shutdown(sctx)
	s.Stop()
	return err
}

// Stop stops every customer's process.
func (s *Server) Stop() {
	s.pool.StopAll()
	s.mu.Lock()
	for _, l := range s.leases {
		l.timer.Stop()
	}
	s.leases = map[string]*lease{}
	s.mu.Unlock()
}

// ---- put and delete

// Definition is what a put carries.
type Definition struct {
	Functions map[string][]byte
	Secrets   map[string]string
	Limits    Limits                       // zero fields: the platform's maximum
	FnLimits  map[string]wdconfig.FnLimits // of each function: zero fields are the default (never "no limit"); Validate fills them in and checks the maximum
}

// Error is a refusal with the HTTP status it is answered with.
type Error struct {
	Status int
	Msg    string
	Detail map[string]string // for each function that does not load, why
}

func (e *Error) Error() string { return e.Msg }

func refuse(status int, format string, a ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Validate checks a definition against the settings and fills in the limits. Nothing about the functions' contents is judged here (that is the probe's job).
func (s *Server) Validate(id string, d *Definition) *Error {
	if !wdconfig.NameRE.MatchString(id) {
		return refuse(http.StatusBadRequest, "%q is not a customer name", id)
	}
	if len(d.Functions) == 0 || len(d.Functions) > s.cfg.MaxFunctions {
		return refuse(http.StatusBadRequest, "a customer has 1 to %d functions", s.cfg.MaxFunctions)
	}
	for name, body := range d.Functions {
		if !wdconfig.NameRE.MatchString(name) {
			return refuse(http.StatusBadRequest, "%q is not a function name", name)
		}
		if len(body) == 0 || len(body) > s.cfg.MaxBundleBytes {
			return refuse(http.StatusRequestEntityTooLarge, "the bundle of %q must be 1 to %d bytes", name, s.cfg.MaxBundleBytes)
		}
	}
	if len(d.Secrets) > s.cfg.MaxSecrets {
		return refuse(http.StatusBadRequest, "at most %d secrets", s.cfg.MaxSecrets)
	}
	for k, v := range d.Secrets {
		if !wdconfig.EnvNameRE.MatchString(k) {
			return refuse(http.StatusBadRequest, "%q is not a secret name", k)
		}
		if len(v) > 8192 || strings.IndexByte(v, 0) >= 0 {
			return refuse(http.StatusBadRequest, "the secret %q is longer than 8192 bytes or has a NUL byte", k)
		}
	}
	lim := d.Limits
	if lim.MemoryMiB == 0 {
		lim.MemoryMiB = s.cfg.MemoryMiB
	}
	if lim.CPUQuotaPct == 0 {
		lim.CPUQuotaPct = s.cfg.CPUQuotaPct
	}
	if lim.Pids == 0 {
		lim.Pids = s.cfg.PidsLimit
	}
	if lim.MemoryMiB < 32 || lim.MemoryMiB > s.cfg.MemoryMiB || lim.CPUQuotaPct < 1 || lim.CPUQuotaPct > s.cfg.CPUQuotaPct || lim.Pids < 8 || lim.Pids > s.cfg.PidsLimit {
		return refuse(http.StatusBadRequest, "limits: memory_mib 32..%v, cpu_quota_pct 1..%v, pids 8..%d (the platform's maximum)", s.cfg.MemoryMiB, s.cfg.CPUQuotaPct, s.cfg.PidsLimit)
	}
	d.Limits = lim
	fl := make(map[string]wdconfig.FnLimits, len(d.Functions))
	for name := range d.Functions {
		l, e := s.fnLimits(name, d.FnLimits[name])
		if e != nil {
			return e
		}
		fl[name] = l
	}
	d.FnLimits = fl
	return nil
}

// fnLimits fills the zero fields of a function's limits with the default and refuses a field above the platform's maximum.
func (s *Server) fnLimits(name string, l wdconfig.FnLimits) (wdconfig.FnLimits, *Error) {
	d, m := s.cfg.FnDefault, s.cfg.FnMax
	fill := func(v *uint32, def uint32) {
		if *v == 0 {
			*v = def
		}
	}
	fill(&l.HeapMB, d.HeapMB)
	fill(&l.CPUMs, d.CPUMs)
	fill(&l.WallMs, d.WallMs)
	fill(&l.ExternalMB, d.ExternalMB)
	fill(&l.Subrequests, d.Subrequests)
	fill(&l.ConcurrentSubrequests, d.ConcurrentSubrequests)
	if l.HeapMB < 16 || l.HeapMB > m.HeapMB || l.CPUMs > m.CPUMs || l.WallMs > m.WallMs || l.ExternalMB > m.ExternalMB || l.Subrequests > m.Subrequests || l.ConcurrentSubrequests > m.ConcurrentSubrequests {
		return l, refuse(http.StatusBadRequest, "limits of %q: heap_mb 16..%d, cpu_ms ..%d, wall_ms ..%d, external_mb ..%d, subrequests ..%d, concurrent_subrequests ..%d (the platform's maximum)",
			name, m.HeapMB, m.CPUMs, m.WallMs, m.ExternalMB, m.Subrequests, m.ConcurrentSubrequests)
	}
	return l, nil
}

// Put makes or replaces a customer (the definition has been validated). The bundles are checked by a throwaway workerd running as the customer's user before anything changes: if one does
// not load, nothing changes and the error says which. A put that has the same bundles as the customer's current ones is not checked again.
func (s *Server) Put(ctx context.Context, id string, d Definition) (*Info, *Error) {
	lock := s.lockOf(id)
	lock.Lock()
	defer lock.Unlock()

	size := definitionBytes(d.Functions, d.Secrets)
	s.mu.Lock()
	old := s.customers[id]
	var oldBytes int64
	if old != nil {
		oldBytes = old.bytes
	}
	if s.stateBytes-oldBytes+size > s.cfg.MaxStateBytes {
		s.mu.Unlock()
		return nil, refuse(http.StatusInsufficientStorage, "the definitions of all customers would be over %d bytes", s.cfg.MaxStateBytes)
	}
	if old == nil && len(s.customers) >= s.cfg.MaxCustomers {
		s.mu.Unlock()
		return nil, refuse(http.StatusInsufficientStorage, "this VM holds %d customers at most", s.cfg.MaxCustomers)
	}
	s.mu.Unlock()

	uid, created, err := s.uids.alloc(id)
	if err != nil {
		return nil, refuse(http.StatusInsufficientStorage, "%v", err)
	}
	abandon := func() {
		if created {
			s.uids.free(id, false)
		}
	}

	var changed []string // the functions whose bundle is new: the others were checked when they came
	for name, body := range d.Functions {
		if old == nil || !bytes.Equal(old.fns[name], body) {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	next := &customer{id: id, uid: uid, fns: d.Functions, secrets: d.Secrets, limits: d.Limits, fnLimits: d.FnLimits, bytes: size}
	if len(changed) > 0 {
		if bad := s.checkBundles(ctx, next, changed); len(bad) > 0 {
			abandon()
			return nil, &Error{Status: http.StatusUnprocessableEntity, Msg: "a function does not load", Detail: bad}
		}
	}

	s.mu.Lock()
	s.nextGen++
	next.gen = s.nextGen
	if old != nil {
		s.stateBytes -= old.bytes
	}
	s.stateBytes += size
	s.customers[id] = next
	s.mu.Unlock()
	s.log.Info("a customer is put", "customer", id, "generation", next.gen, "functions", len(next.fns), "secrets", len(next.secrets), "bytes", size, "created", old == nil)
	return s.info(next), nil
}

// Delete removes a customer: its processes are killed at once, and its user id is given back when nothing of it runs.
func (s *Server) Delete(id string) bool {
	lock := s.lockOf(id)
	lock.Lock()
	defer lock.Unlock()
	s.mu.Lock()
	c := s.customers[id]
	if c == nil {
		s.mu.Unlock()
		return false
	}
	delete(s.customers, id)
	s.stateBytes -= c.bytes
	s.mu.Unlock()

	// Kill everything of the customer's user (the processes of every generation, a probe), and wait until nothing is left: only then the number may go to another customer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		killAllOf(c.uid)
		if countOf(c.uid) == 0 && s.wl.liveCount(id) == 0 {
			break
		}
		if time.Now().After(deadline) {
			s.log.Error("a deleted customer's process would not die: the user id is kept back", "customer", id, "uid", c.uid)
			s.uids.free(id, true)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.uids.free(id, false)
	s.mu.Lock()
	delete(s.locks, id)
	s.mu.Unlock()
	s.log.Info("a customer is deleted", "customer", id)
	return true
}

// Info is what the API tells about a customer (never the secrets' values).
type Info struct {
	ID         string   `json:"id"`
	Generation uint64   `json:"generation"`
	Functions  []string `json:"functions"`
	Secrets    int      `json:"secrets"`
	Running    bool     `json:"running"`
}

func (s *Server) info(c *customer) *Info {
	running := false
	for _, k := range s.pool.Stats().Resident {
		if id, _, ok := splitKey(k); ok && id == c.id {
			running = true
		}
	}
	return &Info{ID: c.id, Generation: c.gen, Functions: c.names(), Secrets: len(c.secrets), Running: running}
}

// List is every customer, sorted.
func (s *Server) List() []*Info {
	s.mu.Lock()
	cs := make([]*customer, 0, len(s.customers))
	for _, c := range s.customers {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].id < cs[j].id })
	out := make([]*Info, 0, len(cs))
	for _, c := range cs {
		out = append(out, s.info(c))
	}
	return out
}

// ---- the check of the bundles

// checkBundles says which of the functions workerd would not accept (a syntax error, a module that does not exist): the reason for each. workerd compiles every module when it starts
// and stops at the first one it cannot, so one bad bundle would take the customer's other functions down with it. There is no other parser in the image, so workerd itself is the check: a
// throwaway process of the customer's user, with the same configuration, no secrets and no request that reaches the customer's code (the bundles are imported when the first request
// to a function comes). One process for all of them; only when it fails, one for each, to find the bad ones.
func (s *Server) checkBundles(ctx context.Context, c *customer, names []string) map[string]string {
	if err := s.probe(ctx, c, names); err == nil {
		return nil
	} else if len(names) == 1 {
		s.log.Warn("a bundle does not load", "customer", c.id, "function", names[0], "output", err.Error())
		return map[string]string{names[0]: "does not load"}
	}
	bad := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			if err := s.probe(ctx, c, []string{n}); err != nil {
				s.log.Warn("a bundle does not load", "customer", c.id, "function", n, "output", err.Error())
				mu.Lock()
				bad[n] = "does not load"
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	if len(bad) == 0 { // each is fine alone and all together are not (a conflict between them): say so
		bad[names[0]] = "the functions do not load together"
	}
	return bad
}

// probe starts a throwaway workerd with the given functions and says whether it came up and stayed up for a moment. The first line of what it said is the error.
func (s *Server) probe(ctx context.Context, c *customer, names []string) error {
	select {
	case s.probeSem <- struct{}{}:
		defer func() { <-s.probeSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.wl.mu.Lock()
	s.wl.seq++
	n := s.wl.seq
	s.wl.mu.Unlock()
	name := "p" + strconv.Itoa(n)
	out := &limitedBuffer{}
	b, err := s.wl.begin(runSpec{c: c, name: name, sock: filepath.Join(s.cfg.State, name+".sock"), probe: true, fns: names, extraOut: out})
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = b.cmd.Wait()
		close(exited)
	}()
	stop := func() {
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		b.cleanup()
		s.wl.addLive(c.id, -1)
	}
	timeout := s.cfg.StartTimeout
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A bundle that makes workerd use a great deal of memory while it is only being compiled is stopped like a bomb (the same limit as the customer's process).
	go func() {
		sampler := procSampler{pid: b.cmd.Process.Pid}
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-exited:
				return
			case <-pctx.Done():
				return
			case <-tick.C:
				if rss, ok := sampler.RSSMiB(); ok && rss > c.limits.MemoryMiB {
					s.log.Warn("a bundle takes too much memory to compile", "customer", c.id, "rss_mib", rss)
					_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
					return
				}
			}
		}
	}()
	ready := make(chan error, 1)
	go func() { ready <- s.wl.waitReadyCtx(pctx, filepath.Join(s.cfg.State, name+".sock"), exited) }()
	select {
	case err := <-ready:
		if err != nil {
			stop()
			return firstLine(out.String(), err)
		}
	case <-exited:
		stop()
		return firstLine(out.String(), errors.New("workerd exited while starting"))
	}
	// workerd is up; an error in a bundle may still stop it a moment later: look once more.
	select {
	case <-exited:
		stop()
		return firstLine(out.String(), errors.New("workerd exited"))
	case <-time.After(300 * time.Millisecond):
	}
	stop()
	return nil
}

func firstLine(out string, fallback error) error {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if first == "" {
		return fallback
	}
	return errors.New(first)
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

// waitReadyCtx is waitReady that also ends with ctx.
func (l *launcher) waitReadyCtx(ctx context.Context, sock string, exited <-chan struct{}) error {
	done := make(chan error, 1)
	go func() { done <- l.waitReady(sock, exited) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("workerd did not come up")
	}
}

// ---- leases (the internal API uses them)

func newLeaseID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
