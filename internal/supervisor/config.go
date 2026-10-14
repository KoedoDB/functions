// Package supervisor is the KoedoDB function supervisor: it routes /<name>/... to the function in <FUNCTIONS_DIR>/<name>/index.mjs, which runs in a process of its own (Node, started on the
// function's first request, stopped when idle or to make room: internal/residents), and answers for it.
//
// It is trusted platform code; the functions are not. A function process gets a clean environment, permission (Node's --permission) to read its own directory and the runner and to use
// /tmp and its own scratch directory, no child process, no worker, no native addon; and a heap limit. The supervisor is written in Go so that it takes about 10 MiB of the Pod's memory,
// not the 50 MiB a Node supervisor takes.
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
	Runner    string // runner.mjs
	Node      string // the node executable
	Listen    string // address, ":9000"

	IdleTimeout    time.Duration // a process with no request for this long is stopped
	RequestTimeout time.Duration // a request that takes longer is cancelled (504) and its process is killed
	StartTimeout   time.Duration // how long to wait for a process to listen
	HeapMB         int           // V8 old-space limit of each process; 0: leave it to Node
	MaxConcurrent  int           // requests being served by one function at once; over it: 429
	EnvAllow       []string      // the variables a function may read (the Project's secrets); nothing else is passed on

	BudgetMiB    float64       // memory the processes may use together
	EstimateMiB  float64       // what a process counts as before it is measured
	MinResidency time.Duration // see internal/residents
	Wait         time.Duration
	MaxWaiters   int

	Getenv func(string) string // where the allowed variables come from (os.Getenv)
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
		Runner:         str("RUNNER", "/sup/runner.mjs"),
		Node:           str("NODE_BIN", "node"),
		Listen:         str("LISTEN", ":9000"),
		IdleTimeout:    ms("IDLE_MS", 30000),
		RequestTimeout: ms("REQUEST_MS", 10000),
		StartTimeout:   ms("START_MS", 30000),
		HeapMB:         int(num("HEAP_MB", 144)), // 3/8 of a Pod of 384 MiB
		MaxConcurrent:  int(num("MAX_CONCURRENT", 20)),
		EstimateMiB:    num("FN_ESTIMATE_MIB", 105),
		MinResidency:   ms("FN_MIN_RESIDENCY_MS", 1000),
		Wait:           ms("FN_WAIT_MS", 5000),
		MaxWaiters:     int(num("FN_MAX_WAITERS", 20)),
		Getenv:         os.Getenv,
	}
	for _, name := range strings.Split(str("FN_ENV_ALLOW", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.EnvAllow = append(c.EnvAllow, name)
		}
	}
	// The budget: set by FN_BUDGET_MIB, else the Pod's memory limit (cgroup) less what the supervisor needs, else a default for a Pod of 384 MiB.
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
