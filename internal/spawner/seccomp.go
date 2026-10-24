package spawner

// The tripwire: a seccomp filter on a customer's process that kills it, with SIGSYS, the moment it calls a system call that workerd never needs and that is a step of getting out of
// the wall (the kernel is shared by all customers in the VM: docs/shared-vm.md). It is not a sandbox, the Pod's profile is that; it is an alarm with almost no false positives, because
// normal code never calls these. The spawner sees the SIGSYS in the way the process ended and says so in the log (watchTrap in launcher.go).
//
// The filter is installed by the child (child.go), after no_new_privs and before it executes workerd, so that workerd's first instruction already runs under it. It is inherited by every
// thread and every process the customer's code makes, and it cannot be removed.

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	prSetSeccomp        = 22 // PR_SET_SECCOMP
	seccompModeFilter   = 2  // SECCOMP_MODE_FILTER
	seccompRetKillProc  = 0x80000000
	seccompRetAllow     = 0x7fff0000
	bpfLdWAbs           = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK             = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJgeK             = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfRetK             = 0x06 // BPF_RET | BPF_K
	seccompDataNrOffset = 0    // struct seccomp_data: int nr; __u32 arch; ...
	seccompDataArchOff  = 4
	x32SyscallBit       = 0x40000000 // an x86-64 process may call the x32 numbers; none is a system call of ours
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	len    uint16
	filter *sockFilter
}

// trapFilter builds the filter: a call from another architecture's ABI, an x32 number, or one of the system calls in trapped, ends the process; everything else is allowed.
func trapFilter(arch uint32, trapped []uint32) ([]sockFilter, error) {
	if len(trapped) == 0 || len(trapped) > 200 {
		return nil, errors.New("the list of trapped system calls is empty or too long")
	}
	// Layout: 0 load arch, 1 jeq arch (else kill), 2 load nr, 3 jge x32 (then kill), then one jeq per trapped call, then ALLOW (n), then KILL (n+1). A jump is 255 instructions at most;
	// 200 calls keep every one inside.
	n := 4 + len(trapped)
	kill := n + 1
	f := make([]sockFilter, 0, n+2)
	jump := func(from int) uint8 { return uint8(kill - (from + 1)) }
	f = append(f,
		sockFilter{code: bpfLdWAbs, k: seccompDataArchOff},
		sockFilter{code: bpfJeqK, jt: 0, jf: jump(1), k: arch},
		sockFilter{code: bpfLdWAbs, k: seccompDataNrOffset},
		sockFilter{code: bpfJgeK, jt: jump(3), jf: 0, k: x32SyscallBit},
	)
	for i, nr := range trapped {
		at := 4 + i
		f = append(f, sockFilter{code: bpfJeqK, jt: jump(at), jf: 0, k: nr})
	}
	f = append(f, sockFilter{code: bpfRetK, k: seccompRetAllow}, sockFilter{code: bpfRetK, k: seccompRetKillProc})
	return f, nil
}

// installTrap puts the filter on the calling thread. The caller must have set no_new_privs (this is not allowed without it or CAP_SYS_ADMIN) and must be on the thread that executes workerd
// (runtime.LockOSThread): a filter belongs to a thread, and execve keeps the one of the thread that calls it.
func installTrap() error {
	arch, trapped, ok := trapTable()
	if !ok {
		return fmt.Errorf("no seccomp tripwire for this architecture")
	}
	f, err := trapFilter(arch, trapped)
	if err != nil {
		return err
	}
	prog := sockFprog{len: uint16(len(f)), filter: &f[0]}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter, uintptr(unsafe.Pointer(&prog))); e != 0 {
		return fmt.Errorf("seccomp: %v", e)
	}
	return nil
}
