package spawner

import (
	"fmt"
	"sort"
	"sync"
)

// Limits of one customer's process: what the put asked for (or the platform's maximum), never more than the maximum.
type Limits struct {
	MemoryMiB   float64
	CPUQuotaPct float64
	Pids        int
}

// customer is one definition of a customer: the functions' bundles, the secrets and the limits. It is immutable once it is in the table: a put makes a new one with the next
// generation, and the processes of the old one drain and stop (they are named <customer>.<generation>).
type customer struct {
	id      string
	gen     uint64
	uid     int
	fns     map[string][]byte
	secrets map[string]string
	limits  Limits
	bytes   int64
}

// names are the functions' names, sorted.
func (c *customer) names() []string {
	out := make([]string, 0, len(c.fns))
	for n := range c.fns {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// secretNames are the secrets' names, sorted. The values stay in the map.
func (c *customer) secretNames() []string {
	out := make([]string, 0, len(c.secrets))
	for n := range c.secrets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func definitionBytes(fns map[string][]byte, secrets map[string]string) int64 {
	var n int64
	for name, b := range fns {
		n += int64(len(name) + len(b))
	}
	for k, v := range secrets {
		n += int64(len(k) + len(v))
	}
	return n
}

// uidAlloc gives each customer a user id (and the group id of the same number) of its own, from base on. A number is free again only when the customer is deleted AND nothing of it runs.
type uidAlloc struct {
	mu    sync.Mutex
	base  int
	used  []string // "" = free
	byID  map[string]int
	stuck int // numbers that are kept back because a process of theirs would not die
}

func newUIDAlloc(base, count int) *uidAlloc {
	return &uidAlloc{base: base, used: make([]string, count), byID: map[string]int{}}
}

// alloc returns the customer's number, giving one if it has none; created says that it is new.
func (a *uidAlloc) alloc(id string) (uid int, created bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n, ok := a.byID[id]; ok {
		return a.base + n, false, nil
	}
	for i, who := range a.used {
		if who == "" {
			a.used[i] = id
			a.byID[id] = i
			return a.base + i, true, nil
		}
	}
	return 0, false, fmt.Errorf("no user id left for %q (UID_COUNT=%d)", id, len(a.used))
}

// uid is the customer's number, if it has one.
func (a *uidAlloc) uid(id string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[id]
	return a.base + n, ok
}

// free gives the number back. keep says that the number must not be given again (a process of it is still there): it stays taken by "!<id>".
func (a *uidAlloc) free(id string, keep bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.byID[id]
	if !ok {
		return
	}
	delete(a.byID, id)
	if keep {
		a.used[n] = "!" + id
		a.stuck++
		return
	}
	a.used[n] = ""
}
