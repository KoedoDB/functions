// Package supervisor is the KoedoDB function supervisor: it routes /<customer>/<name>/... (or /<name>/... for one customer) to a function that runs in the customer's workerd process
// (unmodified workerd: one process for each customer, all of the customer's functions in it, started on the first request, stopped when idle or to make room: internal/residents), and
// answers for it.
//
// It is trusted platform code; the functions are not. A customer's process gets a clean environment (the variables the functions may read are in its configuration and nowhere else), no
// core files, and limits applied from outside: memory and CPU looks, and a cgroup when there is one (workerd.go, limits.go, docs/workerd.md). The supervisor is written in Go so that it takes
// about 10 MiB of the Pod's memory.
package supervisor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the supervisor's settings (README, "Settings").
type Config struct {
	Functions string // where the bundles are: <Functions>/<name>/index.mjs
	Control   string // where the sockets and scratch directories of the processes are (a tmpfs of its own, not /tmp); the sockets paths must be short (107 bytes at most)
	Listen    string // address, ":9000"

	IdleTimeout    time.Duration // a process with no request for this long is stopped
	RequestTimeout time.Duration // a request that takes longer is cancelled (504) and its process is killed
	StartTimeout   time.Duration // how long to wait for a process to listen
	MaxConcurrent  int           // requests being served by one function at once; over it: 429
	EnvAllow       []string      // the variables a function may read (the Project's secrets); nothing else is passed on

	BudgetMiB    float64       // memory the processes may use together
	EstimateMiB  float64       // what a process counts as before it is measured
	MinResidency time.Duration // see internal/residents
	Wait         time.Duration
	MaxWaiters   int

	Getenv func(string) string // where the allowed variables come from (os.Getenv)

	// The workerd processes (workerd.go): one for each customer, all of the customer's functions in it. The limits are applied from outside.
	CustomersDir string        // when set: <CustomersDir>/<customer>/<function>/index.mjs, routes /<customer>/<function>/...; empty: one customer, "default", with the functions of Functions
	Workerd      string        // the workerd executable
	CompatDate   string        // the compatibility date of every function (the platform's, never the customer's)
	MemoryMiB    float64       // a workerd process above this resident memory is killed (and the cgroup's memory.max is this, when there is a cgroup)
	CPUQuotaPct  float64       // the CPU a process may use, in percent of one core (cpu.max, and what "nearly all of it" means to the runaway rule)
	PidsLimit    int           // pids.max of the cgroup
	Look         time.Duration // how often the CPU of a process is looked at (a look is one step of the runaway rule)
	MemoryLook   time.Duration // how often its resident memory is looked at (a memory bomb grows in a second; the cgroup, when there is one, is the exact limit)
	RunawayLooks int           // a process that used nearly all of its CPU quota for this many looks in a row is killed (an unmodified workerd has no CPU time limit)
	CgroupRoot   string        // a writable cgroup v2 directory handed down to the supervisor: each customer's process gets a cgroup in it (memory.max, cpu.max, pids.max). Empty: no cgroup
	UIDBase      int           // when more than 0: customer i runs as UIDBase+i (the supervisor must be root); 0: every process runs as the supervisor's user
	UIDCount     int
}

// ConfigFromEnv reads the settings from the environment of the supervisor. A value that is not a number is an error (a typo in a limit must not become the default).
func ConfigFromEnv(lookup func(string) (string, bool), cgroupMemoryMiB func() (float64, bool)) (Config, error) {
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
		if err != nil || f < 0 {
			errs = append(errs, fmt.Sprintf("%s=%q is not a number of 0 or more", key, v))
			return def
		}
		return f
	}
	ms := func(key string, def int) time.Duration {
		return time.Duration(num(key, float64(def))) * time.Millisecond
	}

	c := Config{
		Functions:      str("FUNCTIONS_DIR", "/f"),
		Control:        str("CONTROL_DIR", "/sockets"),
		Listen:         str("LISTEN", ":9000"),
		IdleTimeout:    ms("IDLE_MS", 30000),
		RequestTimeout: ms("REQUEST_MS", 10000),
		StartTimeout:   ms("START_MS", 30000),
		MaxConcurrent:  int(num("MAX_CONCURRENT", 20)),
		EstimateMiB:    num("FN_ESTIMATE_MIB", 50), // an idle workerd is about 50 MiB resident (doc/WORKERD-LIMITS.md of the KoedoDB repository)
		MinResidency:   ms("FN_MIN_RESIDENCY_MS", 1000),
		Wait:           ms("FN_WAIT_MS", 5000),
		MaxWaiters:     int(num("FN_MAX_WAITERS", 20)),
		Getenv:         os.Getenv,

		CustomersDir: str("CUSTOMERS_DIR", ""),
		Workerd:      str("WORKERD_BIN", "/sup/workerd"),
		CompatDate:   str("WD_COMPAT_DATE", "2026-10-01"),
		MemoryMiB:    num("WD_MEMORY_MIB", 256),
		CPUQuotaPct:  num("WD_CPU_QUOTA_PCT", 100),
		PidsLimit:    int(num("WD_PIDS", 64)),
		Look:         ms("WD_LOOK_MS", 2000),
		MemoryLook:   ms("WD_MEMORY_LOOK_MS", 250),
		RunawayLooks: int(num("WD_RUNAWAY_LOOKS", 5)),
		CgroupRoot:   str("WD_CGROUP_ROOT", ""),
		UIDBase:      int(num("WD_UID_BASE", 0)),
		UIDCount:     int(num("WD_UID_COUNT", 4096)),
	}
	for _, name := range strings.Split(str("FN_ENV_ALLOW", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.EnvAllow = append(c.EnvAllow, name)
		}
	}
	// The budget: set by FN_BUDGET_MIB, else the Pod's memory limit (cgroup) less what the supervisor needs, else a default for a Pod of 384 MiB (the processes of 6 customers).
	reserve := num("SUPERVISOR_RESERVE_MIB", 30)
	switch {
	case str("FN_BUDGET_MIB", "") != "":
		c.BudgetMiB = num("FN_BUDGET_MIB", 0)
	default:
		if limit, ok := cgroupMemoryMiB(); ok && limit > reserve {
			c.BudgetMiB = limit - reserve
		} else {
			c.BudgetMiB = 330
		}
	}
	// RUNTIME is not a setting any more: workerd is the only runtime. An old Pod's other value is refused rather than ignored.
	if v := str("RUNTIME", ""); v != "" && v != "workerd" {
		errs = append(errs, fmt.Sprintf("RUNTIME=%q: workerd is the only runtime (leave RUNTIME out)", v))
	}
	if c.MemoryMiB <= 0 || c.CPUQuotaPct <= 0 || c.Look <= 0 || c.MemoryLook <= 0 || c.RunawayLooks <= 0 || c.PidsLimit <= 0 {
		errs = append(errs, "WD_MEMORY_MIB, WD_CPU_QUOTA_PCT, WD_LOOK_MS, WD_MEMORY_LOOK_MS, WD_RUNAWAY_LOOKS and WD_PIDS must be more than 0")
	}
	if c.UIDBase < 0 || (c.UIDBase > 0 && c.UIDCount <= 0) {
		errs = append(errs, "WD_UID_BASE must be 0 or more and WD_UID_COUNT more than 0")
	}
	if c.BudgetMiB <= 0 {
		errs = append(errs, "the budget must be more than 0")
	}
	if c.MaxConcurrent <= 0 {
		errs = append(errs, "MAX_CONCURRENT must be more than 0")
	}
	if len(errs) > 0 {
		return c, fmt.Errorf("settings: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

// CgroupMemoryMiB is the memory limit of the container (cgroup v2), if there is one.
func CgroupMemoryMiB() (float64, bool) {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n / 1024 / 1024, true
}
