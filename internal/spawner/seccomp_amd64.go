package spawner

// auditArchX8664 is AUDIT_ARCH_X86_64.
const auditArchX8664 = 0xc000003e

// trapTable: the system calls (x86-64 numbers) that workerd, V8 and the C library never call and that a way out of the wall needs: tracing and reading another process, the kernel's
// BPF, perf, userfaultfd and io_uring (the guest's kernel has no io_uring and no userfaultfd: this is for a kernel that has them), keys, namespaces and mounts, kernel modules and kexec,
// handles that bypass path checks, time and reboot. The test (seccomp_test.go) checks the ones the Go library names.
func trapTable() (uint32, []uint32, bool) {
	return auditArchX8664, []uint32{
		101, // ptrace
		103, // syslog
		155, // pivot_root
		161, // chroot
		163, // acct
		165, // mount
		166, // umount2
		167, // swapon
		168, // swapoff
		169, // reboot
		172, // iopl
		173, // ioperm
		175, // init_module
		176, // delete_module
		212, // lookup_dcookie
		246, // kexec_load
		248, // add_key
		249, // request_key
		250, // keyctl
		272, // unshare
		298, // perf_event_open
		303, // name_to_handle_at
		304, // open_by_handle_at
		308, // setns
		310, // process_vm_readv
		311, // process_vm_writev
		312, // kcmp
		313, // finit_module
		320, // kexec_file_load
		321, // bpf
		323, // userfaultfd
		425, // io_uring_setup
		426, // io_uring_enter
		427, // io_uring_register
		428, // open_tree
		429, // move_mount
		430, // fsopen
		431, // fsconfig
		432, // fsmount
		433, // fspick
		438, // pidfd_getfd
		442, // mount_setattr
	}, true
}
