package supervisor

// The limits of a customer's workerd process, applied from outside (workerd.go): what the supervisor looks at, the rule that decides, and the cgroup when there is one.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// sampler is what the limits are decided from.
type sampler interface {
	CPUUsec() (int64, bool)  // CPU time the process has used so far, in microseconds
	RSSMiB() (float64, bool) // resident memory, in MiB
}

// procSampler reads /proc/<pid>: it works for any process the supervisor can see, with no cgroup and no privilege.
type procSampler struct{ pid int }

// clockTicks is _SC_CLK_TCK, 100 on every Linux the images run on (the kernel's USER_HZ is a constant of the ABI).
const clockTicks = 100

func (s procSampler) CPUUsec() (int64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(s.pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseProcStat(string(b))
}

// parseProcStat is the CPU time (utime + stime) of a /proc/<pid>/stat line, in microseconds. The name of the process (field 2) is in parentheses and may hold spaces and
// parentheses: the fields after it start after the last ')'.
func parseProcStat(str string) (int64, bool) {
	i := strings.LastIndexByte(str, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(str[i+1:])
	if len(f) < 13 { // state is f[0] (field 3); utime is field 14 = f[11], stime field 15 = f[12]
		return 0, false
	}
	u, err1 := strconv.ParseInt(f[11], 10, 64)
	st, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return (u + st) * 1_000_000 / clockTicks, true
}

func (s procSampler) RSSMiB() (float64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(s.pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			if f := strings.Fields(rest); len(f) >= 1 {
				if kb, err := strconv.ParseFloat(f[0], 64); err == nil {
					return kb / 1024, true
				}
			}
		}
	}
	return 0, false
}

// cgroupSampler reads the cgroup of the process (cpu.stat, memory.current): it counts the threads and every process of the customer, and does not miss a process that is gone.
type cgroupSampler struct{ dir string }

func (s cgroupSampler) CPUUsec() (int64, bool) {
	b, err := os.ReadFile(filepath.Join(s.dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "usage_usec "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func (s cgroupSampler) RSSMiB() (float64, bool) {
	b, err := os.ReadFile(filepath.Join(s.dir, "memory.current"))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return n / 1024 / 1024, true
}

// limits is the rule: resident memory above MemoryMiB, or nearly all of the CPU quota for RunawayLooks looks in a row.
type limits struct {
	MemoryMiB    float64
	CPUQuotaPct  float64
	Look         time.Duration
	RunawayLooks int
}

type watchState struct {
	haveLast bool
	lastCPU  int64
	busy     int
}

// busyUsec is how much CPU in one look counts as "nearly all of the quota": 85% of it.
func (l limits) busyUsec() float64 {
	return 0.85 * (l.CPUQuotaPct / 100) * float64(l.Look.Microseconds())
}

// check is one look: cpuUsec is the CPU time used so far (negative: not known), rssMiB the resident memory. It returns why the process is to be killed ("memory", "runaway"), or "".
func (l limits) check(st *watchState, cpuUsec int64, rssMiB float64) string {
	if rssMiB > l.MemoryMiB {
		return "memory"
	}
	if cpuUsec >= 0 {
		if st.haveLast {
			if float64(cpuUsec-st.lastCPU) >= l.busyUsec() {
				st.busy++
			} else {
				st.busy = 0
			}
		}
		st.lastCPU, st.haveLast = cpuUsec, true
		if st.busy >= l.RunawayLooks {
			return "runaway"
		}
	}
	return ""
}

// cgroup is the cgroup v2 directory of one customer's process, below the directory the supervisor was given (WD_CGROUP_ROOT).
type cgroup struct{ dir string }

// cgroup2Magic is CGROUP2_SUPER_MAGIC (statfs).
const cgroup2Magic = 0x63677270

// isCgroup2 says whether dir is on the cgroup v2 file system (a test directory is not).
func isCgroup2(dir string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(dir, &st) == nil && int64(st.Type) == cgroup2Magic
}

// newCgroup makes the cgroup of a customer and sets its limits: memory.max (and no swap), cpu.max (quotaPct of one core in a period of 100 ms) and pids.max. A file that is not
// there (a controller that is not enabled in the parent) is an error for memory.max, cpu.max and pids.max: a limit that is not applied must not look as if it were.
func newCgroup(root, customer string, memoryMiB, quotaPct float64, pids int) (*cgroup, error) {
	if !nameRE.MatchString(customer) {
		return nil, fmt.Errorf("%q is not a customer name", customer)
	}
	dir := filepath.Join(root, "wd-"+customer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &cgroup{dir: dir}
	const period = 100000
	writes := []struct {
		file, value string
		optional    bool
	}{
		{"memory.max", strconv.FormatInt(int64(memoryMiB*1024*1024), 10), false},
		{"memory.swap.max", "0", true},
		{"cpu.max", fmt.Sprintf("%d %d", int64(quotaPct/100*period), period), false},
		{"pids.max", strconv.Itoa(pids), false},
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.file), []byte(w.value), 0o644); err != nil {
			if w.optional && os.IsNotExist(err) {
				continue
			}
			c.remove()
			return nil, fmt.Errorf("%s: %w", w.file, err)
		}
	}
	return c, nil
}

// remove removes the directory (the process is gone).
func (c *cgroup) remove() { _ = os.Remove(c.dir) }

// readSecrets reads a customer's secrets.json: an object of names and string values. A file that is not there is no secret; a name that is not a variable name is an error.
func readSecrets(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for k := range m {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("%s: %q is not a variable name", path, k)
		}
	}
	return m, nil
}
