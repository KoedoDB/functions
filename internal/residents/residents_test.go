package residents

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type proc struct {
	name string
	rss  float64 // 0: cannot be measured
}

// rig is a supervisor of pretend processes: Start takes no time, Stop takes a few milliseconds, and the log says what happened.
type rig struct {
	t      *testing.T
	pool   *Pool[*proc]
	mu     sync.Mutex
	log    []string
	live   map[*proc]bool
	peak   int
	offset atomic.Int64 // added to the real time: a test moves the time forward, and a wait still ends
	stopMs time.Duration
}

func newRig(t *testing.T, o Options) *rig {
	r := &rig{t: t, live: map[*proc]bool{}, stopMs: 2 * time.Millisecond}
	o.Now = func() time.Time { return time.Now().Add(time.Duration(r.offset.Load())) }
	if o.EstimateMiB == 0 {
		o.EstimateMiB = 105
	}
	r.pool = New(Hooks[*proc]{
		Start: func(name string) (*proc, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			p := &proc{name: name}
			r.live[p] = true
			r.peak = max(r.peak, len(r.live))
			r.log = append(r.log, "start "+name)
			return p, nil
		},
		Stop: func(p *proc) {
			r.mu.Lock()
			r.log = append(r.log, "stop "+p.name)
			r.mu.Unlock()
			time.Sleep(r.stopMs)
			r.mu.Lock()
			delete(r.live, p)
			r.mu.Unlock()
		},
		RSS: func(p *proc) (float64, bool) { r.mu.Lock(); defer r.mu.Unlock(); return p.rss, p.rss > 0 },
	}, o)
	return r
}

func (r *rig) logs() []string          { r.mu.Lock(); defer r.mu.Unlock(); return slices.Clone(r.log) }
func (r *rig) liveCount() int          { r.mu.Lock(); defer r.mu.Unlock(); return len(r.live) }
func (r *rig) peakLive() int           { r.mu.Lock(); defer r.mu.Unlock(); return r.peak }
func (r *rig) advance(d time.Duration) { r.offset.Add(int64(d)) }

func (r *rig) acquire(name string) *Lease[*proc] {
	r.t.Helper()
	l, err := r.pool.Acquire(context.Background(), name)
	if err != nil {
		r.t.Fatalf("acquire %s: %v", name, err)
	}
	return l
}

func opts(budget float64) Options {
	return Options{BudgetMiB: budget, MinResidency: 0, Wait: 300 * time.Millisecond, MaxWaiters: 20}
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func starts(log []string) []string {
	var s []string
	for _, l := range log {
		if len(l) > 5 && l[:5] == "start" {
			s = append(s, l)
		}
	}
	return s
}

func TestAFunctionThatFitsIsStartedAndTheNextRequestUsesTheSameProcess(t *testing.T) {
	r := newRig(t, opts(300))
	a := r.acquire("a")
	a.Release()
	again := r.acquire("a")
	again.Release()
	equal(t, r.logs(), []string{"start a"})
	if a.Handle != again.Handle {
		t.Fatal("not the same process")
	}
}

func TestOverTheBudgetTheLeastRecentlyUsedIdleFunctionIsStoppedForRoom(t *testing.T) {
	r := newRig(t, opts(330)) // 3 processes of 105
	for _, n := range []string{"a", "b", "c"} {
		l := r.acquire(n)
		r.advance(10 * time.Millisecond)
		l.Release()
	}
	a2 := r.acquire("a") // a is now the most recent; b is the least
	r.advance(10 * time.Millisecond)
	a2.Release()
	r.acquire("d").Release()
	equal(t, r.logs(), []string{"start a", "start b", "start c", "stop b", "start d"})
	got := r.pool.Stats().Resident
	slices.Sort(got)
	equal(t, got, []string{"a", "c", "d"})
	if r.pool.Stats().Evictions != 1 {
		t.Fatal("evictions")
	}
}

func TestAFunctionWithARequestInFlightIsNeverStoppedForRoomTheNewRequestWaitsAndGoesOnWhenItIsReleased(t *testing.T) {
	r := newRig(t, opts(210)) // 2 processes
	a := r.acquire("a")
	b := r.acquire("b")
	done := make(chan *Lease[*proc], 1)
	go func() { l, _ := r.pool.Acquire(context.Background(), "c"); done <- l }()
	time.Sleep(40 * time.Millisecond)
	equal(t, r.logs(), []string{"start a", "start b"}) // c waits: both are busy
	b.Release()
	c := <-done
	equal(t, r.logs(), []string{"start a", "start b", "stop b", "start c"})
	if r.pool.Stats().Waits != 1 {
		t.Fatal("waits")
	}
	a.Release()
	c.Release()
}

func TestARequestThatGetsNoRoomWithinTheWaitGetsBusyAndNothingIsStopped(t *testing.T) {
	o := opts(210)
	o.Wait = 40 * time.Millisecond
	r := newRig(t, o)
	a := r.acquire("a")
	b := r.acquire("b")
	if _, err := r.pool.Acquire(context.Background(), "c"); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	equal(t, r.logs(), []string{"start a", "start b"})
	if r.pool.Stats().Busy != 1 {
		t.Fatal("busy")
	}
	a.Release()
	b.Release()
}

func TestWhenAsManyRequestsWaitForRoomAsMaxWaitersSaysTheNextGetsBusyAtOnce(t *testing.T) {
	o := opts(110)
	o.Wait = 2 * time.Second
	o.MaxWaiters = 2
	r := newRig(t, o)
	a := r.acquire("a")
	type res struct {
		l   *Lease[*proc]
		err error
	}
	get := func(name string) chan res {
		ch := make(chan res, 1)
		go func() { l, err := r.pool.Acquire(context.Background(), name); ch <- res{l, err} }()
		return ch
	}
	b := get("b") // waits: a is busy
	time.Sleep(20 * time.Millisecond)
	c := get("c") // waits behind b: those behind the first count too
	time.Sleep(20 * time.Millisecond)
	if _, err := r.pool.Acquire(context.Background(), "d"); !errors.Is(err, ErrBusy) { // a third waiter: no
		t.Fatalf("want ErrBusy, got %v", err)
	}
	if r.pool.Stats().Busy != 1 {
		t.Fatal("busy")
	}
	a.Release()
	rb := <-b
	rb.l.Release()
	rc := <-c
	rc.l.Release()
	equal(t, starts(r.logs()), []string{"start a", "start b", "start c"})
}

func TestARequestThatWaitsBehindAnotherIsGivenUpAtItsOwnTimeCountedFromWhenItCame(t *testing.T) {
	o := opts(110)
	o.Wait = 80 * time.Millisecond
	o.MaxWaiters = 5
	r := newRig(t, o)
	a := r.acquire("a")
	t0 := time.Now()
	errs := make(chan error, 2)
	go func() { _, err := r.pool.Acquire(context.Background(), "b"); errs <- err }()
	time.Sleep(30 * time.Millisecond)
	go func() { _, err := r.pool.Acquire(context.Background(), "c"); errs <- err }()
	for i := 0; i < 2; i++ {
		if err := <-errs; !errors.Is(err, ErrBusy) {
			t.Fatalf("want ErrBusy, got %v", err)
		}
	}
	if took := time.Since(t0); took > 300*time.Millisecond {
		t.Fatalf("both were given up within about the wait (80 ms from b, 80 ms from c), took %v", took)
	}
	a.Release()
}

func TestRequestsThatComeTogetherForAFunctionThatIsNotResidentStartItOnce(t *testing.T) {
	r := newRig(t, opts(300))
	var wg sync.WaitGroup
	leases := make([]*Lease[*proc], 5)
	for i := range leases {
		wg.Add(1)
		go func() { defer wg.Done(); leases[i], _ = r.pool.Acquire(context.Background(), "a") }()
	}
	wg.Wait()
	equal(t, r.logs(), []string{"start a"})
	if r.pool.Stats().Starts != 1 {
		t.Fatal("starts")
	}
	for _, l := range leases {
		l.Release()
	}
}

func TestManyRequestsForTheSameFunctionThatWaitsForRoomCountAsOneWaiter(t *testing.T) {
	o := opts(110)
	o.Wait = 2 * time.Second
	o.MaxWaiters = 1
	r := newRig(t, o)
	a := r.acquire("a")
	var wg sync.WaitGroup
	leases := make([]*Lease[*proc], 6)
	errs := make([]error, 6)
	run := func(i int) { defer wg.Done(); leases[i], errs[i] = r.pool.Acquire(context.Background(), "b") }
	wg.Add(1)
	go run(0) // waits: a is busy
	time.Sleep(20 * time.Millisecond)
	for i := 1; i < 6; i++ { // five more for the same function: they join it, none is Busy
		wg.Add(1)
		go run(i)
	}
	time.Sleep(30 * time.Millisecond)
	if r.pool.Stats().Busy != 0 {
		t.Fatal("a request for a function that already waits must join it")
	}
	a.Release()
	wg.Wait()
	for i, l := range leases {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		l.Release()
	}
	equal(t, starts(r.logs()), []string{"start a", "start b"})
}

func TestAProcessYoungerThanMinResidencyIsNotStoppedForRoom(t *testing.T) {
	o := opts(110)
	o.MinResidency = 5 * time.Second
	o.Wait = 40 * time.Millisecond
	r := newRig(t, o)
	r.acquire("a").Release()
	if _, err := r.pool.Acquire(context.Background(), "b"); !errors.Is(err, ErrBusy) { // a is idle, but new
		t.Fatalf("want ErrBusy, got %v", err)
	}
	equal(t, r.logs(), []string{"start a"})
	r.advance(6 * time.Second)
	r.acquire("b").Release()
	equal(t, r.logs(), []string{"start a", "stop a", "start b"})
}

func TestAProcessThatExitedFreesItsRoom(t *testing.T) {
	o := opts(110)
	o.Wait = 2 * time.Second
	r := newRig(t, o)
	a := r.acquire("a")
	done := make(chan *Lease[*proc], 1)
	go func() { l, _ := r.pool.Acquire(context.Background(), "b"); done <- l }()
	time.Sleep(30 * time.Millisecond)
	equal(t, r.logs(), []string{"start a"})
	r.pool.Exited("a", a.Handle) // it died while a request was in flight
	a.Release()
	(<-done).Release()
	equal(t, r.logs(), []string{"start a", "start b"})
}

func TestAnExitOfAnOlderProcessLeavesTheNewerOneAlone(t *testing.T) {
	r := newRig(t, opts(300))
	a := r.acquire("a")
	a.Release()
	r.pool.Exited("a", &proc{name: "a"}) // some other handle
	if len(r.pool.Stats().Resident) != 1 {
		t.Fatal("the resident process must stay")
	}
}

func TestOneProcessCanAlwaysRunEvenWhenItIsMoreThanTheBudget(t *testing.T) {
	r := newRig(t, opts(50))
	r.acquire("a").Release()
	equal(t, r.logs(), []string{"start a"})
}

func TestWhatAProcessReallyUsesCountsNotOnlyTheEstimate(t *testing.T) {
	r := newRig(t, opts(300))
	a := r.acquire("a")
	r.mu.Lock()
	a.Handle.rss = 250 // a grew
	r.mu.Unlock()
	a.Release()
	r.acquire("b").Release() // 250 + 105 > 300: a goes
	equal(t, r.logs(), []string{"start a", "stop a", "start b"})
}

func TestAProcessThatMeasuresLessThanTheEstimateStillCountsAsTheEstimate(t *testing.T) {
	// A process just started is small and grows under load: counting it as it is now would let in more than the budget holds.
	r := newRig(t, opts(330)) // 3 processes of 105
	for _, n := range []string{"a", "b", "c"} {
		l := r.acquire(n)
		r.mu.Lock()
		l.Handle.rss = 30 // just started
		r.mu.Unlock()
		l.Release()
	}
	r.acquire("d").Release() // 3 x 105 + 105 > 330: one goes, although the three measure 90 together
	if r.liveCount() > 3 {
		t.Fatalf("more than 3 live: %d", r.liveCount())
	}
	if r.pool.Stats().Evictions != 1 {
		t.Fatalf("evictions %d", r.pool.Stats().Evictions)
	}
}

func TestEveryRequestThatWaitedForAStartThatFailedBusyIsCounted(t *testing.T) {
	o := opts(110)
	o.Wait = 60 * time.Millisecond
	o.MaxWaiters = 10
	r := newRig(t, o)
	a := r.acquire("a")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	run := func() { defer wg.Done(); _, err := r.pool.Acquire(context.Background(), "b"); errs <- err }
	wg.Add(1)
	go run()
	time.Sleep(15 * time.Millisecond)
	for i := 0; i < 4; i++ { // four more join the first
		wg.Add(1)
		go run()
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("want ErrBusy, got %v", err)
		}
		n++
	}
	if n != 5 || r.pool.Stats().Busy != 5 {
		t.Fatalf("5 requests got Busy, the count says %d (%d errors)", r.pool.Stats().Busy, n)
	}
	a.Release()
}

func TestReapIdleStopsWhatHasBeenIdleButNotAFunctionWithWorkLeftNorOneInFlight(t *testing.T) {
	r := newRig(t, opts(1000))
	busy := r.acquire("busy")
	r.acquire("idle").Release()
	r.acquire("bg").Release()
	r.advance(31 * time.Second)
	stopped := r.pool.ReapIdle(30*time.Second, func(n string) bool { return n == "bg" })
	equal(t, stopped, []string{"idle"})
	var stops []string
	for _, l := range r.logs() {
		if len(l) > 4 && l[:4] == "stop" {
			stops = append(stops, l)
		}
	}
	equal(t, stops, []string{"stop idle"})
	busy.Release()
}

func TestThirtyFunctionsCalledInTurnInABudgetOfThreeNeverRunMoreThanThreeAndNeverFail(t *testing.T) {
	r := newRig(t, opts(330))
	for round := 0; round < 3; round++ {
		for i := 0; i < 30; i++ {
			r.acquire("f" + string(rune('A'+i%26)) + string(rune('a'+i/26))).Release()
		}
	}
	if p := r.peakLive(); p > 3 {
		t.Fatalf("at most 3 at the same time, saw %d", p)
	}
	s := r.pool.Stats()
	if s.Busy != 0 || s.Starts != 90 { // all cold: a function is out of the Pod by the time it is called again
		t.Fatalf("busy %d starts %d", s.Busy, s.Starts)
	}
}

func TestRequestsForManyFunctionsAtOnceEachHeldForAMomentStayWithinTheBudget(t *testing.T) {
	o := opts(330)
	o.Wait = 5 * time.Second
	o.MaxWaiters = 200
	r := newRig(t, o)
	r.stopMs = time.Millisecond
	var served atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 120; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := r.pool.Acquire(context.Background(), "f"+string(rune('a'+i%12)))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			if n := r.liveCount(); n > 3 {
				t.Errorf("live %d", n)
			}
			time.Sleep(time.Duration(3+i%4) * time.Millisecond)
			l.Release()
			served.Add(1)
		}()
	}
	wg.Wait()
	if served.Load() != 120 {
		t.Fatalf("served %d", served.Load())
	}
	if p := r.peakLive(); p > 3 {
		t.Fatalf("at most 3 at the same time, saw %d", p)
	}
}

func TestARequestWhoseContextEndsWhileItWaitsGivesUpAndLeavesNoLeaseBehind(t *testing.T) {
	o := opts(210) // two processes
	o.Wait = 2 * time.Second
	r := newRig(t, o)
	a := r.acquire("a")
	b := r.acquire("b")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := r.pool.Acquire(ctx, "c"); !errors.Is(err, context.DeadlineExceeded) { // c waits for room: a and b are busy
		t.Fatalf("want the context's error, got %v", err)
	}
	b.Release() // room for c's start, which is still in the queue: it starts for nobody
	time.Sleep(60 * time.Millisecond)
	r.pool.mu.Lock()
	e := r.pool.entries["c"]
	inflight := -1
	if e != nil {
		inflight = e.inflight
	}
	r.pool.mu.Unlock()
	if e != nil && inflight != 0 {
		t.Fatalf("c was started for a request that gave up, and holds %d leases", inflight)
	}
	a.Release()
	// every process can be stopped for room, so a new function gets its room (nothing is held for the one that gave up)
	r.acquire("d").Release()
	r.acquire("e").Release()
	if r.pool.Stats().Busy != 0 {
		t.Fatal("busy")
	}
}

func TestStopAllStopsEveryResidentProcess(t *testing.T) {
	r := newRig(t, opts(1000))
	for _, n := range []string{"a", "b", "c"} {
		r.acquire(n).Release()
	}
	r.pool.StopAll()
	if r.liveCount() != 0 || len(r.pool.Stats().Resident) != 0 {
		t.Fatal("not all stopped")
	}
}

func TestAFailedStartIsReportedAndLeavesNothingResident(t *testing.T) {
	boom := errors.New("cannot start")
	p := New(Hooks[*proc]{Start: func(string) (*proc, error) { return nil, boom }, Stop: func(*proc) {}}, Options{BudgetMiB: 300, Wait: time.Second})
	if _, err := p.Acquire(context.Background(), "a"); !errors.Is(err, boom) {
		t.Fatalf("want the start's error, got %v", err)
	}
	if len(p.Stats().Resident) != 0 {
		t.Fatal("nothing must be resident")
	}
}
