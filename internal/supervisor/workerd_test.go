package supervisor

import (
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
	"sync/atomic"
	"testing"
	"time"
)

// Unit tests (no workerd) first, then tests that run a real workerd: they need it in $WORKERD_BIN or on the PATH, and are skipped without it.

func TestCapnpStringEscapesWhatBreaksALiteral(t *testing.T) {
	cases := map[string]string{
		`plain`:      `"plain"`,
		`a "quote"`:  `"a \"quote\""`,
		`back\slash`: `"back\\slash"`,
		"two\nlines": `"two\nlines"`,
		"tab\there":  `"tab\there"`,
		"bell\x07":   `"bell\x07"`,
		"del\x7f":    `"del\x7f"`,
		"日本語":        `"日本語"`,
		"cr\rlf":     `"cr\rlf"`,
	}
	for in, want := range cases {
		if got := capnpString(in); got != want {
			t.Errorf("capnpString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTheWorkerdConfigHasARouterAWorkerForEachFunctionAndTheAllowedVariablesOnly(t *testing.T) {
	cfg, err := BuildWorkerdConfig(WorkerdSpec{
		Base: "/sockets/wd-acme", Socket: "/sockets/wd-acme.sock", CompatDate: "2026-10-01",
		Functions: []WorkerdFunction{{"zeta", "/f/zeta/index.mjs"}, {"alpha", "/f/alpha/index.mjs"}},
		Env:       map[string]string{"B_KEY": `va"lue`, "A_KEY": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name = "router"`, `name = "fn-alpha"`, `name = "fn-zeta"`,
		`embed "../../f/alpha/index.mjs"`, `embed "../../f/zeta/index.mjs"`, // relative to the directory of the configuration: workerd does not read an absolute embed path
		`address = "unix:/sockets/wd-acme.sock"`, `service = "router"`,
		`compatibilityFlags = ["nodejs_compat_populate_process_env"]`,
		`(name = "A_KEY", text = "x")`, `(name = "B_KEY", text = "va\"lue")`,
		`(name = "F0", service = "fn-alpha"), (name = "F1", service = "fn-zeta")`, // sorted: alpha is F0
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the configuration lacks %s:\n%s", want, cfg)
		}
	}
	if strings.Index(cfg, `"A_KEY"`) > strings.Index(cfg, `"B_KEY"`) {
		t.Error("the variables are not sorted")
	}
	again, _ := BuildWorkerdConfig(WorkerdSpec{Base: "/sockets/wd-acme", Socket: "/sockets/wd-acme.sock", CompatDate: "2026-10-01",
		Functions: []WorkerdFunction{{"alpha", "/f/alpha/index.mjs"}, {"zeta", "/f/zeta/index.mjs"}}, Env: map[string]string{"A_KEY": "x", "B_KEY": `va"lue`}})
	if again != cfg {
		t.Error("the same input gives another configuration")
	}
}

func TestTheWorkerdConfigRefusesWhatItCannotMakeSafely(t *testing.T) {
	ok := WorkerdSpec{Base: "/s/x", Socket: "/s/x.sock", CompatDate: "2026-10-01", Functions: []WorkerdFunction{{"a", "/f/a/index.mjs"}}}
	bad := map[string]func(*WorkerdSpec){
		"no function":             func(s *WorkerdSpec) { s.Functions = nil },
		"no socket":               func(s *WorkerdSpec) { s.Socket = "" },
		"no date":                 func(s *WorkerdSpec) { s.CompatDate = "" },
		"a bad name":              func(s *WorkerdSpec) { s.Functions = []WorkerdFunction{{"A_b", "/f/x/index.mjs"}} },
		"a relative bundle":       func(s *WorkerdSpec) { s.Functions = []WorkerdFunction{{"a", "f/a/index.mjs"}} },
		"a bad variable":          func(s *WorkerdSpec) { s.Env = map[string]string{"A B": "x"} },
		"a variable that is code": func(s *WorkerdSpec) { s.Env = map[string]string{`X", text = "y"), (name = "Z`: "x"} },
	}
	for what, mutate := range bad {
		sp := ok
		mutate(&sp)
		if _, err := BuildWorkerdConfig(sp); err == nil {
			t.Errorf("%s: no error", what)
		}
	}
}

func TestTheLimitsKillAProcessThatUsesTooMuchMemoryOrLoopsForLongEnough(t *testing.T) {
	l := limits{MemoryMiB: 256, CPUQuotaPct: 100, Look: 2 * time.Second, RunawayLooks: 5}
	var st watchState
	// The first look only takes the CPU time as the starting point; a process that is busy for 4 looks and rests once starts counting again.
	cpu := int64(1_000_000)
	for i, busy := range []bool{true, true, true, true, false, true, true, true, true} {
		if busy {
			cpu += 1_900_000 // 95% of a core in 2 s
		} else {
			cpu += 10_000
		}
		if got := l.check(&st, cpu, 60); got != "" {
			t.Fatalf("look %d: %q, nothing is wrong yet", i, got)
		}
	}
	cpu += 1_900_000
	if got := l.check(&st, cpu, 60); got != "runaway" { // 5 busy looks in a row
		t.Fatalf("5 busy looks in a row: %q, want runaway", got)
	}
	// Memory is decided at once, whatever the CPU does.
	var st2 watchState
	if got := l.check(&st2, 0, 300); got != "memory" {
		t.Fatalf("300 MiB: %q, want memory", got)
	}
	if got := l.check(&st2, 0, 256); got != "" {
		t.Fatalf("exactly the limit: %q, want none", got)
	}
	// A CPU time that is not known (negative) is never a runaway.
	var st3 watchState
	for i := 0; i < 20; i++ {
		if got := l.check(&st3, -1, 10); got != "" {
			t.Fatalf("unknown CPU: %q", got)
		}
	}
	// "Nearly all of the quota" follows the quota: 50% of a core is busy at 0.45 of a core when the quota is 50.
	half := limits{MemoryMiB: 256, CPUQuotaPct: 50, Look: 2 * time.Second, RunawayLooks: 2}
	var st4 watchState
	half.check(&st4, 0, 1)
	half.check(&st4, 900_000, 1) // 0.45 of a core for 2 s = 900 ms
	if got := half.check(&st4, 1_800_000, 1); got != "runaway" {
		t.Fatalf("a 50%% quota used at 45%%: %q, want runaway", got)
	}
}

func TestTheCPUTimeIsReadFromAProcStatLineWhoseNameHasSpacesAndParentheses(t *testing.T) {
	line := "4242 (work (er) 1) S 1 4242 4242 0 -1 4194560 100 0 0 0 150 50 0 0 20 0 8 0 100 1000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	if us, ok := parseProcStat(line); !ok || us != 2_000_000 { // utime 150 + stime 50 ticks of 10 ms
		t.Errorf("parseProcStat = %d, %v; want 2000000 (200 ticks)", us, ok)
	}
	if _, ok := parseProcStat("garbage"); ok {
		t.Error("garbage is not a stat line")
	}
	if _, ok := parseProcStat("1 (x) S 1"); ok {
		t.Error("a short line is not a stat line")
	}
	self := procSampler{pid: os.Getpid()}
	if rss, ok := self.RSSMiB(); !ok || rss <= 0 {
		t.Errorf("RSS of this process: %v, %v", rss, ok)
	}
	if us, ok := self.CPUUsec(); !ok || us < 0 {
		t.Errorf("CPU of this process: %v, %v", us, ok)
	}
}

func TestTheCgroupOfACustomerIsMadeWithItsLimits(t *testing.T) {
	root := t.TempDir()
	cg, err := newCgroup(root, "acme", 256, 100, 64)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"memory.max": "268435456", "memory.swap.max": "0", "cpu.max": "100000 100000", "pids.max": "64"} {
		b, err := os.ReadFile(filepath.Join(cg.dir, file))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %q (%v), want %s", file, b, err, want)
		}
	}
	if filepath.Base(cg.dir) != "wd-acme" || filepath.Dir(cg.dir) != root {
		t.Errorf("the cgroup is %s", cg.dir)
	}
	cg.remove()
	if _, err := os.Stat(cg.dir); err == nil {
		// the files of the test are still in it: a real cgroup directory has none the kernel does not own, and rmdir works; here only an empty directory goes
		t.Log("a directory with files stays: only the real cgroup removes its own files")
	}
	half, err := newCgroup(root, "half", 128, 50, 10)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(half.dir, "cpu.max")); strings.TrimSpace(string(b)) != "50000 100000" {
		t.Errorf("cpu.max of a 50%% quota: %q", b)
	}
	for _, bad := range []string{"", "../x", "A", "a/b"} {
		if _, err := newCgroup(root, bad, 1, 1, 1); err == nil {
			t.Errorf("customer %q: no error", bad)
		}
	}
	if isCgroup2(root) {
		t.Error("a test directory is not on the cgroup file system")
	}
	s := cgroupSampler{dir: cg.dir}
	if err := os.WriteFile(filepath.Join(cg.dir, "cpu.stat"), []byte("usage_usec 1234\nuser_usec 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg.dir, "memory.current"), []byte("104857600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if us, ok := s.CPUUsec(); !ok || us != 1234 {
		t.Errorf("cpu.stat: %d, %v", us, ok)
	}
	if mib, ok := s.RSSMiB(); !ok || mib != 100 {
		t.Errorf("memory.current: %v, %v", mib, ok)
	}
}

func TestEachCustomerGetsAUserIdOfItsOwnAndTheRangeEnds(t *testing.T) {
	a := newUIDAlloc(20000, 2)
	x, _ := a.uid("acme")
	y, _ := a.uid("globex")
	x2, _ := a.uid("acme")
	if x != 20000 || y != 20001 || x2 != x {
		t.Errorf("uids %d %d %d", x, y, x2)
	}
	if _, err := a.uid("initech"); err == nil {
		t.Error("a third customer in a range of 2")
	}
}

func TestTheRouteSplitsTheCustomerTheFunctionAndTheRest(t *testing.T) {
	one := &Server{cfg: Config{}}
	for path, want := range map[string][4]interface{}{
		"/hello":     {"default", "hello", "/", true},
		"/hello/a/b": {"default", "hello", "/a/b", true},
		"/Hello/a":   {"default", "Hello", "/a", false},
		"/":          {"default", "", "/", false},
	} {
		c, n, r, ok := one.route(path)
		if c != want[0] || n != want[1] || r != want[2] || ok != want[3] {
			t.Errorf("%s: %q %q %q %v, want %v", path, c, n, r, ok, want)
		}
	}
	many := &Server{cfg: Config{CustomersDir: "/c"}}
	for path, want := range map[string][4]interface{}{
		"/acme/hello":     {"acme", "hello", "/", true},
		"/acme/hello/a?b": {"acme", "hello", "/a?b", true},
		"/acme":           {"", "", "", false},
		"/ACME/hello":     {"ACME", "hello", "/", false},
	} {
		c, n, r, ok := many.route(path)
		if c != want[0] || n != want[1] || r != want[2] || ok != want[3] {
			t.Errorf("%s: %q %q %q %v, want %v", path, c, n, r, ok, want)
		}
	}
}

func TestSecretsOfACustomerAreReadFromItsFileAndAMistakeIsAnError(t *testing.T) {
	dir := t.TempDir()
	if m, err := readSecrets(filepath.Join(dir, "none.json")); err != nil || m != nil {
		t.Errorf("no file: %v %v", m, err)
	}
	good := filepath.Join(dir, "good.json")
	_ = os.WriteFile(good, []byte(`{"API_KEY":"k","DB_URL":"u"}`), 0o600)
	if m, err := readSecrets(good); err != nil || m["API_KEY"] != "k" || len(m) != 2 {
		t.Errorf("good: %v %v", m, err)
	}
	for name, body := range map[string]string{"badname": `{"A B":"x"}`, "notjson": `{`, "notstrings": `{"A":1}`} {
		p := filepath.Join(dir, name+".json")
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, err := readSecrets(p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestTheSettingsAreReadAndATypoIsAnError(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	none := func() (float64, bool) { return 0, false }
	c, err := ConfigFromEnv(env(nil), none)
	if err != nil {
		t.Fatal(err)
	}
	if c.Functions != "/f" || c.Control != "/sockets" || c.MaxConcurrent != 20 || c.RequestTimeout != 10*time.Second || c.BudgetMiB != 330 ||
		c.MemoryLook != 250*time.Millisecond || c.MemoryMiB != 256 || c.CPUQuotaPct != 100 || c.Look != 2*time.Second || c.RunawayLooks != 5 || c.EstimateMiB != 50 || c.PidsLimit != 64 || c.Workerd != "/sup/workerd" {
		t.Errorf("defaults: %+v", c)
	}
	if c, _ = ConfigFromEnv(env(nil), func() (float64, bool) { return 384, true }); c.BudgetMiB != 354 {
		t.Errorf("the Pod's limit less the supervisor's 30: %v", c.BudgetMiB)
	}
	given := map[string]string{"FN_ESTIMATE_MIB": "80", "WD_MEMORY_MIB": "128", "FN_BUDGET_MIB": "200", "FN_ENV_ALLOW": " A, B ,,C", "RUNTIME": "workerd"}
	if c, err = ConfigFromEnv(env(given), none); err != nil || c.EstimateMiB != 80 || c.MemoryMiB != 128 || c.BudgetMiB != 200 || !slices.Equal(c.EnvAllow, []string{"A", "B", "C"}) {
		t.Errorf("given: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"RUNTIME": "node"}, {"RUNTIME": "deno"}, {"WD_MEMORY_MIB": "0"}, {"WD_RUNAWAY_LOOKS": "x"}, {"MAX_CONCURRENT": "twenty"}, {"WD_UID_BASE": "10", "WD_UID_COUNT": "0"}} {
		if _, err := ConfigFromEnv(env(bad), none); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

// ---- tests with a real workerd ----

func workerdBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("WORKERD_BIN"); p != "" {
		return p
	}
	p, err := exec.LookPath("workerd")
	if err != nil {
		t.Skip("workerd is not installed (set WORKERD_BIN)")
	}
	return p
}

const (
	wHello  = `export default (req) => { const u = new URL(req.url); return new Response('hello ' + u.pathname + u.search, { headers: { 'x-method': req.method } }); };`
	wFetch  = `export default { fetch: (req) => new Response('from fetch ' + new URL(req.url).pathname) };`
	wThrow  = `export default () => { throw new Error('boom'); };`
	wNoResp = `export default () => 'not a response';`
	wNoExp  = `export const x = 1;`
	wSecret = `export default (req, env) => Response.json({ process: process.env.API_KEY ?? null, env: env.API_KEY ?? null, other: process.env.OTHER ?? null });`
	wLoop   = `export default () => { while (true) {} };`
	wSlow   = `export default async (req) => { const ms = Number(new URL(req.url).searchParams.get('ms') ?? 3000); await new Promise((r) => setTimeout(r, ms)); return new Response('late'); };`
	wBomb   = `export default () => { const keep = []; for (;;) { const a = new Uint8Array(16 << 20); a.fill(1); keep.push(a); } };`
	wCount  = `let n = 0; export default () => new Response(String(++n));`
)

type wrig struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	dir string
}

// newWRig serves customers (customer -> function -> JavaScript; "secrets.json" as a function name is the customer's secrets file) from a CUSTOMERS_DIR.
func newWRig(t *testing.T, customers map[string]map[string]string, extra map[string]string, mutate func(*Config)) *wrig {
	t.Helper()
	bin := workerdBin(t)
	dir := t.TempDir()
	for customer, fns := range customers {
		for name, src := range fns {
			path := filepath.Join(dir, customer, name, "index.mjs")
			if name == "secrets.json" {
				path = filepath.Join(dir, customer, "secrets.json")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	control, err := os.MkdirTemp("", "wd") // short: a Unix socket's path is 107 bytes at most
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(control) })
	vars := map[string]string{"WORKERD_BIN": bin, "CUSTOMERS_DIR": dir, "CONTROL_DIR": control, "FN_BUDGET_MIB": "4000", "FN_MIN_RESIDENCY_MS": "0",
		"REQUEST_MS": "8000", "START_MS": "20000", "WD_LOOK_MS": "200", "WD_MEMORY_LOOK_MS": "50", "WD_RUNAWAY_LOOKS": "5", "WD_MEMORY_MIB": "256", "FUNCTIONS_DIR": dir}
	for k, v := range extra {
		vars[k] = v
	}
	cfg, err := ConfigFromEnv(func(k string) (string, bool) { v, ok := vars[k]; return v, ok }, func() (float64, bool) { return 0, false })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Getenv = func(k string) string { return vars["ENV_"+k] }
	if mutate != nil {
		mutate(&cfg)
	}
	r := &wrig{t: t, dir: dir}
	r.s, err = New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ts = httptest.NewServer(r.s)
	t.Cleanup(func() { r.ts.Close(); r.s.Stop() })
	return r
}

func (r *wrig) do(path string) answer {
	r.t.Helper()
	res, err := http.Get(r.ts.URL + path)
	if err != nil {
		return answer{status: 0, body: err.Error()}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return answer{res.StatusCode, string(b), res.Header}
}

func (r *wrig) running() []string {
	r.t.Helper()
	var st struct {
		Running []string `json:"running"`
		Starts  int      `json:"starts"`
	}
	res, err := http.Get(r.ts.URL + "/_stats")
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		r.t.Fatal(err)
	}
	slices.Sort(st.Running)
	return st.Running
}

func (r *wrig) starts() int {
	r.t.Helper()
	var st struct {
		Starts int `json:"starts"`
	}
	res, err := http.Get(r.ts.URL + "/_stats")
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	_ = json.NewDecoder(res.Body).Decode(&st)
	return st.Starts
}

func TestWorkerdServesAFunctionAndPassesThePathTheQueryAndTheMethod(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"hello": wHello, "viafetch": wFetch}}, nil, nil)
	a := r.do("/acme/hello/a/b?x=1")
	if a.status != 200 || a.body != "hello /a/b?x=1" {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if a.header.Get("x-fn-ms") == "" || a.header.Get("x-method") != "GET" {
		t.Errorf("headers: %v", a.header)
	}
	if a := r.do("/acme/hello"); a.status != 200 || a.body != "hello /" {
		t.Errorf("no rest: %d %q", a.status, a.body)
	}
	if a := r.do("/acme/viafetch/z"); a.status != 200 || a.body != "from fetch /z" {
		t.Errorf("an object with a fetch method: %d %q", a.status, a.body)
	}
}

func TestWorkerdAnswersForTheFunctionsThatFailAndTheOnesThatAreNotThere(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"hello": wHello, "throws": wThrow, "noresponse": wNoResp, "noexport": wNoExp}}, nil, nil)
	if a := r.do("/acme/throws"); a.status != 500 || !strings.Contains(a.body, "function error") {
		t.Errorf("a handler that throws: %d %q", a.status, a.body)
	}
	if a := r.do("/acme/noresponse"); a.status != 500 {
		t.Errorf("a handler that returns a string: %d", a.status)
	}
	if a := r.do("/acme/noexport"); a.status != 500 {
		t.Errorf("a bundle without a handler: %d", a.status)
	}
	if a := r.do("/acme/nothere"); a.status != 404 {
		t.Errorf("no such function: %d", a.status)
	}
	if a := r.do("/nobody/hello"); a.status != 404 {
		t.Errorf("no such customer: %d", a.status)
	}
	if a := r.do("/ACME/hello"); a.status != 400 {
		t.Errorf("a bad name: %d", a.status)
	}
	if a := r.do("/acme/hello"); a.status != 200 {
		t.Errorf("the others still work: %d", a.status)
	}
}

func TestWorkerdABundleThatDoesNotParseIsLeftOutAndTheCustomersOtherFunctionsRun(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"hello": wHello, "syntax": `export default (`}}, nil, nil)
	if a := r.do("/acme/hello"); a.status != 200 {
		t.Fatalf("the good function of a customer with a bad one: %d %q", a.status, a.body)
	}
	if a := r.do("/acme/syntax"); a.status != 502 {
		t.Errorf("the bundle that does not parse: %d, want 502", a.status)
	}
}

func TestWorkerdTheBundlesAreCheckedByOneThrowawayProcessAndNotAgainWhileTheyStayTheSame(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"a": wHello, "b": wHello}}, nil, nil)
	if a := r.do("/acme/a"); a.status != 200 {
		t.Fatalf("a: %d %q", a.status, a.body)
	}
	if n := r.s.wl.probes; n != 1 {
		t.Fatalf("the probes of two good bundles: %d, want 1 (one throwaway process for both)", n)
	}
	_ = r.s.wpool.ReapIdle(0, nil)
	eventually(t, "the process is gone", func() bool { return len(r.running()) == 0 })
	if a := r.do("/acme/b"); a.status != 200 {
		t.Fatalf("b after the restart: %d %q", a.status, a.body)
	}
	if n := r.s.wl.probes; n != 1 {
		t.Errorf("the probes after a restart with the same bundles: %d, want still 1", n)
	}
	// A bundle that is changed is checked again, and a bad one is found among the good.
	if err := os.WriteFile(filepath.Join(r.dir, "acme", "b", "index.mjs"), []byte("export default ("), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = r.s.wpool.ReapIdle(0, nil)
	eventually(t, "the process is gone again", func() bool { return len(r.running()) == 0 })
	if a := r.do("/acme/a"); a.status != 200 {
		t.Errorf("a with a bad neighbour: %d %q", a.status, a.body)
	}
	if a := r.do("/acme/b"); a.status != 502 {
		t.Errorf("the bundle that was changed into a bad one: %d, want 502", a.status)
	}
}

func TestWorkerdFunctionsReadOnlyTheirCustomersAllowedVariables(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{
		"acme":   {"s": wSecret, "secrets.json": `{"API_KEY":"acme-key"}`},
		"globex": {"s": wSecret, "secrets.json": `{"API_KEY":"globex-key"}`},
		"plain":  {"s": wSecret},
	}, map[string]string{"FN_ENV_ALLOW": "OTHER", "ENV_OTHER": "from-the-supervisor", "ENV_NOT_ALLOWED": "no"}, nil)
	read := func(path string) (p, e, o *string) {
		var v struct{ Process, Env, Other *string }
		a := r.do(path)
		if a.status != 200 {
			t.Fatalf("%s: %d %q", path, a.status, a.body)
		}
		if err := json.Unmarshal([]byte(strings.NewReplacer(`"process"`, `"Process"`, `"env"`, `"Env"`, `"other"`, `"Other"`).Replace(a.body)), &v); err != nil {
			t.Fatal(err)
		}
		return v.Process, v.Env, v.Other
	}
	if p, e, o := read("/acme/s"); p == nil || *p != "acme-key" || e == nil || *e != "acme-key" || o == nil || *o != "from-the-supervisor" {
		t.Errorf("acme: %v %v %v", p, e, o)
	}
	if p, _, _ := read("/globex/s"); p == nil || *p != "globex-key" {
		t.Errorf("globex gets its own: %v", p)
	}
	if p, _, o := read("/plain/s"); p != nil || o == nil || *o != "from-the-supervisor" {
		t.Errorf("a customer with no secrets file: %v %v", p, o)
	}
}

func TestWorkerdRunsOneProcessForEachCustomerWhateverTheirNumberOfFunctions(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"a": wCount, "b": wCount}, "globex": {"a": wCount}}, nil, nil)
	if got := r.running(); len(got) != 0 {
		t.Fatalf("nothing runs before the first request: %v", got)
	}
	r.do("/acme/a")
	r.do("/acme/b")
	if got := r.running(); !slices.Equal(got, []string{"acme"}) {
		t.Fatalf("two functions of one customer: %v, want [acme]", got)
	}
	r.do("/globex/a")
	if got := r.running(); !slices.Equal(got, []string{"acme", "globex"}) || r.starts() != 2 {
		t.Fatalf("two customers: %v, %d starts", got, r.starts())
	}
	// the functions of a customer share the process (a counter in one module is its own: each function is a Worker), and the customer's processes are apart
	if a := r.do("/acme/a"); a.body != "2" {
		t.Errorf("acme/a, the second request: %q", a.body)
	}
	if a := r.do("/globex/a"); a.body != "2" {
		t.Errorf("globex/a, the second request: %q", a.body)
	}
}

func TestWorkerdStopsAnIdleCustomerAndStartsItAgainOnTheNextRequest(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"c": wCount}}, map[string]string{"IDLE_MS": "600"}, nil)
	if a := r.do("/acme/c"); a.body != "1" {
		t.Fatalf("first: %q", a.body)
	}
	time.Sleep(300 * time.Millisecond)
	if stopped := r.s.wpool.ReapIdle(200*time.Millisecond, nil); !slices.Equal(stopped, []string{"acme"}) {
		t.Fatalf("stopped %v, want [acme]", stopped)
	}
	eventually(t, "the idle process is gone", func() bool { return len(r.running()) == 0 })
	if a := r.do("/acme/c"); a.status != 200 || a.body != "1" || r.starts() != 2 {
		t.Fatalf("after the idle stop: %d %q, %d starts", a.status, a.body, r.starts())
	}
}

func TestWorkerdAnOverrunKillsTheCustomersProcessAnd504AndTheNextRequestIsServed(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"slow": wSlow, "hello": wHello}, "globex": {"hello": wHello}}, map[string]string{"REQUEST_MS": "400"}, nil)
	if a := r.do("/globex/hello"); a.status != 200 {
		t.Fatalf("globex: %d", a.status)
	}
	if a := r.do("/acme/slow?ms=5000"); a.status != 504 {
		t.Fatalf("an overrun: %d, want 504", a.status)
	}
	if a := r.do("/acme/hello"); a.status != 200 || r.starts() < 3 {
		t.Fatalf("the next request: %d, %d starts (a fresh process)", a.status, r.starts())
	}
	if a := r.do("/globex/hello"); a.status != 200 {
		t.Fatalf("the neighbour: %d", a.status)
	}
}

func TestWorkerdTooManyConcurrentRequestsToOneFunctionAre429(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"slow": wSlow}}, map[string]string{"MAX_CONCURRENT": "2"}, nil)
	var wg sync.WaitGroup
	var got429 atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := r.do("/acme/slow?ms=700")
			if a.status == 429 {
				got429.Add(1)
				if a.header.Get("retry-after") == "" {
					t.Error("429 without Retry-After")
				}
			}
		}()
	}
	wg.Wait()
	if got429.Load() < 1 {
		t.Error("no 429 for 4 requests at once with MAX_CONCURRENT=2")
	}
}

func TestWorkerdALoopIsStoppedByTheLookAndTheNeighbourNeverFeelsIt(t *testing.T) {
	// 5 looks of 200 ms: the loop is stopped after about a second (the defaults, 5 looks of 2 s, are about ten seconds: see the test below).
	r := newWRig(t, map[string]map[string]string{"acme": {"loop": wLoop, "hello": wHello}, "globex": {"hello": wHello}}, nil, nil)
	if a := r.do("/globex/hello"); a.status != 200 {
		t.Fatalf("globex: %d", a.status)
	}
	var bad atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the neighbour is asked all the time
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if a := r.do("/globex/hello"); a.status != 200 {
				bad.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t0 := time.Now()
	a := r.do("/acme/loop")
	took := time.Since(t0)
	close(stop)
	wg.Wait()
	if a.status != 502 {
		t.Errorf("the loop: %d %q, want 502 (its process was killed)", a.status, a.body)
	}
	if took > 6*time.Second {
		t.Errorf("the loop took %v to be stopped", took)
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests of the neighbour failed", bad.Load())
	}
	if a := r.do("/acme/hello"); a.status != 200 {
		t.Errorf("the next request of the customer: %d", a.status)
	}
}

func TestWorkerdALoopIsStoppedInAboutTenSecondsWithTheDefaultSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about ten seconds")
	}
	r := newWRig(t, map[string]map[string]string{"acme": {"loop": wLoop, "hello": wHello}}, map[string]string{"WD_LOOK_MS": "2000", "REQUEST_MS": "60000"}, nil)
	t0 := time.Now()
	a := r.do("/acme/loop")
	took := time.Since(t0)
	if a.status != 502 {
		t.Errorf("%d %q, want 502", a.status, a.body)
	}
	if took < 8*time.Second || took > 16*time.Second {
		t.Errorf("a loop was stopped after %v, want about 10 s (5 looks of 2 s)", took)
	}
	if a := r.do("/acme/hello"); a.status != 200 {
		t.Errorf("the next request: %d", a.status)
	}
}

func TestWorkerdAProcessThatUsesTooMuchMemoryIsKilledAtTheLimitAndTheNeighbourIsUnharmed(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"acme": {"bomb": wBomb, "hello": wHello}, "globex": {"hello": wHello}}, map[string]string{"WD_MEMORY_MIB": "160", "REQUEST_MS": "20000"}, nil)
	if a := r.do("/globex/hello"); a.status != 200 {
		t.Fatalf("globex: %d", a.status)
	}
	var bad atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if a := r.do("/globex/hello"); a.status != 200 {
				bad.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	t0 := time.Now()
	a := r.do("/acme/bomb")
	took := time.Since(t0)
	close(stop)
	wg.Wait()
	if a.status != 502 {
		t.Errorf("the memory bomb: %d %q, want 502", a.status, a.body)
	}
	if took > 10*time.Second {
		t.Errorf("the bomb took %v to be stopped", took)
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests of the neighbour failed", bad.Load())
	}
	if a := r.do("/acme/hello"); a.status != 200 {
		t.Errorf("the next request of the customer: %d", a.status)
	}
}

func TestWorkerdWithoutCustomersServesTheFunctionsDirectoryAsTheOnlyCustomer(t *testing.T) {
	r := newWRig(t, map[string]map[string]string{"x": {"hello": wHello}}, nil, func(c *Config) {
		c.Functions = filepath.Join(c.CustomersDir, "x") // the functions of "x" are the Project's: /<function>/...
		c.CustomersDir = ""
	})
	if a := r.do("/hello/p"); a.status != 200 || a.body != "hello /p" {
		t.Fatalf("%d %q", a.status, a.body)
	}
	if got := r.running(); !slices.Equal(got, []string{"default"}) {
		t.Errorf("running: %v", got)
	}
}
