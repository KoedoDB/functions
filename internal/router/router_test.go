package router

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The router against a fake spawner and a fake workerd on Unix sockets: no root, no workerd.

func TestTheRouteSplitsTheCustomerTheFunctionAndTheRestAndAnEscapedNameIsNoName(t *testing.T) {
	for path, want := range map[string][4]any{
		"/acme/hello":       {"acme", "hello", "/", true},
		"/acme/hello/a/b":   {"acme", "hello", "/a/b", true},
		"/acme/hello/a%2Fb": {"acme", "hello", "/a%2Fb", true},
		"/acme":             {"", "", "", false},
		"/":                 {"", "", "", false},
		"/ACME/hello":       {"ACME", "hello", "/", false},
		"/%61cme/hello":     {"%61cme", "hello", "/", false},
		"/acme/..":          {"acme", "..", "/", false},
		"/acme/he.llo":      {"acme", "he.llo", "/", false},
	} {
		c, n, r, ok := route(path)
		if c != want[0] || n != want[1] || r != want[2] || ok != want[3] {
			t.Errorf("%s: %q %q %q %v, want %v", path, c, n, r, ok, want)
		}
	}
}

func TestTheSettingsAreReadAndATypoIsAnError(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	c, err := ConfigFromEnv(env(nil))
	if err != nil || c.Listen != ":9000" || c.Control != "/sockets" || c.MaxConcurrent != 100 || c.CustomerConcurrent != 200 || c.MaxInflight != 1500 || c.RequestTimeout != 70*time.Second {
		t.Errorf("defaults: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"REQUEST_MS": "x"}, {"MAX_CONCURRENT": "0"}, {"MAX_INFLIGHT": "-1"}} {
		if _, err := ConfigFromEnv(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

type fakeSpawner struct {
	t         *testing.T
	dir       string
	mu        sync.Mutex
	acquired  []string
	released  []string       // "<lease>:<outcome>"
	status    map[string]int // customer/function -> status of acquire (default 200)
	socket    string
	leasesOut atomic.Int32
}

func newFakes(t *testing.T) (*fakeSpawner, *Server, *http.ServeMux) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeSpawner{t: t, dir: dir, status: map[string]int{}, socket: filepath.Join(dir, "w.sock")}
	sp := http.NewServeMux()
	sp.HandleFunc("POST /v1/acquire", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("customer") + "/" + r.URL.Query().Get("function")
		f.mu.Lock()
		f.acquired = append(f.acquired, key)
		st := f.status[key]
		f.mu.Unlock()
		if st != 0 && st != 200 {
			w.WriteHeader(st)
			return
		}
		f.leasesOut.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": "L" + key, "socket": f.socket, "functions": []string{"hello"}})
	})
	sp.HandleFunc("POST /v1/release", func(w http.ResponseWriter, r *http.Request) {
		f.leasesOut.Add(-1)
		f.mu.Lock()
		f.released = append(f.released, r.URL.Query().Get("lease")+":"+r.URL.Query().Get("outcome"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	sp.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	serveUnix(t, filepath.Join(dir, "spawner.sock"), sp)
	wd := http.NewServeMux()
	serveUnix(t, f.socket, wd)
	s := New(Config{Control: dir, RequestTimeout: 300 * time.Millisecond, AcquireTimeout: 2 * time.Second, MaxConcurrent: 2, CustomerConcurrent: 3, MaxInflight: 4}, nil)
	return f, s, wd
}

func serveUnix(t *testing.T, path string, h http.Handler) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func get(t *testing.T, s *Server, path string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b), rec.Header()
}

func TestTheRequestGoesToTheCustomersSocketAsFunctionAndRestAndTheLeaseIsReleased(t *testing.T) {
	f, s, wd := newFakes(t)
	wd.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.EscapedPath() + "?" + r.URL.RawQuery + " xff=" + r.Header.Get("X-Forwarded-For") + " kd=" + r.Header.Get("X-Koedodb-Customer") + " ok=" + r.Header.Get("X-Other")))
	})
	code, body, hdr := get(t, s, "/acme/hello/a%2Fb/c?x=1&y=%20", map[string]string{"X-Koedodb-Customer": "globex", "x-koedodb-Plan": "pro", "X-Other": "keep", "X-Forwarded-For": "1.2.3.4"})
	if code != 200 || body != "/hello/a%2Fb/c?x=1&y=%20 xff= kd= ok=keep" {
		t.Errorf("%d %q", code, body)
	}
	if hdr.Get("x-fn-ms") == "" {
		t.Error("no x-fn-ms header")
	}
	if got := f.released; len(got) != 1 || got[0] != "Lacme/hello:ok" {
		t.Errorf("released %v", got)
	}
}

func TestWhatTheSpawnerRefusesIsWhatTheCallerGetsAndNothingIsLeased(t *testing.T) {
	f, s, _ := newFakes(t)
	f.status["acme/nope"] = 404
	f.status["acme/busy"] = 503
	f.status["acme/broken"] = 500
	for path, want := range map[string]int{"/acme/nope": 404, "/acme/busy": 503, "/acme/broken": 502, "/Acme/x": 400, "/acme": 400, "/_unknown": 400} {
		if code, _, hdr := get(t, s, path, nil); code != want || (want == 503 && hdr.Get("retry-after") == "") {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	if f.leasesOut.Load() != 0 || len(f.released) != 0 {
		t.Errorf("leases out %d, released %v", f.leasesOut.Load(), f.released)
	}
}

func TestAnOverrunIsA504AndTheSpawnerIsToldToKillTheProcessAndABrokenProcessIsA502(t *testing.T) {
	f, s, wd := newFakes(t)
	wd.HandleFunc("/hello/slow", func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) })
	wd.HandleFunc("/hello/dead", func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	})
	if code, _, _ := get(t, s, "/acme/hello/slow", nil); code != 504 {
		t.Errorf("slow: %d", code)
	}
	if code, _, _ := get(t, s, "/acme/hello/dead", nil); code != 502 {
		t.Errorf("dead: %d", code)
	}
	if got := strings.Join(f.released, ","); got != "Lacme/hello:timeout,Lacme/hello:broken" {
		t.Errorf("released %s", got)
	}
}

func TestTooManyRequestsToOneFunctionOrOneCustomerOrTheWholeRouterAreRefused(t *testing.T) {
	f, s, wd := newFakes(t)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	wd.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	})
	s.cfg.RequestTimeout = 10 * time.Second
	var wg sync.WaitGroup
	hold := func(path string) {
		wg.Add(1)
		go func() { defer wg.Done(); get(t, s, path, nil) }()
		<-started
	}
	hold("/acme/hello") // 1 of 2 for the function
	hold("/acme/hello") // 2 of 2
	if code, _, hdr := get(t, s, "/acme/hello", nil); code != 429 || hdr.Get("retry-after") == "" {
		t.Errorf("a third request to one function: %d", code)
	}
	hold("/acme/other") // 3 of 3 for the customer
	if code, _, _ := get(t, s, "/acme/third", nil); code != 429 {
		t.Errorf("a fourth request for one customer: %d", code)
	}
	hold("/globex/hello") // 4 of 4 in all
	if code, _, hdr := get(t, s, "/initech/hello", nil); code != 503 || hdr.Get("retry-after") == "" {
		t.Errorf("a fifth request in all: %d", code)
	}
	close(release)
	wg.Wait()
	if f.leasesOut.Load() != 0 {
		t.Errorf("leases out: %d", f.leasesOut.Load())
	}
	if code, _, _ := get(t, s, "/acme/hello", nil); code != 200 {
		t.Errorf("after they are done: %d", code)
	}
}

func TestHealthIsOkWhileTheSpawnerAnswersAndNotWhenItDoesNot(t *testing.T) {
	_, s, _ := newFakes(t)
	if code, body, _ := get(t, s, "/_healthz", nil); code != 200 || body != "ok" {
		t.Errorf("%d %q", code, body)
	}
	dead := New(Config{Control: t.TempDir(), RequestTimeout: time.Second, AcquireTimeout: time.Second, MaxConcurrent: 1, CustomerConcurrent: 1, MaxInflight: 1}, nil)
	if code, _, _ := get(t, dead, "/_healthz", nil); code != 503 {
		t.Errorf("no spawner: %d", code)
	}
	if code, _, _ := get(t, dead, "/acme/hello", nil); code != 502 {
		t.Errorf("a request with no spawner: %d", code)
	}
}

func TestTheConnectionPoolOfAProcessIsForgottenWhenNobodyUsesItAnyMore(t *testing.T) {
	_, s, wd := newFakes(t)
	wd.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	get(t, s, "/acme/hello", nil)
	s.mu.Lock()
	n := len(s.transports)
	for _, tr := range s.transports {
		tr.last.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	}
	s.mu.Unlock()
	s.sweep()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n != 1 || len(s.transports) != 0 || len(s.inflight) != 0 {
		t.Errorf("transports before %d after %d, counters %d", n, len(s.transports), len(s.inflight))
	}
}
