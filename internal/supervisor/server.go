package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/koedodb/functions/internal/residents"
)

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Server is the supervisor.
type Server struct {
	cfg Config
	log *slog.Logger

	wpool *residents.Pool[*WProc] // a workerd process for each customer
	wl    *wlauncher

	mu       sync.Mutex
	inflight map[string]*atomic.Int32 // requests being served by one function, for MAX_CONCURRENT
}

// New makes a supervisor. The directories of the settings are made if they are not there (Control is a tmpfs of its own in the Pod).
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := os.MkdirAll(cfg.Control, 0o700); err != nil {
		return nil, err
	}
	// A process that runs out of memory aborts, and a core file of its whole address space would be written into the Pod's memory. The limit is inherited by the
	// workerd processes, and with the hard limit at 0 they cannot raise it.
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: logger, inflight: map[string]*atomic.Int32{}}
	opts := residents.Options{BudgetMiB: cfg.BudgetMiB, EstimateMiB: cfg.EstimateMiB, MinResidency: cfg.MinResidency, Wait: cfg.Wait, MaxWaiters: cfg.MaxWaiters}
	if st, err := os.Stat(cfg.Workerd); err != nil || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("WORKERD_BIN=%q is not an executable file", cfg.Workerd)
	}
	s.wl = &wlauncher{cfg: cfg, log: logger}
	if cfg.UIDBase > 0 {
		if os.Geteuid() != 0 {
			return nil, errors.New("WD_UID_BASE needs the supervisor to run as root: a process cannot switch to another user otherwise")
		}
		s.wl.uids = newUIDAlloc(cfg.UIDBase, cfg.UIDCount)
	}
	s.wpool = residents.New(residents.Hooks[*WProc]{Start: s.wl.start, Stop: s.wl.stop, RSS: s.wl.rss}, opts)
	s.wl.pool = s.wpool
	return s, nil
}

func (s *Server) counter(name string) *atomic.Int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.inflight[name]
	if c == nil {
		c = &atomic.Int32{}
		s.inflight[name] = c
	}
	return c
}

// ServeHTTP: GET /_stats, or /<name>/<anything>?<query> for the function <name>.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if r.URL.Path == "/_stats" {
		st := s.stats()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"running": st.Resident, "starts": st.Starts, "evictions": st.Evictions, "busy": st.Busy, "waits": st.Waits, "used_mib": st.UsedMiB, "budget_mib": st.BudgetMiB})
		return
	}
	s.serveWorkerd(w, r, t0)
}

// Run serves until ctx ends, stops what is idle every second, and at the end stops every function process.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if stopped := s.wpool.ReapIdle(s.cfg.IdleTimeout, nil); len(stopped) > 0 {
					s.log.Info("idle customers stopped", "customers", stopped)
				}
			}
		}
	}()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	s.Stop()
	return nil
}

// Handler is the supervisor as an http.Handler (the tests serve it with httptest).
func (s *Server) Handler() http.Handler { return s }

// Stop stops every customer's process.
func (s *Server) Stop() { s.wpool.StopAll() }

// stats is the statistics of the pool.
func (s *Server) stats() residents.Stats { return s.wpool.Stats() }
