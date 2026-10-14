// Package residents keeps the function processes that are resident in a Pod within the memory the Pod has.
//
// A Pod's memory is used by the function processes running at one moment, not by the functions the Project has: the supervisor starts a function's process on its first request and
// stops it when it has been idle. A page that calls ten different functions at once would start ten processes (about 105 MiB each) in a Pod of 384 MiB, and the kernel would kill one.
// Here the number of resident processes is bounded by a budget, and the least recently used one that has no request in flight is stopped to make room (an LRU). A request that cannot
// get room waits a little and then gets ErrBusy (the supervisor answers 503 with Retry-After): it is slower, and nothing is killed.
//
// The package knows nothing of how a process is started, stopped or measured: the supervisor gives it three functions (Hooks).
//
// The rules:
//   - The budget is in MiB. A resident process counts as the larger of its measured memory (Hooks.RSS) and the estimate: a process that has just started is small and grows under load
//     (a bare Node process is 57 MiB and 105 MiB after a load, since V8 collects garbage only at its heap limit), so what it uses now is not what it will use; one that has grown past
//     the estimate counts as it is. One process can always run, even when the estimate is more than the budget (otherwise nothing could run).
//   - A process with a request in flight is never stopped for room. A process younger than MinResidency is not either (two functions would stop each other in turn).
//   - Requests that come together for a function that is not resident start it once, and count as one waiter.
//   - Room is made one request at a time, in the order the requests came, so that two requests cannot both take the last room.
//   - A request waits at most Wait, counted from its arrival, and at most MaxWaiters requests wait at once (those behind the first count too): the next gets ErrBusy at once.
package residents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrBusy is what a request gets when there is no room for its function and none can be made within the wait (use errors.Is).
var ErrBusy = errors.New("residents: the Pod is busy")

func busy(reason string) error { return fmt.Errorf("%w: %s", ErrBusy, reason) }

// Hooks are the three things the pool asks of the supervisor. H is the handle of a process (a pointer to the process, for example).
type Hooks[H comparable] struct {
	// Start starts the process of a function and returns its handle at once; the process may not be listening yet.
	Start func(name string) (H, error)
	// Stop stops the process: gently first (SIGTERM), then for certain (SIGKILL). It returns when the process is gone.
	Stop func(h H)
	// RSS is the memory the process uses now, in MiB, and false when it cannot be measured (the estimate is used). It may be nil.
	RSS func(h H) (mib float64, ok bool)
}

// Options. A zero MinResidency or Wait means none; Defaults gives the values the Pod uses.
type Options struct {
	// BudgetMiB is the memory the function processes may use together: the Pod's limit less what the supervisor itself needs.
	BudgetMiB float64
	// EstimateMiB is what a process counts as at least (measured on 2026-10-10: about 105 after a load, 57 just after the start).
	EstimateMiB float64
	// MinResidency: a process is not stopped for room before it has been resident this long.
	MinResidency time.Duration
	// Wait is how long a request waits for room before it gets ErrBusy.
	Wait time.Duration
	// MaxWaiters is how many requests may wait for room at once; the next gets ErrBusy at once.
	MaxWaiters int
	// Now is the clock (nil: time.Now); the tests move it.
	Now func() time.Time
}

// Defaults are the settings of a Pod with the given budget.
func Defaults(budgetMiB float64) Options {
	return Options{BudgetMiB: budgetMiB, EstimateMiB: 105, MinResidency: time.Second, Wait: 5 * time.Second, MaxWaiters: 20}
}

type entry[H comparable] struct {
	name      string
	handle    H
	inflight  int
	lastUsed  time.Time
	startedAt time.Time
	stopping  bool
}

// call is one start of a function that several requests may wait for.
type call[H comparable] struct {
	done      chan struct{}
	finished  bool
	e         *entry[H]
	err       error
	waiters   int // requests that wait for this start; each gets a lease the moment it is done
	abandoned int // of them, those that gave up (their context ended)
}

type job[H comparable] struct {
	name     string
	c        *call[H]
	deadline time.Time
}

// Stats is what the supervisor reports (/_stats) and logs.
type Stats struct {
	Resident  []string
	Starts    int
	Evictions int
	Busy      int
	Waits     int
	UsedMiB   float64
	BudgetMiB float64
}

// Pool is the set of resident processes.
type Pool[H comparable] struct {
	hooks Hooks[H]
	o     Options

	mu          sync.Mutex
	entries     map[string]*entry[H]
	calls       map[string]*call[H]
	fifo        []job[H] // the starts that need room, in the order they came; one worker takes them in turn
	working     bool
	changed     chan struct{} // closed (and replaced) when something that a waiter looks at has changed
	queued      int           // starts that are not through (the one being served, and those behind it)
	headBlocked bool          // the one being served waits for room: those behind it wait too
	starts      int
	evictions   int
	busyCount   int
	waits       int
}

// New makes a pool. It panics on a budget that is not more than 0.
func New[H comparable](hooks Hooks[H], o Options) *Pool[H] {
	if !(o.BudgetMiB > 0) {
		panic("residents: the budget must be more than 0")
	}
	if o.EstimateMiB <= 0 {
		o.EstimateMiB = 105
	}
	if o.MaxWaiters <= 0 {
		o.MaxWaiters = 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Pool[H]{hooks: hooks, o: o, entries: map[string]*entry[H]{}, calls: map[string]*call[H]{}, changed: make(chan struct{})}
}

// Lease is a request that has its function's process. Release it when the request is over (its answer was sent to the end, or it failed): the process may then be stopped for room again.
type Lease[H comparable] struct {
	Name   string
	Handle H
	p      *Pool[H]
	e      *entry[H]
	once   sync.Once
}

// Release ends the lease; calling it again does nothing.
func (l *Lease[H]) Release() {
	l.once.Do(func() { l.p.release(l.e) })
}

func (p *Pool[H]) release(e *entry[H]) {
	p.mu.Lock()
	e.inflight--
	e.lastUsed = p.o.Now()
	p.wakeLocked() // a request waiting for room may be able to stop this one now
	p.mu.Unlock()
}

// Acquire is a request for the function name: its process is there (started, or made room for) when it returns. ctx ends the wait of this request only.
func (p *Pool[H]) Acquire(ctx context.Context, name string) (*Lease[H], error) {
	p.mu.Lock()
	if e := p.entries[name]; e != nil && !e.stopping {
		e.inflight++
		e.lastUsed = p.o.Now()
		p.mu.Unlock()
		return &Lease[H]{Name: name, Handle: e.handle, p: p, e: e}, nil
	}
	c := p.calls[name]
	if c == nil {
		// While the one in front waits for room, those behind it wait too, and they are limited: past MaxWaiters the request gets ErrBusy at once, instead of a queue that grows.
		if p.headBlocked && p.queued >= p.o.MaxWaiters {
			p.busyCount++
			p.mu.Unlock()
			return nil, busy("too many requests are waiting for room")
		}
		c = &call[H]{done: make(chan struct{})}
		p.calls[name] = c
		p.queued++
		p.fifo = append(p.fifo, job[H]{name: name, c: c, deadline: p.o.Now().Add(p.o.Wait)}) // the wait is counted from the arrival, not from the turn
		if !p.working {
			p.working = true
			go p.work()
		}
	}
	c.waiters++
	p.mu.Unlock()

	select {
	case <-c.done:
	case <-ctx.Done():
		p.mu.Lock()
		if c.finished {
			if c.err == nil { // the lease was reserved for this request: give it back
				c.e.inflight--
				c.e.lastUsed = p.o.Now()
				p.wakeLocked()
			}
		} else {
			c.abandoned++
		}
		p.mu.Unlock()
		return nil, ctx.Err()
	}
	if c.err != nil {
		return nil, c.err
	}
	return &Lease[H]{Name: name, Handle: c.e.handle, p: p, e: c.e}, nil // c.e.inflight already counts this request
}

// Exited says that the process of name is gone (it exited, or the supervisor killed it for a timeout): its memory is free again. handle is the one that exited; a newer process of
// the same function is left alone.
func (p *Pool[H]) Exited(name string, handle H) {
	p.mu.Lock()
	if e := p.entries[name]; e != nil && e.handle == handle {
		delete(p.entries, name)
		p.wakeLocked()
	}
	p.mu.Unlock()
}

// ReapIdle stops what has been idle for more than idle (the supervisor calls it every second) and returns their names. keepAlive (may be nil) says that a function has work left after
// its answer (EdgeRuntime.waitUntil) and is not idle yet.
func (p *Pool[H]) ReapIdle(idle time.Duration, keepAlive func(name string) bool) []string {
	p.mu.Lock()
	now := p.o.Now()
	var victims []*entry[H]
	for _, e := range p.entries {
		if e.inflight == 0 && !e.stopping && now.Sub(e.lastUsed) > idle && (keepAlive == nil || !keepAlive(e.name)) {
			e.stopping = true
			victims = append(victims, e)
		}
	}
	for _, e := range victims {
		delete(p.entries, e.name)
	}
	p.mu.Unlock()
	names := make([]string, 0, len(victims))
	for _, e := range victims {
		p.hooks.Stop(e.handle)
		names = append(names, e.name)
	}
	p.mu.Lock()
	p.wakeLocked()
	p.mu.Unlock()
	return names
}

// StopAll stops every resident process (the supervisor is shutting down).
func (p *Pool[H]) StopAll() {
	p.mu.Lock()
	var all []*entry[H]
	for _, e := range p.entries {
		e.stopping = true
		all = append(all, e)
	}
	p.entries = map[string]*entry[H]{}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range all {
		wg.Add(1)
		go func() { defer wg.Done(); p.hooks.Stop(e.handle) }()
	}
	wg.Wait()
	p.mu.Lock()
	p.wakeLocked()
	p.mu.Unlock()
}

// Stats reports the pool.
func (p *Pool[H]) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.entries))
	for n := range p.entries {
		names = append(names, n)
	}
	return Stats{Resident: names, Starts: p.starts, Evictions: p.evictions, Busy: p.busyCount, Waits: p.waits, UsedMiB: p.usedLocked(), BudgetMiB: p.o.BudgetMiB}
}

// ---- inside ----

// work takes the starts in the order they came, one at a time, until there are none.
func (p *Pool[H]) work() {
	for {
		p.mu.Lock()
		if len(p.fifo) == 0 {
			p.working = false
			p.mu.Unlock()
			return
		}
		j := p.fifo[0]
		p.fifo = p.fifo[1:]
		p.mu.Unlock()

		e, err := p.makeRoomAndStart(j)

		p.mu.Lock()
		j.c.e, j.c.err, j.c.finished = e, err, true
		if err == nil { // a lease for every request that waits for this start, so that the process cannot be stopped for room before they take it
			e.inflight += j.c.waiters - j.c.abandoned
			e.lastUsed = p.o.Now()
		}
		if errors.Is(err, ErrBusy) {
			p.busyCount += j.c.waiters - j.c.abandoned // every request that waited for this start gets Busy
		}
		p.queued--
		p.headBlocked = false
		delete(p.calls, j.name)
		p.mu.Unlock()
		close(j.c.done)
	}
}

func (p *Pool[H]) makeRoomAndStart(j job[H]) (*entry[H], error) {
	waited := false
	for {
		p.mu.Lock()
		if e := p.entries[j.name]; e != nil && !e.stopping {
			p.mu.Unlock()
			return e, nil // it got started some other way while this one waited its turn
		}
		// Room if the process fits, and a process can always run when none is.
		if len(p.entries) == 0 || p.usedLocked()+p.o.EstimateMiB <= p.o.BudgetMiB {
			p.mu.Unlock()
			break
		}
		if v := p.victimLocked(); v != nil {
			v.stopping = true
			if p.entries[v.name] == v {
				delete(p.entries, v.name) // it leaves the set at once: its room is counted free while it goes
			}
			p.mu.Unlock()
			p.hooks.Stop(v.handle)
			p.mu.Lock()
			p.evictions++
			p.wakeLocked()
			p.mu.Unlock()
			continue
		}
		// Everything resident is busy or too young: wait for a release, an exit, or the time to pass.
		if !waited {
			waited = true
			p.waits++
		}
		p.headBlocked = true
		left := j.deadline.Sub(p.o.Now())
		if left <= 0 {
			p.mu.Unlock()
			return nil, busy("no room for the function: the Pod is busy")
		}
		wait := min(left, p.nextMaturityLocked())
		changed := p.changed
		p.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
	h, err := p.hooks.Start(j.name)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	now := p.o.Now()
	e := &entry[H]{name: j.name, handle: h, lastUsed: now, startedAt: now}
	p.entries[j.name] = e
	p.starts++
	p.mu.Unlock()
	return e, nil
}

func (p *Pool[H]) usedLocked() float64 {
	var total float64
	for _, e := range p.entries {
		mib := p.o.EstimateMiB
		if p.hooks.RSS != nil {
			if m, ok := p.hooks.RSS(e.handle); ok && m > mib {
				mib = m
			}
		}
		total += mib
	}
	return total
}

// victimLocked is the least recently used process that has no request in flight and is not new.
func (p *Pool[H]) victimLocked() *entry[H] {
	now := p.o.Now()
	var best *entry[H]
	for _, e := range p.entries {
		if e.inflight > 0 || e.stopping || now.Sub(e.startedAt) < p.o.MinResidency {
			continue
		}
		if best == nil || e.lastUsed.Before(best.lastUsed) {
			best = e
		}
	}
	return best
}

// nextMaturityLocked is how long until the youngest idle process may be stopped (so that a waiter looks again then).
func (p *Pool[H]) nextMaturityLocked() time.Duration {
	now := p.o.Now()
	soonest := p.o.Wait
	for _, e := range p.entries {
		if e.inflight > 0 || e.stopping {
			continue
		}
		soonest = min(soonest, max(time.Millisecond, p.o.MinResidency-now.Sub(e.startedAt)))
	}
	return max(soonest, time.Millisecond)
}

func (p *Pool[H]) wakeLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}
