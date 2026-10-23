// Package spawner is the privileged half of the KoedoDB function supervisor for a VM that holds SEVERAL customers.
//
// It is the only process that knows the customers' bundles and secrets and the only one that starts workerd. It runs as root with four capabilities (CAP_SETUID, CAP_SETGID, CAP_CHOWN,
// CAP_KILL) and no more, and keeps the platform's side of the wall: each customer's workerd is started as a user and a group of its own (no supplementary groups, no_new_privs, no
// capabilities, a read-only and almost empty file system view, resource limits), on a listening socket the spawner made and handed down as a file descriptor, so that no customer can
// reach another customer's process, files or secrets. docs/shared-vm.md says what that does and what it does not.
//
// It has two doors, both small: the control API (HTTPS, mutual TLS, for the Regional Agent: put and delete a customer) and the internal socket (a Unix socket only the router's user can
// connect to: acquire a customer's process for a request, release it). The router (package router) is the non-root half that faces the edge.
package spawner

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the spawner's settings (README, "Settings").
type Config struct {
	Control string // the directory shared with the router (a tmpfs): the internal socket and the customers' sockets. The spawner makes it root:RouterGID, mode 0750
	State   string // the spawner's own scratch (a tmpfs only the spawner's container mounts): the run directories of the processes. Never shared
	Workerd string // the workerd executable
	Child   string // the executable that becomes workerd after dropping what it can (this binary: "<Child> child ..."); empty: the spawner's own
	Compat  string // the compatibility date of every function (the platform's, never the customer's)

	// Who may connect to the internal socket, and who owns the sockets the customers' processes listen on.
	RouterUID int
	RouterGID int

	// The control API.
	ControlListen string
	ControlCert   string
	ControlKey    string
	ControlCA     string   // the CA that signs the Regional Agent's client certificate
	ControlAllow  []string // the common names of the client certificates that may call it (at least one)

	// Users: customer i gets the user and group UIDBase+i. The range must be unused in the VM (never 0 and never the router's user).
	UIDBase  int
	UIDCount int

	IdleTimeout    time.Duration // a process with no request for this long is stopped
	StartTimeout   time.Duration // how long to wait for a process to answer
	LeaseTTL       time.Duration // a lease the router does not release is released after this (a request is at most RequestTimeout long)
	BudgetMiB      float64       // memory the processes may use together
	EstimateMiB    float64       // what a process counts as before it is measured
	MinResidency   time.Duration // see internal/residents
	Wait           time.Duration
	MaxWaiters     int
	MaxCustomers   int   // customers that may be put (bounded by UIDCount too)
	MaxFunctions   int   // functions of one customer
	MaxBundleBytes int   // one bundle
	MaxStateBytes  int64 // all the bundles and secrets of all the customers together (they are held in memory)
	MaxSecrets     int
	MaxProbes      int // throwaway workerd processes that check a put at the same time

	// Limits of a customer's process, the platform's maximum: a put may ask for less, never for more.
	MemoryMiB    float64 // a workerd process above this resident memory is killed (and the cgroup's memory.max)
	CPUQuotaPct  float64 // percent of one core (cpu.max, and what "nearly all of it" means to the runaway rule)
	PidsLimit    int     // pids.max of the cgroup, and RLIMIT_NPROC of the customer's user
	Look         time.Duration
	MemoryLook   time.Duration
	RunawayLooks int
	CgroupRoot   string // a writable cgroup v2 directory handed down to the spawner: each customer's process gets a cgroup in it. Empty: no cgroup (the looks only)
}

// ConfigFromEnv reads the settings from the environment. A value that is not a number is an error (a typo in a limit must not become the default). The settings that make the isolation
// (the users, the control API's certificates) have no "off" and no default that works without them.
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
		Control:        str("CONTROL_DIR", "/sockets"),
		State:          str("STATE_DIR", "/state"),
		Workerd:        str("WORKERD_BIN", "/sup/workerd"),
		Child:          str("CHILD_BIN", ""),
		Compat:         str("WD_COMPAT_DATE", "2026-10-01"),
		RouterUID:      int(num("ROUTER_UID", 10001)),
		RouterGID:      int(num("ROUTER_GID", 10001)),
		ControlListen:  str("CONTROL_LISTEN", ":9443"),
		ControlCert:    str("CONTROL_CERT", ""),
		ControlKey:     str("CONTROL_KEY", ""),
		ControlCA:      str("CONTROL_CLIENT_CA", ""),
		UIDBase:        int(num("UID_BASE", 20000)),
		UIDCount:       int(num("UID_COUNT", 1000)),
		IdleTimeout:    ms("IDLE_MS", 30000),
		StartTimeout:   ms("START_MS", 30000),
		LeaseTTL:       ms("LEASE_TTL_MS", 20000),
		EstimateMiB:    num("FN_ESTIMATE_MIB", 50), // an idle workerd is about 50 MiB resident (doc/WORKERD-LIMITS.md of the KoedoDB repository)
		MinResidency:   ms("FN_MIN_RESIDENCY_MS", 1000),
		Wait:           ms("FN_WAIT_MS", 5000),
		MaxWaiters:     int(num("FN_MAX_WAITERS", 20)),
		MaxCustomers:   int(num("MAX_CUSTOMERS", 1000)),
		MaxFunctions:   int(num("MAX_FUNCTIONS", 50)),
		MaxBundleBytes: int(num("MAX_BUNDLE_BYTES", 5<<20)),
		MaxStateBytes:  int64(num("MAX_STATE_BYTES", 256<<20)),
		MaxSecrets:     int(num("MAX_SECRETS", 64)),
		MaxProbes:      int(num("MAX_PROBES", 4)),
		MemoryMiB:      num("WD_MEMORY_MIB", 256),
		CPUQuotaPct:    num("WD_CPU_QUOTA_PCT", 100),
		PidsLimit:      int(num("WD_PIDS", 64)),
		Look:           ms("WD_LOOK_MS", 2000),
		MemoryLook:     ms("WD_MEMORY_LOOK_MS", 250),
		RunawayLooks:   int(num("WD_RUNAWAY_LOOKS", 5)),
		CgroupRoot:     str("WD_CGROUP_ROOT", ""),
	}
	for _, name := range strings.Split(str("CONTROL_ALLOW_CN", ""), ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.ControlAllow = append(c.ControlAllow, name)
		}
	}
	// The budget: set by FN_BUDGET_MIB, else the VM's memory limit (cgroup) less what the spawner and the router need, else a default.
	reserve := num("SUPERVISOR_RESERVE_MIB", 60)
	switch {
	case str("FN_BUDGET_MIB", "") != "":
		c.BudgetMiB = num("FN_BUDGET_MIB", 0)
	default:
		if limit, ok := cgroupMemoryMiB(); ok && limit > reserve {
			c.BudgetMiB = limit - reserve
		} else {
			c.BudgetMiB = 1024
		}
	}
	if c.MemoryMiB <= 0 || c.CPUQuotaPct <= 0 || c.Look <= 0 || c.MemoryLook <= 0 || c.RunawayLooks <= 0 || c.PidsLimit <= 0 {
		errs = append(errs, "WD_MEMORY_MIB, WD_CPU_QUOTA_PCT, WD_LOOK_MS, WD_MEMORY_LOOK_MS, WD_RUNAWAY_LOOKS and WD_PIDS must be more than 0")
	}
	if c.UIDBase < 1000 || c.UIDCount <= 0 || c.UIDBase+c.UIDCount > 4_000_000_000 {
		errs = append(errs, "UID_BASE must be 1000 or more and UID_COUNT more than 0 (a customer must never run as root or as a system user)")
	} else if c.RouterUID >= c.UIDBase && c.RouterUID < c.UIDBase+c.UIDCount || c.RouterGID >= c.UIDBase && c.RouterGID < c.UIDBase+c.UIDCount {
		errs = append(errs, "ROUTER_UID and ROUTER_GID must be outside the range of the customers' users")
	}
	if c.RouterUID <= 0 || c.RouterGID <= 0 {
		errs = append(errs, "ROUTER_UID and ROUTER_GID must be more than 0")
	}
	if c.MaxCustomers > c.UIDCount {
		c.MaxCustomers = c.UIDCount // a customer needs a user id
	}
	if c.MaxCustomers <= 0 {
		errs = append(errs, "MAX_CUSTOMERS must be more than 0")
	}
	if c.MaxFunctions <= 0 || c.MaxBundleBytes <= 0 || c.MaxStateBytes <= 0 || c.MaxSecrets <= 0 || c.MaxProbes <= 0 {
		errs = append(errs, "MAX_FUNCTIONS, MAX_BUNDLE_BYTES, MAX_STATE_BYTES, MAX_SECRETS and MAX_PROBES must be more than 0")
	}
	if c.BudgetMiB <= 0 {
		errs = append(errs, "the budget must be more than 0")
	}
	if c.ControlCert == "" || c.ControlKey == "" || c.ControlCA == "" || len(c.ControlAllow) == 0 {
		errs = append(errs, "CONTROL_CERT, CONTROL_KEY, CONTROL_CLIENT_CA and CONTROL_ALLOW_CN are all needed: the control API has no plain mode")
	}
	if c.LeaseTTL <= 0 || c.StartTimeout <= 0 {
		errs = append(errs, "LEASE_TTL_MS and START_MS must be more than 0")
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
