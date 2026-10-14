package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/koedodb/functions/internal/residents"
)

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Server is the supervisor.
type Server struct {
	cfg  Config
	pool *residents.Pool[*Proc]
	l    *launcher
	log  *slog.Logger

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
	s := &Server{cfg: cfg, log: logger, inflight: map[string]*atomic.Int32{}}
	s.l = &launcher{cfg: cfg, logf: func(f string, a ...any) { logger.Info(strings.TrimSpace(f), "args", a) }}
	opts := residents.Options{BudgetMiB: cfg.BudgetMiB, EstimateMiB: cfg.EstimateMiB, MinResidency: cfg.MinResidency, Wait: cfg.Wait, MaxWaiters: cfg.MaxWaiters}
	s.pool = residents.New(residents.Hooks[*Proc]{Start: s.l.start, Stop: s.l.stop, RSS: s.l.rss}, opts)
	s.l.pool = s.pool
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
		st := s.pool.Stats()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"running": st.Resident, "starts": st.Starts, "evictions": st.Evictions, "busy": st.Busy, "waits": st.Waits, "used_mib": st.UsedMiB, "budget_mib": st.BudgetMiB})
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	name := parts[0]
	rest := "/"
	if len(parts) == 2 {
		rest = "/" + parts[1]
	}
	if !nameRE.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	if st, err := os.Stat(s.l.bundlePath(name)); err != nil || !st.Mode().IsRegular() {
		http.Error(w, "no such function", http.StatusNotFound)
		return
	}

	// A function serves MAX_CONCURRENT requests at once. The memory it uses grows with the requests in flight, not with the rate. Over the limit: 429, and the function is not touched.
	count := s.counter(name)
	if int(count.Add(1)) > s.cfg.MaxConcurrent {
		count.Add(-1)
		w.Header().Set("retry-after", "1")
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		return
	}
	defer count.Add(-1)

	// The process: running, or started, within the memory of the Pod (an LRU stops the least recently used one for room; no room within the wait is Busy).
	lease, err := s.pool.Acquire(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, residents.ErrBusy):
			w.Header().Set("retry-after", "1")
			http.Error(w, "the function runtime is busy, try again", http.StatusServiceUnavailable)
		case r.Context().Err() != nil:
			// the client has gone
		default:
			s.log.Error("a function could not be started", "function", name, "error", err)
			http.Error(w, "function failed to start", http.StatusBadGateway)
		}
		return
	}
	defer lease.Release() // the request counts until its answer has been sent to the end
	p := lease.Handle

	select {
	case <-p.ready:
	case <-r.Context().Done():
		return
	}
	if p.readyErr != nil {
		s.log.Error("a function did not start", "function", name, "error", p.readyErr)
		s.l.kill(p)
		http.Error(w, "function failed to start", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	out := r.Clone(ctx)
	out.URL = &url.URL{Path: rest, RawQuery: r.URL.RawQuery}
	out.RequestURI = ""
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host = "http", "function"
			req.Header["X-Forwarded-For"] = nil // nothing is added: the function sees the request as it came
		},
		Transport:     p.transport,
		FlushInterval: -1, // stream the body as it comes
		ModifyResponse: func(res *http.Response) error {
			res.Header.Set("x-fn-ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				go s.l.kill(p) // an overrun: the next request starts a fresh process
				http.Error(w, "function timed out", http.StatusGatewayTimeout)
			case r.Context().Err() != nil:
				// the client has gone
			default:
				go s.l.kill(p) // the function died: the next request starts a fresh process
				http.Error(w, "function failed", http.StatusBadGateway)
			}
		},
	}
	rp.ServeHTTP(w, out)
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
				if stopped := s.pool.ReapIdle(s.cfg.IdleTimeout, nil); len(stopped) > 0 {
					s.log.Info("idle functions stopped", "functions", stopped)
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
	s.pool.StopAll()
	return nil
}

// Handler is the supervisor as an http.Handler (the tests serve it with httptest).
func (s *Server) Handler() http.Handler { return s }

// Stop stops every function process.
func (s *Server) Stop() { s.pool.StopAll() }
