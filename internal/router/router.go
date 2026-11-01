// Package router is the non-root half of the KoedoDB function supervisor for a VM that holds several customers: it faces the edge (Envoy), routes /<customer>/<function>/... to the
// customer's workerd process, and counts and limits what it carries.
//
// It holds no bundle, no secret, and no authority over customers: it can ask the spawner (a Unix socket only its user can open) to make sure a customer's process is up for a request and
// to say when the request is over, and it can connect to the customers' sockets in the shared directory, which is all it can reach. A caller is not authenticated here: a function is a
// public URL, the path is the address, and what may reach this port is the Pod's network policy (only the edge). Headers that the edge adds for the platform start with X-Koedodb-; a
// request that arrives with such a header has it removed, so a caller cannot forge one.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/koedodb/functions/internal/wdconfig"
)

// Config is the router's settings.
type Config struct {
	Listen             string        // ":9000"
	Control            string        // the directory shared with the spawner: the internal socket is <Control>/spawner.sock
	RequestTimeout     time.Duration // a request that takes longer is cancelled (504) and its process is killed
	AcquireTimeout     time.Duration // how long to wait for the spawner (it waits for room, and for a start)
	MaxConcurrent      int           // requests being served by one function at once; over it: 429
	CustomerConcurrent int           // requests being served for one customer at once (all its functions); over it: 429
	MaxInflight        int           // requests being served at all; over it: 503
}

// ConfigFromEnv reads the settings from the environment. A value that is not a number is an error.
func ConfigFromEnv(lookup func(string) (string, bool)) (Config, error) {
	var errs []string
	str := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	num := func(key string, def float64) float64 {
		v, ok := lookup(key)
		if !ok || v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%q is not a number above 0", key, v))
			return def
		}
		return f
	}
	c := Config{
		Listen:             str("LISTEN", ":9000"),
		Control:            str("CONTROL_DIR", "/sockets"),
		RequestTimeout:     time.Duration(num("REQUEST_MS", 70000)) * time.Millisecond,
		AcquireTimeout:     time.Duration(num("ACQUIRE_MS", 40000)) * time.Millisecond,
		MaxConcurrent:      int(num("MAX_CONCURRENT", 100)),
		CustomerConcurrent: int(num("CUSTOMER_CONCURRENT", 200)),
		MaxInflight:        int(num("MAX_INFLIGHT", 1500)),
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("settings: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// Server is the router.
type Server struct {
	cfg Config
	log *slog.Logger

	spawner *http.Client
	total   atomic.Int64

	mu         sync.Mutex
	inflight   map[string]*atomic.Int32 // by customer and by customer/function
	transports map[string]*transport    // by socket path
}

type transport struct {
	tr   *http.Transport
	last atomic.Int64 // unix nanoseconds of the last use
}

// New makes a router.
func New(cfg Config, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	sock := filepath.Join(cfg.Control, "spawner.sock")
	return &Server{
		cfg: cfg, log: logger, inflight: map[string]*atomic.Int32{}, transports: map[string]*transport{},
		spawner: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     60 * time.Second,
		}},
	}
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

// transportFor is the connection pool for one process's socket. A socket path belongs to one process for good (the spawner numbers them), so a pooled connection never meets another process.
func (s *Server) transportFor(sock string) *http.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.transports[sock]
	if t == nil {
		t = &transport{tr: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
		}}
		s.transports[sock] = t
	}
	t.last.Store(time.Now().UnixNano())
	return t.tr
}

// sweep forgets the connection pools nobody has used for a minute (the process is gone, or idle).
func (s *Server) sweep() {
	cut := time.Now().Add(-time.Minute).UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sock, t := range s.transports {
		if t.last.Load() < cut {
			t.tr.CloseIdleConnections()
			delete(s.transports, sock)
		}
	}
	for name, c := range s.inflight {
		if c.Load() == 0 {
			delete(s.inflight, name)
		}
	}
}

// route splits the escaped path of a request into the customer, the function and the rest (escaped, as it came). Names are matched as they are: an escaped name is no name.
func route(escapedPath string) (customer, name, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(escapedPath, "/"), "/", 3)
	if len(parts) < 2 {
		return "", "", "", false
	}
	customer, name, rest = parts[0], parts[1], "/"
	if len(parts) == 3 {
		rest = "/" + parts[2]
	}
	return customer, name, rest, wdconfig.NameRE.MatchString(customer) && wdconfig.NameRE.MatchString(name)
}

type acquired struct {
	Lease     string   `json:"lease"`
	Socket    string   `json:"socket"`
	Functions []string `json:"functions"`
}

// acquire asks the spawner for the process; the status is what the request is answered with when it is not 200.
func (s *Server) acquire(ctx context.Context, customer, name string) (*acquired, int, string) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.AcquireTimeout)
	defer cancel()
	q := url.Values{"customer": {customer}, "function": {name}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://spawner/v1/acquire?"+q.Encode(), nil)
	res, err := s.spawner.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ""
		}
		s.log.Error("the spawner does not answer", "error", err)
		return nil, http.StatusBadGateway, "function failed to start"
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		var a acquired
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&a); err != nil || a.Lease == "" || a.Socket == "" {
			return nil, http.StatusBadGateway, "function failed to start"
		}
		return &a, 0, ""
	case http.StatusNotFound:
		return nil, http.StatusNotFound, "no such function"
	case http.StatusServiceUnavailable:
		return nil, http.StatusServiceUnavailable, "the function runtime is busy, try again"
	case http.StatusBadRequest:
		return nil, http.StatusBadRequest, "bad name"
	default:
		return nil, http.StatusBadGateway, "function failed to start"
	}
}

// release says that the request is over. outcome: "ok", "timeout" or "broken" (the spawner kills the process of the last two).
func (s *Server) release(lease, outcome string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	q := url.Values{"lease": {lease}, "outcome": {outcome}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://spawner/v1/release?"+q.Encode(), nil)
	res, err := s.spawner.Do(req)
	if err != nil {
		s.log.Warn("the lease could not be released (it expires by itself)", "error", err)
		return
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
}

// ServeHTTP: /_healthz, or /<customer>/<function>/<anything>?<query>.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if r.URL.Path == "/_healthz" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://spawner/v1/ping", nil)
		res, err := s.spawner.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			http.Error(w, "the spawner does not answer", http.StatusServiceUnavailable)
			return
		}
		_ = res.Body.Close()
		_, _ = w.Write([]byte("ok"))
		return
	}
	customer, name, rest, ok := route(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	for k := range r.Header { // what the edge adds for the platform cannot come from a caller
		if strings.HasPrefix(strings.ToLower(k), "x-koedodb-") {
			delete(r.Header, k)
		}
	}

	if int(s.total.Add(1)) > s.cfg.MaxInflight {
		s.total.Add(-1)
		w.Header().Set("retry-after", "1")
		http.Error(w, "the function runtime is busy, try again", http.StatusServiceUnavailable)
		return
	}
	defer s.total.Add(-1)
	byCustomer := s.counter(customer)
	if int(byCustomer.Add(1)) > s.cfg.CustomerConcurrent {
		byCustomer.Add(-1)
		w.Header().Set("retry-after", "1")
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		return
	}
	defer byCustomer.Add(-1)
	byFunction := s.counter(customer + "/" + name)
	if int(byFunction.Add(1)) > s.cfg.MaxConcurrent {
		byFunction.Add(-1)
		w.Header().Set("retry-after", "1")
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		return
	}
	defer byFunction.Add(-1)

	a, status, msg := s.acquire(r.Context(), customer, name)
	if a == nil {
		if status != 0 {
			if status == http.StatusServiceUnavailable {
				w.Header().Set("retry-after", "1")
			}
			http.Error(w, msg, status)
		}
		return
	}
	outcome := "ok"
	defer func() { s.release(a.Lease, outcome) }()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	out := r.Clone(ctx)
	out.URL = &url.URL{Path: "/" + name + decoded, RawPath: "/" + name + rest, RawQuery: r.URL.RawQuery}
	out.RequestURI = ""
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host = "http", "function"
			req.Header["X-Forwarded-For"] = nil // the function sees no caller's address
		},
		Transport:     s.transportFor(a.Socket),
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			res.Header.Set("x-fn-ms", strconv.FormatInt(time.Since(t0).Milliseconds(), 10))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				outcome = "timeout" // an overrun: the customer's process goes, and with it every request in it; the next request starts a fresh one
				http.Error(w, "function timed out", http.StatusGatewayTimeout)
			case r.Context().Err() != nil:
			case errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE):
				// The router itself has run out of file descriptors (two for each request in flight): the customer's process did nothing wrong, and killing it would fail every other request in it.
				s.log.Error("the router has no file descriptor left", "customer", customer, "function", name, "error", err)
				w.Header().Set("retry-after", "1")
				http.Error(w, "busy", http.StatusServiceUnavailable)
			default:
				s.log.Warn("a request to a customer's process failed", "customer", customer, "function", name, "error", err)
				outcome = "broken"
				http.Error(w, "function failed", http.StatusBadGateway)
			}
		},
	}
	rp.ServeHTTP(w, out)
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.sweep()
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
	return srv.Shutdown(sctx)
}

// Handler is the router as an http.Handler (the tests serve it with httptest).
func (s *Server) Handler() http.Handler { return s }
