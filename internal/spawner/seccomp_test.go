package spawner

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

// seccompHelper is "seccomp-helper <number>" (see TestMain in rig_test.go): it sets no_new_privs, installs the tripwire the way the child does, calls the system call with that number
// (with zero arguments) and exits 0 if it comes back.
func seccompHelper(args []string) int {
	runtime.LockOSThread()
	nr, err := strconv.Atoi(args[0])
	if err != nil {
		return 2
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return 3
	}
	if err := installTrap(); err != nil {
		return 4
	}
	_, _, _ = syscall.RawSyscall(uintptr(nr), 0, 0, 0)
	return 0
}

func runHelper(t *testing.T, nr int) error {
	t.Helper()
	return exec.Command(os.Args[0], "seccomp-helper", strconv.Itoa(nr)).Run()
}

func TestTripwireKillsTheProcessWithSIGSYSOnATrappedCall(t *testing.T) {
	_, trappedNrs, _ := trapTable()
	for _, nr := range trappedNrs {
		err := runHelper(t, int(nr))
		if !trapped(err) {
			t.Errorf("system call %d: want the process ended by SIGSYS, got %v", nr, err)
		}
	}
}

func TestTripwireLetsOrdinaryCallsThrough(t *testing.T) {
	for name, nr := range map[string]int{"getpid": syscall.SYS_GETPID, "read": syscall.SYS_READ, "futex": syscall.SYS_FUTEX, "openat": syscall.SYS_OPENAT, "clone": syscall.SYS_CLONE, "prctl": syscall.SYS_PRCTL, "setuid": syscall.SYS_SETUID} {
		if err := runHelper(t, nr); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) && trapped(err) {
				t.Errorf("%s was trapped", name)
			} else {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}

func TestTripwireListIsSaneAndMatchesGoNames(t *testing.T) {
	_, nrs, ok := trapTable()
	if !ok {
		t.Skip("no table")
	}
	seen := map[uint32]bool{}
	for _, nr := range nrs {
		if seen[nr] {
			t.Errorf("system call %d is in the table twice", nr)
		}
		seen[nr] = true
	}
	// The numbers the Go library names must be the ones in the table (a typo in a number would trap the wrong call).
	named := map[string]int{"ptrace": syscall.SYS_PTRACE, "mount": syscall.SYS_MOUNT, "umount2": syscall.SYS_UMOUNT2, "pivot_root": syscall.SYS_PIVOT_ROOT, "chroot": syscall.SYS_CHROOT, "reboot": syscall.SYS_REBOOT, "init_module": syscall.SYS_INIT_MODULE, "delete_module": syscall.SYS_DELETE_MODULE, "keyctl": syscall.SYS_KEYCTL, "add_key": syscall.SYS_ADD_KEY, "request_key": syscall.SYS_REQUEST_KEY, "unshare": syscall.SYS_UNSHARE, "perf_event_open": syscall.SYS_PERF_EVENT_OPEN, "kexec_load": syscall.SYS_KEXEC_LOAD}
	for name, nr := range named {
		if !seen[uint32(nr)] {
			t.Errorf("%s (%d) is not in the table", name, nr)
		}
	}
}

func TestTrapFilterJumpsStayInside(t *testing.T) {
	_, nrs, _ := trapTable()
	f, err := trapFilter(0, nrs)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range f {
		if in.code == bpfJeqK || in.code == bpfJgeK {
			if i+1+int(in.jt) >= len(f) || i+1+int(in.jf) >= len(f) {
				t.Errorf("instruction %d jumps out of the program", i)
			}
		}
	}
}
