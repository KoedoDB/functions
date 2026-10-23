package spawner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheLimitsKillAProcessThatUsesTooMuchMemoryOrLoopsForLongEnough(t *testing.T) {
	l := limits{MemoryMiB: 256, CPUQuotaPct: 100, Look: 2 * time.Second, RunawayLooks: 5}
	var st watchState
	// The first look only takes the CPU time as the starting point; a process that is busy for 4 looks and rests once starts counting again.
	cpu := int64(1_000_000)
	for i, busy := range []bool{true, true, true, true, false, true, true, true, true} {
		if busy {
			cpu += 1_900_000 // 95% of a core in 2 s
		} else {
			cpu += 10_000
		}
		if got := l.check(&st, cpu, 60); got != "" {
			t.Fatalf("look %d: %q, nothing is wrong yet", i, got)
		}
	}
	cpu += 1_900_000
	if got := l.check(&st, cpu, 60); got != "runaway" { // 5 busy looks in a row
		t.Fatalf("5 busy looks in a row: %q, want runaway", got)
	}
	// Memory is decided at once, whatever the CPU does.
	var st2 watchState
	if got := l.check(&st2, 0, 300); got != "memory" {
		t.Fatalf("300 MiB: %q, want memory", got)
	}
	if got := l.check(&st2, 0, 256); got != "" {
		t.Fatalf("exactly the limit: %q, want none", got)
	}
	// A CPU time that is not known (negative) is never a runaway.
	var st3 watchState
	for i := 0; i < 20; i++ {
		if got := l.check(&st3, -1, 10); got != "" {
			t.Fatalf("unknown CPU: %q", got)
		}
	}
	// "Nearly all of the quota" follows the quota: 50% of a core is busy at 0.45 of a core when the quota is 50.
	half := limits{MemoryMiB: 256, CPUQuotaPct: 50, Look: 2 * time.Second, RunawayLooks: 2}
	var st4 watchState
	half.check(&st4, 0, 1)
	half.check(&st4, 900_000, 1) // 0.45 of a core for 2 s = 900 ms
	if got := half.check(&st4, 1_800_000, 1); got != "runaway" {
		t.Fatalf("a 50%% quota used at 45%%: %q, want runaway", got)
	}
}

func TestTheCPUTimeIsReadFromAProcStatLineWhoseNameHasSpacesAndParentheses(t *testing.T) {
	line := "4242 (work (er) 1) S 1 4242 4242 0 -1 4194560 100 0 0 0 150 50 0 0 20 0 8 0 100 1000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0"
	if us, ok := parseProcStat(line); !ok || us != 2_000_000 { // utime 150 + stime 50 ticks of 10 ms
		t.Errorf("parseProcStat = %d, %v; want 2000000 (200 ticks)", us, ok)
	}
	if _, ok := parseProcStat("garbage"); ok {
		t.Error("garbage is not a stat line")
	}
	if _, ok := parseProcStat("1 (x) S 1"); ok {
		t.Error("a short line is not a stat line")
	}
	self := procSampler{pid: os.Getpid()}
	if rss, ok := self.RSSMiB(); !ok || rss <= 0 {
		t.Errorf("RSS of this process: %v, %v", rss, ok)
	}
	if us, ok := self.CPUUsec(); !ok || us < 0 {
		t.Errorf("CPU of this process: %v, %v", us, ok)
	}
}

func TestTheCgroupOfACustomerIsMadeWithItsLimits(t *testing.T) {
	root := t.TempDir()
	cg, err := newCgroup(root, "acme", 256, 100, 64)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"memory.max": "268435456", "memory.swap.max": "0", "cpu.max": "100000 100000", "pids.max": "64"} {
		b, err := os.ReadFile(filepath.Join(cg.dir, file))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %q (%v), want %s", file, b, err, want)
		}
	}
	if filepath.Base(cg.dir) != "wd-acme" || filepath.Dir(cg.dir) != root {
		t.Errorf("the cgroup is %s", cg.dir)
	}
	cg.remove()
	if _, err := os.Stat(cg.dir); err == nil {
		// the files of the test are still in it: a real cgroup directory has none the kernel does not own, and rmdir works; here only an empty directory goes
		t.Log("a directory with files stays: only the real cgroup removes its own files")
	}
	half, err := newCgroup(root, "half", 128, 50, 10)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(half.dir, "cpu.max")); strings.TrimSpace(string(b)) != "50000 100000" {
		t.Errorf("cpu.max of a 50%% quota: %q", b)
	}
	for _, bad := range []string{"", "../x", "A", "a/b"} {
		if _, err := newCgroup(root, bad, 1, 1, 1); err == nil {
			t.Errorf("customer %q: no error", bad)
		}
	}
	if isCgroup2(root) {
		t.Error("a test directory is not on the cgroup file system")
	}
	s := cgroupSampler{dir: cg.dir}
	if err := os.WriteFile(filepath.Join(cg.dir, "cpu.stat"), []byte("usage_usec 1234\nuser_usec 1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg.dir, "memory.current"), []byte("104857600\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if us, ok := s.CPUUsec(); !ok || us != 1234 {
		t.Errorf("cpu.stat: %d, %v", us, ok)
	}
	if mib, ok := s.RSSMiB(); !ok || mib != 100 {
		t.Errorf("memory.current: %v, %v", mib, ok)
	}
}
