package spawner

// The child: what a customer's process is before it becomes workerd. The spawner starts "<this binary> child ..." as the customer's user (the kernel has switched user and group at fork,
// with no supplementary groups); the child makes the process unable to get anything back, sets the limits of the user, and executes workerd on the socket it inherited. It never runs as
// root, and it refuses to run with any capability.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS.
const prSetNoNewPrivs = 38

// ChildMain is "child [--nproc N] [--gate-fd N] <workerd> <config>": it does not return when it works (workerd takes its place).
func ChildMain(args []string) error {
	// no_new_privs and the seccomp filter belong to a thread, and execve keeps those of the thread that calls it: stay on this one until workerd takes the process over.
	runtime.LockOSThread()
	nproc := 0
	gateFD := -1
	for len(args) >= 2 && (args[0] == "--nproc" || args[0] == "--gate-fd") {
		if args[0] == "--gate-fd" {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 4 {
				return errors.New("--gate-fd needs a file descriptor number of 4 or more")
			}
			gateFD, args = n, args[2:]
			continue
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 {
			return errors.New("--nproc needs a number above 0")
		}
		nproc, args = n, args[2:]
	}
	if len(args) != 2 {
		return errors.New("usage: child [--nproc N] <workerd> <config>")
	}
	workerd, config := args[0], args[1]
	if os.Geteuid() == 0 || os.Getuid() == 0 || os.Getgid() == 0 || os.Getegid() == 0 {
		return errors.New("the child must never run as root: the spawner switches user before it starts it")
	}
	if g, err := os.Getgroups(); err != nil || len(g) != 0 {
		return fmt.Errorf("the child has supplementary groups %v (%v)", g, err)
	}
	if eff, err := capEff(); err != nil || eff != 0 {
		return fmt.Errorf("the child has capabilities (%x, %v)", eff, err)
	}
	// No way back: a setuid or file-capability binary cannot give this process more than it has, whatever it executes.
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	// The tripwire (seccomp.go): from here on, a system call that only a way out of the wall needs ends the process with SIGSYS, and the spawner says so.
	if err := installTrap(); err != nil {
		return err
	}
	lim := func(res int, v uint64) error {
		return syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v})
	}
	const rlimitNproc = 6 // RLIMIT_NPROC (not a name in package syscall for every GOOS)
	for _, l := range []struct {
		res int
		v   uint64
	}{{syscall.RLIMIT_CORE, 0}, {syscall.RLIMIT_NOFILE, 4096}, {rlimitMemlock, 0}} {
		if err := lim(l.res, l.v); err != nil {
			return err
		}
	}
	if nproc > 0 {
		if err := lim(rlimitNproc, uint64(nproc)); err != nil { // counted for the whole user: the customer's own, the user ids are not shared
			return err
		}
	}
	// The gate: the spawner puts this process in its cgroup and then closes the write end. Until then nothing of the customer's runs. A closed end with no data is the signal (end of file);
	// anything else (an error) ends the process.
	if gateFD >= 0 {
		g := os.NewFile(uintptr(gateFD), "gate")
		var one [1]byte
		if _, err := g.Read(one[:]); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("the gate: %w", err)
		}
		_ = g.Close() // workerd must not inherit it
	}
	argv := []string{"workerd", "serve", "--socket-fd", "http=3", config}
	return syscall.Exec(workerd, argv, os.Environ())
}

const rlimitMemlock = 8 // RLIMIT_MEMLOCK

// capEff is the effective capability set of this process (the CapEff line of /proc/self/status).
func capEff() (uint64, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		}
	}
	return 0, errors.New("no CapEff in /proc/self/status")
}

// The capabilities the spawner needs: to switch to a customer's user and group (SETUID, SETGID), to give a directory and a socket to a group (CHOWN), and to signal a process of another
// user (KILL). Nothing else: not DAC_OVERRIDE (the spawner reads and writes only what it owns), not SYS_ADMIN (it makes no namespace and no mount), not NET_ADMIN, not SYS_PTRACE.
const (
	capChown  = 0
	capKill   = 5
	capSetgid = 6
	capSetuid = 7
)

// checkCaps refuses to start without the capabilities it needs and says what else it has: more is not an error (the Pod's definition is the place to take it away), but it is logged.
func checkCaps(log interface{ Warn(string, ...any) }) error {
	eff, err := capEff()
	if err != nil {
		return err
	}
	var missing []string
	for name, bit := range map[string]uint{"CAP_CHOWN": capChown, "CAP_KILL": capKill, "CAP_SETGID": capSetgid, "CAP_SETUID": capSetuid} {
		if eff&(1<<bit) == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the spawner lacks capabilities: %s", strings.Join(missing, ", "))
	}
	if extra := eff &^ (1<<capChown | 1<<capKill | 1<<capSetgid | 1<<capSetuid); extra != 0 {
		log.Warn("the spawner has capabilities it does not need: take them away in the Pod's definition", "extra_cap_eff", strconv.FormatUint(extra, 16))
	}
	return nil
}
