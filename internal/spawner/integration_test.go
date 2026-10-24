package spawner

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ---- the stack works

func TestACustomerIsPutAndServedThroughTheRouterAndNothingSecretIsListed(t *testing.T) {
	r := newRig(t, nil, nil)
	if got := r.s.List(); len(got) != 0 {
		t.Fatalf("a new spawner holds %v", got)
	}
	r.mustPut("acme", fns{"hello": jsHello, "count": jsCount}, putOpts{secrets: map[string]string{"API_KEY": "sekrit-acme"}})
	if a := r.call("/acme/hello/a/b?x=1"); a.status != 200 || a.body != "hello /a/b?x=1" || a.header.Get("x-fn-ms") == "" {
		t.Fatalf("hello: %d %q %v", a.status, a.body, a.header)
	}
	if a := r.call("/acme/count"); a.body != "1" {
		t.Errorf("count: %q", a.body)
	}
	if a := r.call("/acme/count"); a.body != "2" { // the same process: one workerd for the customer
		t.Errorf("count again: %q", a.body)
	}
	if got := workerds(r.uidOf("acme")); len(got) != 1 {
		t.Errorf("workerd processes of acme: %v, want one for both functions", got)
	}
	res, err := r.agent.Get(r.ctlURL + "/v1/customers")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var list []Info
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || len(list) != 1 || list[0].ID != "acme" || list[0].Secrets != 1 || !list[0].Running || !slices.Equal(list[0].Functions, []string{"count", "hello"}) {
		t.Errorf("the list: %+v %v", list, err)
	}
	for _, c := range []string{"/acme/nothere", "/nobody/hello", "/acme", "/ACME/hello", "/acme/%68ello"} {
		if a := r.call(c); a.status < 400 {
			t.Errorf("%s: %d", c, a.status)
		}
	}
	if a := r.call("/acme/nothere"); a.status != 404 {
		t.Errorf("a function that is not there: %d", a.status)
	}
	if a := r.call("/nobody/hello"); a.status != 404 {
		t.Errorf("a customer that is not there: %d", a.status)
	}
}

// ---- the wall between customers

func TestAProcessIsTheCustomersUserWithNoGroupsNoCapabilitiesNoNewPrivsAndAnEmptyReadOnlyDirectory(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"hello": jsHello}, putOpts{secrets: map[string]string{"API_KEY": "k"}})
	r.call("/acme/hello")
	pid := r.onlyWorkerd("acme")
	uid := r.uidOf("acme")
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	status := string(b)
	field := func(name string) string {
		for _, l := range strings.Split(status, "\n") {
			if rest, ok := strings.CutPrefix(l, name+":"); ok {
				return strings.Join(strings.Fields(rest), " ")
			}
		}
		return "?"
	}
	u := strconv.Itoa(uid)
	for name, want := range map[string]string{
		"Uid": u + " " + u + " " + u + " " + u, "Gid": u + " " + u + " " + u + " " + u, "Groups": "",
		"NoNewPrivs": "1", "CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000", "CapAmb": "0000000000000000",
	} {
		if got := field(name); got != want {
			t.Errorf("%s of the process: %q, want %q", name, got, want)
		}
	}
	limits, _ := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	for _, want := range []string{"Max core file size", "Max locked memory"} {
		found := false
		for _, l := range strings.Split(string(limits), "\n") {
			if f := strings.Fields(l); strings.HasPrefix(l, want) && f[len(f)-3] == "0" && f[len(f)-2] == "0" { // soft and hard, then the unit
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not 0:\n%s", want, limits)
		}
	}
	want := strconv.Itoa(r.cfg.PidsLimit * 4)
	if !strings.Contains(string(limits), "Max processes             "+want) {
		t.Errorf("RLIMIT_NPROC is not %s:\n%s", want, limits)
	}
	// Its directory: root:<its group>, 0750, and nothing in it any more (the configuration and the bundles were deleted when workerd answered).
	cwd := r.runDirOf("acme") // (the spawner cannot read /proc/<pid>/cwd of a process of another user: it has no CAP_SYS_PTRACE, which is as it should be)
	st, err := os.Stat(cwd)
	if err != nil {
		t.Fatal(err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	if st.Mode().Perm() != 0o750 || sys.Uid != 0 || int(sys.Gid) != uid {
		t.Errorf("the directory %s is %v owner %d:%d", cwd, st.Mode(), sys.Uid, sys.Gid)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("the directory still has %d entries", len(entries))
	}
	if out, err := asUser(uid, "touch", filepath.Join(cwd, "x")); err == nil {
		t.Errorf("the customer could write into its own directory: %s", out)
	}
}

func TestACustomerCannotTouchAnotherCustomersSocketFilesProcessOrSecrets(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"who": jsWhoami("acme")}, putOpts{secrets: map[string]string{"API_KEY": "sekrit-acme"}})
	r.mustPut("globex", fns{"who": jsWhoami("globex")}, putOpts{secrets: map[string]string{"API_KEY": "sekrit-globex"}})
	r.call("/acme/who")
	r.call("/globex/who")
	acme, globex := r.uidOf("acme"), r.uidOf("globex")
	if acme == globex {
		t.Fatal("two customers, one user id")
	}
	gpid := r.onlyWorkerd("globex")
	gdir := r.runDirOf("globex")
	var gsock string
	entries, _ := os.ReadDir(r.control)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "w-globex.") {
			gsock = filepath.Join(r.control, e.Name())
		}
	}
	if gsock == "" {
		t.Fatalf("no socket of globex in %s", r.control)
	}
	// A file in globex's directory that only globex's group may read: the test is a real test only if globex itself can read it.
	planted := filepath.Join(gdir, "planted")
	if err := os.WriteFile(planted, []byte("globex-only"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(planted, 0, globex); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(planted, 0o440); err != nil { // (the spawner's umask is 077)
		t.Fatal(err)
	}
	if out, err := asUser(globex, "cat", planted); err != nil || out != "globex-only" {
		t.Fatalf("globex cannot read its own file: %q %v", out, err)
	}

	// Each is refused by the kernel (not by a missing command): the output must say permission denied or operation not permitted.
	pid := strconv.Itoa(gpid)
	connect := os.Args[0] + " connect "
	refused := map[string]string{
		"list the shared sockets directory":      "ls " + r.control,
		"connect to globex's socket":             connect + gsock,
		"connect to the spawner's socket":        connect + filepath.Join(r.control, "spawner.sock"),
		"list the state directory":               "ls " + r.state,
		"list globex's directory":                "ls " + gdir,
		"read a file in globex's directory":      "cat " + planted,
		"read globex's environment":              "cat /proc/" + pid + "/environ",
		"read globex's memory map":               "cat /proc/" + pid + "/maps",
		"open globex's file descriptors":         "ls /proc/" + pid + "/fd",
		"kill globex's process":                  "kill -9 " + pid,
		"stop globex's process":                  "kill -STOP " + pid,
		"change the owner of globex's directory": "chown " + strconv.Itoa(acme) + " " + gdir,
		"write into globex's directory":          "touch " + gdir + "/x",
		"change the mode of globex's directory":  "chmod 777 " + gdir,
	}
	for what, cmd := range refused {
		out, err := asUser(acme, "sh", "-c", cmd)
		if err == nil {
			t.Errorf("acme could %s: %s", what, out)
		} else if !strings.Contains(strings.ToLower(out), "permission denied") && !strings.Contains(strings.ToLower(out), "not permitted") {
			t.Errorf("acme was stopped from trying to %s, but not by a permission: %q", what, out)
		}
	}
	// The same commands as globex, where they are its own, work: the refusals above are not an accident of the commands.
	for what, cmd := range map[string][]string{
		"globex lists its directory":   {"ls", gdir},
		"globex reads its environment": {"cat", fmt.Sprintf("/proc/%d/environ", gpid)},
	} {
		if out, err := asUser(globex, cmd...); err != nil {
			t.Errorf("%s: %v %s", what, err, out)
		}
	}
	// Each answers with its own identity and secret, through the same router.
	for id, secret := range map[string]string{"acme": "sekrit-acme", "globex": "sekrit-globex"} {
		a := r.call("/" + id + "/who")
		var got struct {
			ID     string
			Secret string
			Proc   string
			Keys   []string
		}
		if err := json.Unmarshal([]byte(a.body), &got); err != nil || got.ID != id || got.Secret != secret || got.Proc != secret || !slices.Equal(got.Keys, []string{"API_KEY"}) {
			t.Errorf("%s answered %d %s (%v)", id, a.status, a.body, err)
		}
	}
	// The secret is not in the other's command line either (it is in nobody's: it is in the environment of its own process only).
	for _, pid := range []int{gpid, r.onlyWorkerd("acme")} {
		cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(cmdline), "sekrit") {
			t.Errorf("a secret is in the command line of %d: %q", pid, cmdline)
		}
	}
}

func TestSecretsAreEnvironmentOfTheCustomersOwnProcessOnlyNeverOnDiskNeverInTheLogAndANameCannotBeTheLoaders(t *testing.T) {
	r := newRig(t, nil, nil)
	secret := "S3cr3t-value-0123456789"
	r.mustPut("acme", fns{"who": jsWhoami("acme")}, putOpts{secrets: map[string]string{"API_KEY": secret, "LD_PRELOAD": "/nonexistent/evil.so"}})
	a := r.call("/acme/who")
	var got struct {
		Secret string
		Keys   []string
		LD     string
	}
	if err := json.Unmarshal([]byte(a.body), &got); err != nil || got.Secret != secret || !slices.Equal(got.Keys, []string{"API_KEY", "LD_PRELOAD"}) || got.LD != "/nonexistent/evil.so" {
		t.Fatalf("%d %s %v", a.status, a.body, err)
	}
	// A secret called LD_PRELOAD is an ordinary binding for the function: the loader never sees it (the process's variable is FNSECRET_LD_PRELOAD), so workerd started.
	pid := r.onlyWorkerd("acme")
	envOut, err := asUser(r.uidOf("acme"), "cat", fmt.Sprintf("/proc/%d/environ", pid)) // only the process's own user can read it (root with four capabilities cannot)
	if err != nil {
		t.Fatal(err)
	}
	env := []byte(envOut)
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		t.Error("the spawner can read the environment of a customer's process")
	}
	if !strings.Contains(string(env), "FNSECRET_API_KEY="+secret) || strings.Contains(string(env), "\x00LD_PRELOAD=") || strings.HasPrefix(string(env), "LD_PRELOAD=") {
		t.Errorf("the environment of the process: %q", env)
	}
	// Not on disk, not in the log: the values are in memory, and in the process's environment.
	for _, root := range []string{r.control, r.state} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if b, _ := os.ReadFile(path); strings.Contains(string(b), secret) {
				t.Errorf("the secret is in the file %s", path)
			}
			return nil
		})
	}
	if strings.Contains(r.logs.String(), secret) {
		t.Error("the secret is in the log")
	}
	// A refused put does not echo the secrets (a put with a name that is not a name).
	if a := r.put("acme", fns{"who": jsWhoami("acme")}, putOpts{secrets: map[string]string{"BAD NAME": secret}}); a.status != 400 || strings.Contains(a.body, secret) {
		t.Errorf("a bad secret name: %d %s", a.status, a.body)
	}
}

func TestAFunctionCannotFetchTheMachineItRunsOnOrThePlatformsOwnPorts(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"f": jsFetch}, putOpts{})
	for _, target := range []string{r.routerURL + "/_healthz", r.ctlURL + "/healthz", "http://127.0.0.1:1/", "http://[::1]:1/"} {
		a := r.call("/acme/f?u=" + target)
		if !strings.Contains(a.body, "refused") && !strings.Contains(a.body, "fetched 5") && !strings.Contains(a.body, "fetched 4") {
			t.Errorf("fetch %s: %d %q", target, a.status, a.body)
		}
		if strings.Contains(a.body, "fetched 200") {
			t.Errorf("a function reached %s: %q", target, a.body)
		}
	}
}

// ---- limits, as before, now in a VM with several customers

func (r *rig) hammer(path string, bad *atomic.Int32) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if a := r.call(path); a.status != 200 {
				bad.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return func() { close(done); wg.Wait() }
}

func TestALoopIsStoppedByTheLooksAndTheNeighbourNeverFeelsIt(t *testing.T) {
	// 5 looks of 200 ms: stopped after about a second (the defaults, 5 looks of 2 s, are about ten seconds: see the next test).
	r := newRig(t, nil, nil)
	// The function's own CPU limit is raised to its maximum so that the looks (about a second here) are what stops it: this is the backup, tested on its own.
	r.mustPut("acme", fns{"loop": jsLoop, "hello": jsHello}, putOpts{fn: map[string]map[string]any{"loop": {"cpu_ms": 5000}}})
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	if a := r.call("/globex/hello"); a.status != 200 {
		t.Fatalf("globex: %d", a.status)
	}
	var bad atomic.Int32
	stop := r.hammer("/globex/hello", &bad)
	t0 := time.Now()
	a := r.call("/acme/loop")
	took := time.Since(t0)
	stop()
	if a.status != 502 {
		t.Errorf("the loop: %d %q, want 502 (its process was killed)", a.status, a.body)
	}
	if took > 6*time.Second {
		t.Errorf("the loop took %v to be stopped", took)
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests of the neighbour failed", bad.Load())
	}
	if a := r.call("/acme/hello"); a.status != 200 {
		t.Errorf("the next request of the customer: %d", a.status)
	}
	if !strings.Contains(r.logs.String(), "reason=runaway") {
		t.Error("the log does not say that the process was killed for running away")
	}
}

func TestALoopIsStoppedInAboutTenSecondsWithTheDefaultSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about ten seconds")
	}
	// TurboFan on and a loop it compiles without interrupt checks: the function's own CPU limit cannot stop it (fnlimits_test.go), so the looks do, at about ten seconds.
	r := newRig(t, map[string]string{"WD_LOOK_MS": "2000", "WD_V8_FLAGS": "none"}, map[string]string{"REQUEST_MS": "60000"})
	r.mustPut("acme", fns{"loop": jsUninterruptible, "hello": jsHello}, putOpts{})
	t0 := time.Now()
	a := r.call("/acme/loop")
	took := time.Since(t0)
	if a.status != 502 {
		t.Errorf("%d %q, want 502", a.status, a.body)
	}
	if took < 8*time.Second || took > 16*time.Second {
		t.Errorf("a loop was stopped after %v, want about 10 s (5 looks of 2 s)", took)
	}
	if a := r.call("/acme/hello"); a.status != 200 {
		t.Errorf("the next request: %d", a.status)
	}
}

func TestAProcessThatUsesTooMuchMemoryIsKilledAtTheLimitAndTheNeighbourIsUnharmed(t *testing.T) {
	r := newRig(t, nil, map[string]string{"REQUEST_MS": "20000"})
	// The function's own ArrayBuffer limit is raised to its maximum, so that the process limit (160 MiB) is what stops the bomb: the backup, tested on its own.
	r.mustPut("acme", fns{"bomb": jsBomb, "hello": jsHello}, putOpts{limits: map[string]any{"memory_mib": 160}, fn: map[string]map[string]any{"bomb": {"external_mb": 512}}})
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	if a := r.call("/globex/hello"); a.status != 200 {
		t.Fatalf("globex: %d", a.status)
	}
	var bad atomic.Int32
	stop := r.hammer("/globex/hello", &bad)
	t0 := time.Now()
	a := r.call("/acme/bomb")
	took := time.Since(t0)
	stop()
	if a.status != 502 {
		t.Errorf("the memory bomb: %d %q, want 502", a.status, a.body)
	}
	if took > 10*time.Second {
		t.Errorf("the bomb took %v to be stopped", took)
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests of the neighbour failed", bad.Load())
	}
	if a := r.call("/acme/hello"); a.status != 200 {
		t.Errorf("the next request of the customer: %d", a.status)
	}
	if !strings.Contains(r.logs.String(), "reason=memory") {
		t.Error("the log does not say that the process was killed for memory")
	}
}

func TestAnOverrunIsA504ItKillsTheCustomersProcessAndTheNextRequestIsServedAFreshOne(t *testing.T) {
	r := newRig(t, nil, map[string]string{"REQUEST_MS": "400"})
	r.mustPut("acme", fns{"slow": jsSlow, "count": jsCount}, putOpts{})
	r.mustPut("globex", fns{"count": jsCount}, putOpts{})
	r.call("/globex/count")
	if a := r.call("/acme/count"); a.body != "1" {
		t.Fatalf("acme: %q", a.body)
	}
	if a := r.call("/acme/slow?ms=5000"); a.status != 504 {
		t.Fatalf("an overrun: %d, want 504", a.status)
	}
	if a := r.call("/acme/count"); a.status != 200 || a.body != "1" { // the process was killed: a counter that starts again
		t.Fatalf("the next request: %d %q (a fresh process)", a.status, a.body)
	}
	if a := r.call("/globex/count"); a.body != "2" { // the neighbour's process is the same one
		t.Fatalf("the neighbour: %q", a.body)
	}
}

func TestAnIdleCustomerIsStoppedAndStartsAgainAndNothingOfItsUserIsLeft(t *testing.T) {
	r := newRig(t, map[string]string{"IDLE_MS": "800"}, nil)
	r.mustPut("acme", fns{"count": jsCount}, putOpts{})
	if a := r.call("/acme/count"); a.body != "1" {
		t.Fatalf("first: %q", a.body)
	}
	uid := r.uidOf("acme")
	// Something left behind by a function that got out of workerd (it cannot, but the spawner does not rely on that): it goes with the last process.
	stray, err := startStray(uid)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the idle process is stopped", func() bool { return len(workerds(uid)) == 0 })
	eventually(t, "what was left of the customer's user is killed", func() bool { return countOf(uid) == 0 })
	stray.Wait()
	if a := r.call("/acme/count"); a.status != 200 || a.body != "1" {
		t.Fatalf("after the idle stop: %d %q", a.status, a.body)
	}
	dir, _ := os.ReadDir(filepath.Join(r.state, "run"))
	sockets, _ := os.ReadDir(r.control)
	if len(dir) != 1 {
		t.Errorf("run directories: %d, want only the running process's", len(dir))
	}
	if len(sockets) != 2 { // spawner.sock and the running process's
		t.Errorf("sockets: %d, want 2", len(sockets))
	}
}

// ---- a changed definition, a refused one, a deleted one

func TestAPutThatDoesNotLoadIsRefusedWithTheFunctionsNameAndNothingChanges(t *testing.T) {
	r := newRig(t, nil, nil)
	a := r.put("acme", fns{"hello": jsHello, "broken": jsSyntax}, putOpts{})
	var body struct {
		Error     string
		Functions map[string]string
	}
	if a.status != 422 || json.Unmarshal([]byte(a.body), &body) != nil || body.Functions["broken"] == "" || body.Functions["hello"] != "" {
		t.Fatalf("%d %s", a.status, a.body)
	}
	if _, ok := r.s.uids.uid("acme"); ok || len(r.s.List()) != 0 {
		t.Error("a refused put left a customer or a user id behind")
	}
	if a := r.put("acme", fns{"noexport": jsNoExp}, putOpts{}); a.status != 200 { // a bundle with no default export loads; it is a 500 of its request, never of the process
		t.Errorf("a module with no default export: %d %s", a.status, a.body)
	}
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	if a := r.call("/globex/hello"); a.status != 200 {
		t.Fatalf("%d", a.status)
	}
	// A refused put of an existing customer leaves the running version alone.
	if a := r.put("globex", fns{"hello": jsSyntax}, putOpts{}); a.status != 422 {
		t.Fatalf("a bad replacement: %d", a.status)
	}
	if a := r.call("/globex/hello/x"); a.status != 200 || a.body != "hello /x" {
		t.Errorf("after the refused put: %d %q", a.status, a.body)
	}
	if got := workerds(r.uidOf("globex")); len(got) != 1 {
		t.Errorf("a refused put left %d processes of globex", len(got))
	}
}

func TestANewDefinitionIsServedFromTheNextRequestAndTheOldProcessFinishesWhatItServesAndGoes(t *testing.T) {
	r := newRig(t, map[string]string{"IDLE_MS": "60000"}, map[string]string{"REQUEST_MS": "20000"})
	r.mustPut("acme", fns{"v": `export default () => new Response('v1'); `, "slow": `export default async () => { await new Promise((r) => setTimeout(r, 1500)); return new Response('slow v1'); };`}, putOpts{})
	if a := r.call("/acme/v"); a.body != "v1" {
		t.Fatal(a.body)
	}
	slow := make(chan answer, 1)
	go func() { slow <- r.call("/acme/slow") }()
	time.Sleep(300 * time.Millisecond)
	r.mustPut("acme", fns{"v": `export default () => new Response('v2'); `, "slow": `export default () => new Response('slow v2');`}, putOpts{})
	if a := r.call("/acme/v"); a.body != "v2" {
		t.Errorf("after the put: %q, want v2", a.body)
	}
	if got := workerds(r.uidOf("acme")); len(got) != 2 {
		t.Errorf("while the old one finishes there are %d processes, want 2", len(got))
	}
	if a := <-slow; a.status != 200 || a.body != "slow v1" {
		t.Errorf("the request in flight: %d %q, want the old version's answer", a.status, a.body)
	}
	eventually(t, "the old process drained and went", func() bool { return len(workerds(r.uidOf("acme"))) == 1 })
	if a := r.call("/acme/slow"); a.body != "slow v2" {
		t.Errorf("%q", a.body)
	}
}

func TestADeleteKillsEverythingOfTheCustomerAndTheUserIdIsGivenAgainWithNothingBehind(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"hello": jsHello}, putOpts{secrets: map[string]string{"API_KEY": "k"}})
	r.call("/acme/hello")
	uid := r.uidOf("acme")
	stray, err := startStray(uid) // a process of the customer's user that the spawner did not start
	if err != nil {
		t.Fatal(err)
	}
	if a := r.del("nobody"); a.status != 404 {
		t.Errorf("delete of a customer that is not there: %d", a.status)
	}
	if a := r.del("acme"); a.status != 204 {
		t.Fatalf("delete: %d %s", a.status, a.body)
	}
	stray.Wait()
	if countOf(uid) != 0 {
		t.Errorf("%d processes of the deleted customer's user are left", countOf(uid))
	}
	if a := r.call("/acme/hello"); a.status != 404 {
		t.Errorf("a deleted customer: %d", a.status)
	}
	if entries, _ := os.ReadDir(filepath.Join(r.state, "run")); len(entries) != 0 {
		t.Errorf("run directories left: %d", len(entries))
	}
	// The next customer gets the same number, and what is behind it is nothing.
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	if r.uidOf("globex") != uid {
		t.Errorf("the freed user id was not given again: %d, was %d", r.uidOf("globex"), uid)
	}
	if a := r.call("/globex/hello"); a.status != 200 {
		t.Errorf("%d", a.status)
	}
	if a := r.call("/acme/hello"); a.status != 404 {
		t.Errorf("the old name answers: %d", a.status)
	}
}

// ---- the doors

func TestTheControlAPIAnswersOnlyTheRegionalAgentOverMutualTLS(t *testing.T) {
	r := newRig(t, nil, nil)
	body := string(r.putBody(fns{"hello": jsHello}, putOpts{}))
	put := func(c *http.Client, url string) answer {
		req, _ := http.NewRequest("PUT", url+"/v1/customers/acme", strings.NewReader(body))
		return r.do(c, req)
	}
	if a := put(r.agent, r.ctlURL); a.status != 200 {
		t.Fatalf("the agent: %d %s", a.status, a.body)
	}
	r.del("acme")
	attempts := map[string]answer{
		"no client certificate":              put(r.pki.httpClient(nil, 0), r.ctlURL),
		"a certificate of another CA":        put(r.pki.httpClient([]tls.Certificate{r.pki.client(t, "regional-agent", true)}, 0), r.ctlURL),
		"another name":                       put(r.pki.httpClient([]tls.Certificate{r.pki.client(t, "someone-else", false)}, 0), r.ctlURL),
		"the name of a customer":             put(r.pki.httpClient([]tls.Certificate{r.pki.client(t, "acme", false)}, 0), r.ctlURL),
		"TLS 1.2 with the right certificate": put(r.pki.httpClient([]tls.Certificate{r.pki.client(t, "regional-agent", false)}, tls.VersionTLS12), r.ctlURL),
		"no TLS at all":                      put(&http.Client{Timeout: 5 * time.Second}, "http://"+strings.TrimPrefix(r.ctlURL, "https://")),
	}
	for what, a := range attempts {
		if a.status == 200 || a.status == 204 {
			t.Errorf("%s: %d", what, a.status)
		}
	}
	if a := attempts["another name"]; a.status != http.StatusForbidden {
		t.Errorf("a valid certificate with another name: %d, want 403", a.status)
	}
	if len(r.s.List()) != 0 {
		t.Errorf("a refused call put a customer: %v", r.s.List())
	}
	// The internal socket and the data port have no control routes.
	for _, path := range []string{"/v1/customers", "/v1/customers/acme"} {
		if a := r.call(path); a.status == 200 {
			t.Errorf("the data port answers %s: %d", path, a.status)
		}
	}
}

func TestTheInternalSocketIsForTheRoutersUserOnly(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"hello": jsHello}, putOpts{})
	sock := filepath.Join(r.control, "spawner.sock")
	// Root can reach the file (it owns it) and is refused by the user check; a customer's user cannot even get to it.
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: dialUnix(sock)}}
	if res, err := c.Post("http://spawner/v1/acquire?customer=acme&function=hello", "text/plain", nil); err == nil {
		res.Body.Close()
		t.Errorf("root was served: %d", res.StatusCode)
	}
	if out, err := asUser(r.uidOf("acme"), os.Args[0], "connect", sock); err == nil {
		t.Errorf("a customer's user connected: %s", out)
	}
	if out, err := asUser(routerUID, os.Args[0], "connect", sock); err != nil {
		t.Errorf("the router's user could not connect: %s", out)
	}
	if !strings.Contains(r.logs.String(), "another user is closed") {
		t.Error("the refusal is not in the log")
	}
}

func TestTheSpawnerHasOnlyTheFourCapabilitiesItNeeds(t *testing.T) {
	needRoot(t)
	if os.Getenv("EXPECT_MINIMAL_CAPS") == "" {
		t.Skip("set EXPECT_MINIMAL_CAPS=1 when the container has exactly CHOWN, KILL, SETGID, SETUID (scripts/test-in-docker.sh does)")
	}
	eff, err := capEff()
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1<<capChown | 1<<capKill | 1<<capSetgid | 1<<capSetuid); eff != want {
		t.Fatalf("CapEff is %x, want %x", eff, want)
	}
	// Everything the rig does (the whole suite) works with them: nothing else was needed to start processes as other users, to give sockets to a group, to kill, and to clean up.
	r := newRig(t, nil, nil)
	r.mustPut("acme", fns{"hello": jsHello}, putOpts{})
	if a := r.call("/acme/hello"); a.status != 200 {
		t.Fatalf("%d %s", a.status, a.body)
	}
	if strings.Contains(r.logs.String(), "capabilities it does not need") {
		t.Error("the spawner says it has capabilities it does not need")
	}
}

func TestTheSpawnerRefusesToStartWithoutItsCapabilitiesOrAsAnotherUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test is for an ordinary user (the capabilities check is covered by the rig as root)")
	}
	cfg, err := ConfigFromEnv(env(controlEnv), noCgroup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Errorf("an ordinary user: %v", err)
	}
}

// ---- many customers in one VM

func TestAHundredCustomersInOneVMEachSeesOnlyItsOwnIdentityAndSecretAndTheLeastRecentlyUsedAreMadeRoom(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about a minute")
	}
	// Room for about 30 processes: with 100 customers the pool has to stop the least recently used ones to start the next.
	r := newRig(t, map[string]string{"FN_BUDGET_MIB": "1500", "FN_ESTIMATE_MIB": "50", "START_MS": "30000"}, map[string]string{"REQUEST_MS": "20000", "MAX_INFLIGHT": "400"})
	const n = 100
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("c%03d", i)
		r.mustPut(ids[i], fns{"who": jsWhoami(ids[i])}, putOpts{secrets: map[string]string{"API_KEY": "secret-of-" + ids[i]}})
	}
	uids := map[int]string{}
	for _, id := range ids {
		u := r.uidOf(id)
		if other, dup := uids[u]; dup {
			t.Fatalf("%s and %s share the user id %d", id, other, u)
		}
		uids[u] = id
	}
	var wg sync.WaitGroup
	var wrong atomic.Int32
	sem := make(chan struct{}, 8)
	for round := 0; round < 2; round++ {
		for _, id := range ids {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				a := r.call("/" + id + "/who")
				var got struct {
					ID, Secret string
					Keys       []string
				}
				if a.status != 200 || json.Unmarshal([]byte(a.body), &got) != nil || got.ID != id || got.Secret != "secret-of-"+id || !slices.Equal(got.Keys, []string{"API_KEY"}) {
					t.Errorf("%s answered %d %s", id, a.status, a.body)
					wrong.Add(1)
				}
			}()
		}
		wg.Wait()
	}
	st := r.s.pool.Stats()
	t.Logf("100 customers, 2 rounds: %d resident now, %d starts, %d evictions, %d wrong answers", len(st.Resident), st.Starts, st.Evictions, wrong.Load())
	if wrong.Load() != 0 {
		t.Fatalf("%d wrong answers", wrong.Load())
	}
	if st.Evictions == 0 {
		t.Error("100 customers did not need the pool to make room: the test does not test the eviction")
	}
	// Every process runs as its own customer's user, and no two customers share a process.
	seen := map[int]bool{}
	for _, id := range ids {
		for _, pid := range workerds(r.uidOf(id)) {
			if seen[pid] {
				t.Errorf("pid %d counted twice", pid)
			}
			seen[pid] = true
		}
	}
}
