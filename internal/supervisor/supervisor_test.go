package supervisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests run real Node processes: they need `node` (22.13 or later, for --permission) and are skipped without it.

func needNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
}

const (
	helloJS  = `export default (req) => { const u = new URL(req.url); return new Response('hello ' + u.pathname + u.search, { headers: { 'x-method': req.method } }); };`
	echoJS   = `export default async (req) => new Response(await req.text(), { status: 201 });`
	pidJS    = `export default () => new Response(String(process.pid));`
	envsJS   = `export default () => Response.json(Object.keys(process.env).sort());`
	secret   = `export default () => new Response(process.env.API_KEY ?? 'none');`
	slowJS   = `export default async (req) => { const ms = Number(new URL(req.url).searchParams.get('ms') ?? 3000); await new Promise((r) => setTimeout(r, ms)); return new Response('late ' + process.pid); };`
	dieJS    = `export default (req) => { if (new URL(req.url).pathname === '/die') process.exit(1); return new Response(String(process.pid)); };`
	throwJS  = `export default () => { throw new Error('boom'); };`
	noExp    = `export const x = 1;`
	syntax   = `export default (`
	fsJS     = `import fs from 'node:fs'; export default () => { try { fs.readFileSync('/etc/passwd'); return new Response('read'); } catch (e) { return new Response(String(e.code)); } };`
	spawnJS  = `import cp from 'node:child_process'; export default () => { try { cp.execSync('id'); return new Response('ran'); } catch (e) { return new Response(String(e.code)); } };`
	heapJS   = `export default () => { const a = []; for (;;) a.push(new Array(1e6).fill(1)); };`
	cookies  = `export default () => { const h = new Headers(); h.append('set-cookie', 'a=1'); h.append('set-cookie', 'b=2'); return new Response('ok', { headers: h }); };`
	streamJS = `export default () => new Response(new ReadableStream({ async start(c) { for (const s of ['one,', 'two,', 'three']) { c.enqueue(new TextEncoder().encode(s)); await new Promise((r) => setTimeout(r, 30)); } c.close(); } }));`
	fetchObj = `export default { fetch: (req) => new Response('from fetch ' + new URL(req.url).pathname) };`
)

type rig struct {
	t    *testing.T
	s    *Server
	ts   *httptest.Server
	dir  string
	envs map[string]string
}

// newRig serves the given functions (name -> JavaScript). mutate changes the settings.
func newRig(t *testing.T, fns map[string]string, mutate func(*Config)) *rig {
	t.Helper()
	needNode(t)
	dir := t.TempDir()
	for name, src := range fns {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "index.mjs"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	control, err := os.MkdirTemp("", "fs") // short: a Unix socket's path is 107 bytes at most
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(control) })
	runner, _ := filepath.Abs("../../runner/runner.mjs")
	r := &rig{t: t, dir: dir, envs: map[string]string{}}
	cfg, err := ConfigFromEnv(func(k string) (string, bool) {
		return map[string]string{"FUNCTIONS_DIR": dir, "CONTROL_DIR": control, "RUNNER": runner, "FN_BUDGET_MIB": "2000", "FN_MIN_RESIDENCY_MS": "0", "REQUEST_MS": "5000", "START_MS": "10000"}[k], k == "FUNCTIONS_DIR" || k == "CONTROL_DIR" || k == "RUNNER" || k == "FN_BUDGET_MIB" || k == "FN_MIN_RESIDENCY_MS" || k == "REQUEST_MS" || k == "START_MS"
	}, func() (float64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Getenv = func(k string) string { return r.envs[k] }
	if mutate != nil {
		mutate(&cfg)
	}
	r.s, err = New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ts = httptest.NewServer(r.s)
	t.Cleanup(func() { r.ts.Close(); r.s.Stop() })
	return r
}

type answer struct {
	status int
	body   string
	header http.Header
}

func (r *rig) do(method, path, body string) answer {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return answer{res.StatusCode, string(b), res.Header}
}

func (r *rig) get(path string) answer { r.t.Helper(); return r.do("GET", path, "") }

func (r *rig) stats() (running []string, evictions, busy int) {
	r.t.Helper()
	var st struct {
		Running   []string `json:"running"`
		Evictions int      `json:"evictions"`
		Busy      int      `json:"busy"`
	}
	if err := json.Unmarshal([]byte(r.get("/_stats").body), &st); err != nil {
		r.t.Fatal(err)
	}
	slices.Sort(st.Running)
	return st.Running, st.Evictions, st.Busy
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("not true in time: %s", what)
}

func TestARequestIsRoutedToItsFunctionWithThePathAndTheQueryAfterTheName(t *testing.T) {
	r := newRig(t, map[string]string{"hello": helloJS}, nil)
	a := r.get("/hello/a/b?x=1&y=2")
	if a.status != 200 || a.body != "hello /a/b?x=1&y=2" || a.header.Get("x-method") != "GET" {
		t.Fatalf("%d %q %v", a.status, a.body, a.header)
	}
	if a.header.Get("x-fn-ms") == "" {
		t.Fatal("no x-fn-ms")
	}
	if root := r.get("/hello"); root.body != "hello /" {
		t.Fatalf("%q", root.body)
	}
}

func TestBadNamesAreRefusedAndAnUnknownFunctionIsNotFound(t *testing.T) {
	r := newRig(t, map[string]string{"hello": helloJS}, nil)
	for _, bad := range []string{"/Hello/x", "/he_llo/x", "/" + strings.Repeat("a", 41) + "/x", "/../etc/passwd", "/"} {
		if a := r.get(bad); a.status != 400 && a.status != 404 {
			t.Errorf("%s: %d", bad, a.status)
		}
	}
	if a := r.get("/nothere/x"); a.status != 404 {
		t.Fatalf("%d", a.status)
	}
}

func TestABodyAndAMethodReachTheFunctionAndItsStatusAndHeadersComeBack(t *testing.T) {
	r := newRig(t, map[string]string{"echo": echoJS, "cookies": cookies}, nil)
	a := r.do("POST", "/echo/", "the body of the request")
	if a.status != 201 || a.body != "the body of the request" {
		t.Fatalf("%d %q", a.status, a.body)
	}
	c := r.get("/cookies/")
	if got := c.header.Values("Set-Cookie"); !slices.Equal(got, []string{"a=1", "b=2"}) {
		t.Fatalf("set-cookie %v", got)
	}
}

func TestAnObjectWithAFetchMethodIsServedToo(t *testing.T) {
	r := newRig(t, map[string]string{"app": fetchObj}, nil)
	if a := r.get("/app/z"); a.body != "from fetch /z" {
		t.Fatalf("%q", a.body)
	}
}

func TestTheBodyOfAnswerIsStreamed(t *testing.T) {
	r := newRig(t, map[string]string{"stream": streamJS}, nil)
	if a := r.get("/stream/"); a.status != 200 || a.body != "one,two,three" {
		t.Fatalf("%d %q", a.status, a.body)
	}
}

func TestAFunctionGetsACleanEnvironmentAndOnlyTheVariablesItIsAllowed(t *testing.T) {
	r := newRig(t, map[string]string{"envs": envsJS, "secret": secret}, func(c *Config) { c.EnvAllow = []string{"API_KEY"} })
	r.envs["API_KEY"] = "k-123"
	r.envs["DATABASE_PASSWORD"] = "must-not-be-seen" // in the supervisor's environment, not allowed
	var keys []string
	if err := json.Unmarshal([]byte(r.get("/envs/").body), &keys); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"API_KEY", "FN_SOCKET", "NODE_ENV", "NO_COLOR"}) {
		t.Fatalf("the function sees %v", keys)
	}
	if a := r.get("/secret/"); a.body != "k-123" {
		t.Fatalf("%q", a.body)
	}
}

func TestAFunctionCannotReadOtherFilesNorStartAProcess(t *testing.T) {
	r := newRig(t, map[string]string{"fs": fsJS, "spawn": spawnJS}, nil)
	if a := r.get("/fs/"); a.body != "ERR_ACCESS_DENIED" {
		t.Fatalf("reading /etc/passwd: %q", a.body)
	}
	if a := r.get("/spawn/"); a.body != "ERR_ACCESS_DENIED" {
		t.Fatalf("starting a process: %q", a.body)
	}
}

func TestAFunctionThatThrowsAnswers500AndStaysUp(t *testing.T) {
	r := newRig(t, map[string]string{"throws": throwJS}, nil)
	if a := r.get("/throws/"); a.status != 500 || a.body != "function error" {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if running, _, _ := r.stats(); !slices.Equal(running, []string{"throws"}) {
		t.Fatalf("running %v", running)
	}
}

func TestAFunctionThatDiesAnswers502AndTheNextRequestStartsAFreshProcess(t *testing.T) {
	r := newRig(t, map[string]string{"die": dieJS}, nil)
	first := r.get("/die/ok")
	if first.status != 200 {
		t.Fatalf("%d", first.status)
	}
	if a := r.get("/die/die"); a.status != 502 {
		t.Fatalf("the process exited during the request: %d %q", a.status, a.body)
	}
	eventually(t, "the dead process leaves the set", func() bool { running, _, _ := r.stats(); return len(running) == 0 })
	second := r.get("/die/ok")
	if second.status != 200 || second.body == first.body {
		t.Fatalf("a fresh process is expected: %q then %q", first.body, second.body)
	}
}

func TestABundleThatDoesNotLoadIsBadGatewayEveryTime(t *testing.T) {
	r := newRig(t, map[string]string{"noexp": noExp, "syntax": syntax}, nil)
	for _, name := range []string{"noexp", "syntax"} {
		for i := 0; i < 2; i++ {
			if a := r.get("/" + name + "/"); a.status != 502 || !strings.Contains(a.body, "failed to start") {
				t.Fatalf("%s: %d %q", name, a.status, a.body)
			}
		}
	}
	eventually(t, "nothing stays resident", func() bool { running, _, _ := r.stats(); return len(running) == 0 })
}

func TestARequestThatTakesLongerThanTheLimitIsCancelledAndItsProcessIsKilled(t *testing.T) {
	r := newRig(t, map[string]string{"slow": slowJS, "die": dieJS}, func(c *Config) { c.RequestTimeout = 400 * time.Millisecond })
	t0 := time.Now()
	a := r.get("/slow/?ms=5000")
	if a.status != 504 || a.body != "function timed out\n" {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if time.Since(t0) > 3*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
	eventually(t, "the process is killed", func() bool { running, _, _ := r.stats(); return !slices.Contains(running, "slow") })
}

func TestAFunctionOverItsHeapLimitDiesAndTheNextRequestIsServedByAFreshProcess(t *testing.T) {
	r := newRig(t, map[string]string{"heap": heapJS, "die": dieJS}, func(c *Config) { c.HeapMB = 64 })
	if a := r.get("/heap/"); a.status != 502 {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if a := r.get("/die/ok"); a.status != 200 { // another function in the same Pod is not touched
		t.Fatalf("%d", a.status)
	}
}

func TestNoCoreFileIsWrittenWhenAFunctionAborts(t *testing.T) {
	r := newRig(t, map[string]string{"heap": heapJS}, func(c *Config) { c.HeapMB = 64 })
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &lim); err != nil || lim.Cur != 0 || lim.Max != 0 {
		t.Fatalf("%v %v", lim, err)
	}
	t.Chdir(t.TempDir()) // where a core file would be written (core_pattern "core")
	if a := r.get("/heap/"); a.status != 502 {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if cores, _ := filepath.Glob("core*"); len(cores) != 0 {
		t.Fatalf("core files: %v", cores)
	}
}

func TestOverMaxConcurrentTheAnswerIs429AndTheFunctionIsNotTouched(t *testing.T) {
	r := newRig(t, map[string]string{"slow": slowJS}, func(c *Config) { c.MaxConcurrent = 2 })
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); statuses <- r.get("/slow/?ms=700").status }()
	}
	time.Sleep(300 * time.Millisecond) // both are in flight
	a := r.get("/slow/?ms=10")
	if a.status != 429 || a.header.Get("Retry-After") != "1" {
		t.Fatalf("%d %v", a.status, a.header)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		if s := <-statuses; s != 200 {
			t.Fatalf("a request in flight: %d", s)
		}
	}
	if a := r.get("/slow/?ms=10"); a.status != 200 { // the count went back down
		t.Fatalf("%d", a.status)
	}
}

func fns(n int, src string) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		m["f"+string(rune('a'+i))] = src
	}
	return m
}

func TestManyFunctionsInTurnInASmallBudgetNeverHoldMoreThanTheBudgetAndNeverFail(t *testing.T) {
	// A budget of 100 MiB and 40 MiB for a process before it is measured (a bare Node process is 40 to 60): two processes at most.
	r := newRig(t, fns(6, pidJS), func(c *Config) { c.BudgetMiB = 130; c.EstimateMiB = 55 })
	for round := 0; round < 3; round++ {
		for i := 0; i < 6; i++ {
			if a := r.get("/f" + string(rune('a'+i)) + "/"); a.status != 200 {
				t.Fatalf("function %d: %d %q", i, a.status, a.body)
			}
			if running, _, _ := r.stats(); len(running) > 2 {
				t.Fatalf("more than the budget holds: %v", running)
			}
		}
	}
	if _, evictions, busy := r.stats(); evictions == 0 || busy != 0 {
		t.Fatalf("evictions %d busy %d", evictions, busy)
	}
}

func TestRequestsForMoreFunctionsThanTheBudgetHoldsWaitForRoomAndAreAllServed(t *testing.T) {
	r := newRig(t, fns(5, slowJS), func(c *Config) {
		c.BudgetMiB = 130
		c.EstimateMiB = 55
		c.Wait = 8 * time.Second
		c.MaxWaiters = 50
	})
	var wg sync.WaitGroup
	bad := make(chan answer, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := r.get("/f" + string(rune('a'+i)) + "/?ms=300"); a.status != 200 {
				bad <- a
			}
		}()
	}
	wg.Wait()
	close(bad)
	for a := range bad {
		t.Errorf("%d %q", a.status, a.body)
	}
}

func TestWhenThereIsNoRoomWithinTheWaitTheAnswerIs503WithRetryAfter(t *testing.T) {
	r := newRig(t, fns(2, slowJS), func(c *Config) { c.BudgetMiB = 70; c.EstimateMiB = 55; c.Wait = 150 * time.Millisecond })
	done := make(chan int, 1)
	go func() { done <- r.get("/fa/?ms=1200").status }()
	time.Sleep(400 * time.Millisecond) // fa is in flight and nothing can be stopped
	a := r.get("/fb/?ms=10")
	if a.status != 503 || a.header.Get("Retry-After") != "1" {
		t.Fatalf("%d %v %q", a.status, a.header, a.body)
	}
	if s := <-done; s != 200 {
		t.Fatalf("the function in flight: %d", s)
	}
}

func TestAnIdleFunctionIsStoppedAndTheNextRequestStartsItAgain(t *testing.T) {
	r := newRig(t, map[string]string{"pid": pidJS}, func(c *Config) { c.IdleTimeout = 200 * time.Millisecond })
	first := r.get("/pid/").body
	time.Sleep(450 * time.Millisecond)
	if stopped := r.s.pool.ReapIdle(r.s.cfg.IdleTimeout, nil); !slices.Equal(stopped, []string{"pid"}) {
		t.Fatalf("stopped %v", stopped)
	}
	if second := r.get("/pid/").body; second == first {
		t.Fatalf("the same process %s", first)
	}
}

func TestSettingsFromTheEnvironment(t *testing.T) {
	env := map[string]string{}
	look := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	noCgroup := func() (float64, bool) { return 0, false }

	c, err := ConfigFromEnv(look, noCgroup)
	if err != nil || c.Functions != "/f" || c.Control != "/sockets" || c.HeapMB != 144 || c.MaxConcurrent != 20 || c.RequestTimeout != 10*time.Second || c.BudgetMiB != 330 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = ConfigFromEnv(look, func() (float64, bool) { return 384, true })
	if err != nil || c.BudgetMiB != 354 {
		t.Fatalf("the Pod's limit less the supervisor's 30: %v %v", c.BudgetMiB, err)
	}
	env["FN_BUDGET_MIB"] = "200"
	env["FN_ENV_ALLOW"] = " A, B ,,C"
	env["HEAP_MB"] = "96"
	c, err = ConfigFromEnv(look, func() (float64, bool) { return 384, true })
	if err != nil || c.BudgetMiB != 200 || c.HeapMB != 96 || !slices.Equal(c.EnvAllow, []string{"A", "B", "C"}) {
		t.Fatalf("set: %+v %v", c, err)
	}
	env["MAX_CONCURRENT"] = "twenty"
	if _, err = ConfigFromEnv(look, noCgroup); err == nil || !strings.Contains(err.Error(), "MAX_CONCURRENT") {
		t.Fatalf("a typo must be an error, got %v", err)
	}
	_ = context.Background
}
