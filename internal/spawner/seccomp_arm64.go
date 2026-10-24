package spawner

// auditArchAarch64 is AUDIT_ARCH_AARCH64.
const auditArchAarch64 = 0xc00000b7

// trapTable: the same system calls as seccomp_amd64.go, with the arm64 numbers (arm64 has no iopl, ioperm, and the old calls of the x86-64 table).
func trapTable() (uint32, []uint32, bool) {
	return auditArchAarch64, []uint32{
		18,  // lookup_dcookie
		39,  // umount2
		40,  // mount
		41,  // pivot_root
		51,  // chroot
		89,  // acct
		97,  // unshare
		104, // kexec_load
		105, // init_module
		106, // delete_module
		116, // syslog
		117, // ptrace
		142, // reboot
		217, // add_key
		218, // request_key
		219, // keyctl
		224, // swapon
		225, // swapoff
		241, // perf_event_open
		264, // name_to_handle_at
		265, // open_by_handle_at
		268, // setns
		270, // process_vm_readv
		271, // process_vm_writev
		272, // kcmp
		273, // finit_module
		280, // bpf
		282, // userfaultfd
		294, // kexec_file_load
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
