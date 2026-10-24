package spawner

// The per-function limits of the patched workerd (workerd/patches, docs/limits.md), seen through the whole stack: a put with limits, the configuration the spawner writes, a real patched
// workerd as the customer's user. Each test makes ONE function misbehave and checks that only that function is stopped: its answer, the process still being the same process (nothing was
// killed from outside), another function of the same customer, and another customer. They need the patched binary (WORKERD_BIN): an unmodified workerd refuses the configuration, and the
// put answers 422.

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	jsCPULoop   = `export default () => { for (;;) {} };`
	jsMicroLoop = `export default async () => { for (;;) { await Promise.resolve(); } };`
	// a timer that never fires in time (a promise nothing can resolve is another thing: workerd cancels that request by itself at once)
	jsHang = `export default () => new Promise((r) => setTimeout(r, 120000));`
	// objects on the JavaScript heap, not buffers
	jsHeapBomb = `export default () => { const keep = []; for (;;) keep.push({ a: 1, b: 'x' + keep.length }); };`
	jsReDoS    = `export default () => new Response(String(/(a+)+$/.test('a'.repeat(60) + 'b')));`
	jsSortBig  = `export default () => { const a = Array.from({ length: 3e7 }, (_, i) => (i * 7919) % 1000003); a.sort((p, q) => p - q); return new Response('sorted'); };`
	// Loops TurboFan compiles without a check for V8's interrupts: TerminateExecution has no effect on them (found on 2026-10-24 with 1.20261023.1 + our patch; with --no-turbofan, the default
	// of the supervisor, the same loops are stopped at the limit). The tests that need them uninterrupted run with WD_V8_FLAGS=none.
	jsUninterruptible     = `export default () => { for (;;) new Array(1000).fill(0); };`
	jsUninterruptibleHeap = `export default () => { const keep = []; for (;;) keep.push(new Array(1000).fill(0)); };`
	// ArrayBuffers: the answer is a RangeError the function can catch
	jsBufBomb = `export default () => { const keep = []; try { for (;;) { const a = new Uint8Array(8 << 20); a.fill(1); keep.push(a); } } catch (e) { return new Response('caught ' + e.name + ' after ' + keep.length * 8 + ' MiB', { status: 507 }); } };`
	// sequential fetches of a refused address: each one is a subrequest, counted before it connects
	jsSubFlood  = `export default async () => { let refused = 0, limited = 0, other = 0; for (let i = 0; i < 40; i++) { try { await fetch('http://127.0.0.1:1/'); } catch (e) { if (/Too many subrequests/.test(e.message)) limited++; else if (/refused|blocked|connect|internal/i.test(e.message)) refused++; else other++; } } return new Response('refused=' + refused + ' limited=' + limited + ' other=' + other); };`
	jsConcFlood = `export default async () => { const rs = await Promise.allSettled(Array.from({ length: 40 }, () => fetch('http://127.0.0.1:1/'))); const limited = rs.filter((r) => r.status === 'rejected' && /Too many/.test(String(r.reason && r.reason.message))).length; return new Response('limited=' + limited + ' of 40'); };`
	// WebAssembly memory: not counted by external_mb (a known gap)
	jsWasmBomb = `export default () => { const m = new WebAssembly.Memory({ initial: 256 }); const keep = [m]; for (;;) { new Uint8Array(m.buffer).fill(1); const n = new WebAssembly.Memory({ initial: 256 }); keep.push(n); } };`
	jsWasmOne  = `export default () => { const m = new WebAssembly.Memory({ initial: 2048 }); new Uint8Array(m.buffer, 0, 4096).fill(1); return new Response('wasm memory of ' + m.buffer.byteLength / 1048576 + ' MiB'); };`
	jsSockets  = `import { connect } from 'cloudflare:sockets'; export default async () => { let ok = 0, err = 0; const ss = []; for (let i = 0; i < 200; i++) { try { const s = connect('127.0.0.1:1'); ss.push(s); ok++; } catch (e) { err++; } } return new Response('connect() returned ' + ok + ', threw ' + err); };`
)

// newLimitsRig is a rig with the production looks (5 looks of 2 s: a runaway is killed from outside after about 10 s), so that the spawner's backup does not get in before the function's own
// limit, which is what these tests are about.
func newLimitsRig(t *testing.T) *rig {
	return newRig(t, map[string]string{"WD_LOOK_MS": "2000", "WD_MEMORY_MIB": "1024"}, map[string]string{"REQUEST_MS": "30000"})
}

// watch hammers two paths while a test runs and keeps the number of failures and the slowest answer of each.
type watcher struct {
	mu      sync.Mutex
	bad     atomic.Int32
	slowest time.Duration
	stop    func()
}

func (r *rig) watch(path string) *watcher {
	w := &watcher{}
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
			t0 := time.Now()
			a := r.call(path)
			d := time.Since(t0)
			if a.status != 200 {
				w.bad.Add(1)
			}
			w.mu.Lock()
			if d > w.slowest {
				w.slowest = d
			}
			w.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	w.stop = func() { close(done); wg.Wait() }
	return w
}

// misbehave puts acme (the bad function and a good one) and globex (a good one), makes the bad function misbehave while both good ones are called all the time, and checks that nothing but
// the bad function was touched. It returns the answer of the bad function and how long it took.
func (r *rig) misbehave(bad string, limits map[string]any, extra map[string]any) (answer, time.Duration, *watcher, *watcher) {
	t := r.t
	t.Helper()
	fl := map[string]map[string]any{"bad": limits}
	r.mustPut("acme", fns{"bad": bad, "good": jsHello}, putOpts{fn: fl})
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	if a := r.call("/acme/good"); a.status != 200 {
		t.Fatalf("acme/good before: %d %s", a.status, a.body)
	}
	if a := r.call("/globex/hello"); a.status != 200 {
		t.Fatalf("globex before: %d", a.status)
	}
	pid := r.onlyWorkerd("acme")
	same := r.watch("/acme/good")
	other := r.watch("/globex/hello")
	t0 := time.Now()
	a := r.call("/acme/bad")
	took := time.Since(t0)
	same.stop()
	other.stop()
	if got := r.onlyWorkerd("acme"); got != pid {
		t.Errorf("the process of acme was replaced (pid %d, now %d): the function was stopped from outside, not by its own limit", pid, got)
	}
	if n := same.bad.Load(); n != 0 {
		t.Errorf("%d requests of acme's other function failed", n)
	}
	if n := other.bad.Load(); n != 0 {
		t.Errorf("%d requests of the other customer failed", n)
	}
	if a := r.call("/acme/good"); a.status != 200 {
		t.Errorf("acme/good after: %d", a.status)
	}
	if a := r.call("/acme/bad"); a.status == 0 {
		t.Errorf("the bad function after: no answer")
	}
	for _, reason := range []string{"reason=runaway", "reason=memory"} {
		if strings.Contains(r.logs.String(), reason) {
			t.Errorf("the spawner killed the process (%s): the function's own limit did not do it", reason)
		}
	}
	t.Logf("bad function: %d %q in %v; acme's other function: slowest answer %v (%d failures); other customer: slowest %v (%d failures)",
		a.status, strings.TrimSpace(a.body), took.Round(time.Millisecond), same.slowest.Round(time.Millisecond), same.bad.Load(), other.slowest.Round(time.Millisecond), other.bad.Load())
	return a, took, same, other
}

func TestALoopInOneFunctionStopsAfterItsCPUTimeInMillisecondsAndOnlyThatFunctionIsStopped(t *testing.T) {
	for name, src := range map[string]string{"cpu loop": jsCPULoop, "microtask loop": jsMicroLoop, "allocating loop": jsUninterruptible, "regular expression": jsReDoS, "sort of 30 million": jsSortBig} {
		t.Run(name, func(t *testing.T) {
			r := newLimitsRig(t)
			a, took, same, other := r.misbehave(src, map[string]any{"cpu_ms": 150}, nil)
			if a.status != 500 {
				t.Errorf("%d %q, want 500", a.status, a.body)
			}
			if took > 1500*time.Millisecond {
				t.Errorf("stopped after %v, want about 150 ms", took)
			}
			// workerd runs the customer's functions on one thread: the loop holds it until it is stopped, so the other function waits for at most about that long.
			if same.slowest > 1500*time.Millisecond || other.slowest > time.Second {
				t.Errorf("the neighbours waited %v and %v", same.slowest, other.slowest)
			}
		})
	}
}

func TestTheDefaultCPUTimeStopsALoopInAboutASecondNotTenSeconds(t *testing.T) {
	r := newLimitsRig(t)
	a, took, _, _ := r.misbehave(jsCPULoop, nil, nil)
	if a.status != 500 || took > 3*time.Second {
		t.Errorf("%d in %v, want 500 in about 1 s (FN_CPU_MS)", a.status, took)
	}
}

func TestAFunctionThatHangsIsStoppedAtItsWallTime(t *testing.T) {
	r := newLimitsRig(t)
	a, took, _, _ := r.misbehave(jsHang, map[string]any{"wall_ms": 600}, nil)
	if a.status != 500 || took < 500*time.Millisecond || took > 2500*time.Millisecond {
		t.Errorf("%d in %v, want 500 after about 600 ms", a.status, took)
	}
}

func TestAHeapBombInOneFunctionIsStoppedAtItsHeapAndNothingElseIsHarmed(t *testing.T) {
	for name, src := range map[string]string{"small objects": jsHeapBomb, "arrays kept by a loop": jsUninterruptibleHeap} {
		t.Run(name, func(t *testing.T) {
			r := newLimitsRig(t)
			a, took, _, _ := r.misbehave(src, map[string]any{"heap_mb": 64}, nil)
			if a.status != 500 || took > 5*time.Second {
				t.Errorf("%d %q in %v, want 500 within seconds", a.status, a.body, took)
			}
		})
	}
}

func TestAnArrayBufferBombGetsARangeErrorAtItsExternalMemoryAndNothingElseIsHarmed(t *testing.T) {
	r := newLimitsRig(t)
	a, took, _, _ := r.misbehave(jsBufBomb, map[string]any{"external_mb": 32, "heap_mb": 256}, nil)
	if a.status != 507 || !strings.Contains(a.body, "RangeError") || took > 5*time.Second {
		t.Errorf("%d %q in %v, want the function to catch a RangeError", a.status, a.body, took)
	}
}

// The backup: what stops a loop or a bomb that the function's own limits do not. It is the spawner's looks (and, in a Pod with a delegated cgroup, the kernel) around the customer's whole
// process: the customer's other functions go with it, other customers do not feel it.
func TestWhatTheFunctionLimitsCannotStopIsStoppedByTheOuterWallAndOnlyTheOwnersProcessGoes(t *testing.T) {
	for name, src := range map[string]string{"an allocating loop TurboFan does not let V8 interrupt (WD_V8_FLAGS=none)": jsUninterruptible, "the same with the memory kept (WD_V8_FLAGS=none)": jsUninterruptibleHeap} {
		t.Run(name, func(t *testing.T) {
			if testing.Short() {
				t.Skip("takes about ten seconds")
			}
			r := newRig(t, map[string]string{"WD_LOOK_MS": "2000", "WD_MEMORY_MIB": "1024", "WD_V8_FLAGS": "none"}, map[string]string{"REQUEST_MS": "30000"})
			r.mustPut("acme", fns{"bad": src, "good": jsHello}, putOpts{fn: map[string]map[string]any{"bad": {"cpu_ms": 200, "heap_mb": 64}}})
			r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
			r.call("/acme/good")
			var bad atomic.Int32
			stop := r.hammer("/globex/hello", &bad)
			t0 := time.Now()
			a := r.call("/acme/bad")
			took := time.Since(t0)
			stop()
			t.Logf("%d %q after %v; log: %v", a.status, strings.TrimSpace(a.body), took.Round(time.Millisecond), strings.Contains(r.logs.String(), "a customer's process is killed"))
			if a.status != 502 || took > 20*time.Second || !strings.Contains(r.logs.String(), "a customer's process is killed") {
				t.Errorf("the outer wall did not stop it: %d after %v", a.status, took)
			}
			if bad.Load() != 0 {
				t.Errorf("%d requests of the other customer failed", bad.Load())
			}
			if a := r.call("/acme/good"); a.status != 200 {
				t.Errorf("acme (a fresh process) after: %d", a.status)
			}
		})
	}
}

func TestASubrequestFloodIsCutAtTheLimitsOfTheFunction(t *testing.T) {
	r := newLimitsRig(t)
	a, _, _, _ := r.misbehave(jsSubFlood, map[string]any{"subrequests": 5}, nil)
	if !strings.Contains(a.body, "limited=35") {
		t.Errorf("%d %q: 5 of 40 fetches are allowed, so 35 must be refused by the limit", a.status, a.body)
	}
}

func TestAFloodOfSubrequestsOpenAtOnceIsCutAtTheLimitOfTheFunction(t *testing.T) {
	r := newLimitsRig(t)
	b, _, _, _ := r.misbehave(jsConcFlood, map[string]any{"concurrent_subrequests": 4}, nil)
	if strings.Contains(b.body, "limited=0 ") || !strings.Contains(b.body, "limited=") {
		t.Errorf("%d %q: at most 4 fetches may be open at once", b.status, b.body)
	}
}

// What the patch does NOT cover. These tests state the facts and fail only if the outer wall (the spawner's looks and, in a Pod, the cgroup) stops holding.
func TestWebAssemblyMemoryIsNotCountedByTheFunctionLimitButABombIsStoppedAnyway(t *testing.T) {
	r := newRig(t, nil, map[string]string{"REQUEST_MS": "20000"})
	r.mustPut("acme", fns{"one": jsWasmOne, "bomb": jsWasmBomb, "good": jsHello}, putOpts{fn: map[string]map[string]any{"one": {"external_mb": 32}, "bomb": {"external_mb": 32}}, limits: map[string]any{"memory_mib": 256}})
	r.mustPut("globex", fns{"hello": jsHello}, putOpts{})
	a := r.call("/acme/one")
	t.Logf("a 128 MiB WebAssembly.Memory with external_mb=32: %d %q", a.status, a.body)
	var bad atomic.Int32
	stop := r.hammer("/globex/hello", &bad)
	t0 := time.Now()
	b := r.call("/acme/bomb")
	took := time.Since(t0)
	stop()
	t.Logf("a WebAssembly memory bomb: %d %q after %v; killed by the outer wall (the spawner's looks) = %v", b.status, strings.TrimSpace(b.body), took.Round(time.Millisecond), strings.Contains(r.logs.String(), "a customer's process is killed"))
	// Stopped by the function's own cpu_ms (500: filling memory is a busy loop too), or by the outer wall (502), whichever comes first; what matters is that it is stopped and the others are not hurt.
	if b.status != 500 && b.status != 502 || took > 15*time.Second {
		t.Errorf("the Wasm bomb was not stopped: %d after %v", b.status, took)
	}
	if bad.Load() != 0 {
		t.Errorf("%d requests of the other customer failed", bad.Load())
	}
	if a := r.call("/acme/good"); a.status != 200 {
		t.Errorf("acme after: %d", a.status)
	}
}

func TestRawSocketsAreCountedByTheConcurrentSubrequestLimitOfTheFunction(t *testing.T) {
	// connect() of cloudflare:sockets went through the same counter as fetch in this build (checked here; the source suggested it might not). A control with a high limit shows the
	// refusals come from the limit and not from the destination being refused.
	r := newLimitsRig(t)
	r.mustPut("acme", fns{"low": jsSockets, "high": jsSockets, "good": jsHello}, putOpts{fn: map[string]map[string]any{
		"low": {"subrequests": 1000, "concurrent_subrequests": 4}, "high": {"subrequests": 1000, "concurrent_subrequests": 256}}})
	lo, hi := r.call("/acme/low"), r.call("/acme/high")
	t.Logf("200 connect() calls with concurrent_subrequests=4: %q; with 256: %q", strings.TrimSpace(lo.body), strings.TrimSpace(hi.body))
	if !strings.Contains(lo.body, "returned 4,") || !strings.Contains(hi.body, "returned 200,") {
		t.Errorf("connect() is not cut at the concurrent limit: %q, control %q", lo.body, hi.body)
	}
	if g := r.call("/acme/good"); g.status != 200 {
		t.Errorf("acme/good after: %d", g.status)
	}
}
