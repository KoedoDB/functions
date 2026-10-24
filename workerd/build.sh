#!/usr/bin/env bash
# Build the KoedoDB variant of workerd (the upstream release in ./UPSTREAM + the patches in ./patches: per-Worker limits) on an ordinary machine, and record what it costs.
# Result: ./workerd-linux-amd64 (what the Dockerfile copies) and ./workerd.sha256 (its checksum, which the Dockerfile checks). Tested on Debian 13 "trixie" (x86_64); built before on Ubuntu 26.04.
# Run it ONLY on the build host, never on a machine with other work: the build is held to a memory cap, but it takes about an hour (about 20 minutes with a warm cache). Needs about 40 GB free disk, 8 GB RAM (the build is held to 5 GB of it), network.
# Usage:   ./build.sh            # everything: tools, source, patches, build, report
#          BUILD_ROOT=/path ./build.sh   # where everything goes (default: $HOME/koedodb-build)
# Everything downloaded is checked against a pinned SHA-256 (and the LLVM release against its Sigstore bundle with cosign) before it is used.
set -euo pipefail

BUILD_ROOT="${BUILD_ROOT:-$HOME/koedodb-build}"
KIT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Defaults follow the machine: 3 jobs / 5 GB for Bazel / a 7 GB cap is what was measured (a 8 GB machine); with 16 GB: 4 jobs, 10 GB, a 13 GB cap.
MEM_TOTAL_MB=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
if [ "$MEM_TOTAL_MB" -ge 15000 ]; then _J=4; _R=10240; _S=13G; else _J=3; _R=5120; _S=7G; fi
JOBS="${JOBS:-$_J}"               # concurrent compile jobs
BAZEL_RAM_MB="${BAZEL_RAM_MB:-$_R}"
SCOPE_MAX="${SCOPE_MAX:-$_S}"     # hard cap of the whole build (cgroup), when systemd-run is there
. "$KIT/UPSTREAM"                   # UPSTREAM_TAG, UPSTREAM_REV
UPSTREAM_REV="${UPSTREAM_REV}"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/cloudflare/workerd.git}"   
SRC_DIR="${SRC_DIR:-$BUILD_ROOT/workerd-src}"   # where the source goes, and what the outputs are called (nightly.sh: OUT_TAG=-nightly)
OUT_TAG="${OUT_TAG:-}"
BAZELISK_URL=https://github.com/bazelbuild/bazelisk/releases/download/v1.29.0/bazelisk-linux-amd64
BAZELISK_SHA=5a408715e932c0250d28bd84555f12edbf70117de42f9181691c736eacc4a992
COSIGN_URL=https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64
COSIGN_SHA=4629c757b7618056f8ddd7e2625ae9fdd94c0372a65049520bc7d9df9efc7f71
LLVM_VER=22.1.8
LLVM_TAR=LLVM-$LLVM_VER-Linux-X64.tar.xz
LLVM_URL=https://github.com/llvm/llvm-project/releases/download/llvmorg-$LLVM_VER/$LLVM_TAR
LLVM_SHA=df0e1ecf16caf3489a272a5eea4eec9b0d82878f6477fa309504f918a0006384

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# 0. The machine
[ "$(uname -m)" = x86_64 ] || die "x86_64 only"
for c in git curl xz python3 sha256sum nice; do command -v $c >/dev/null || die "missing command: $c (Ubuntu: sudo apt install git curl xz-utils python3 coreutils)"; done
[ -e /usr/include/stdio.h ] || die "missing libc headers (Ubuntu: sudo apt install build-essential)"
mkdir -p "$BUILD_ROOT"
free_gb=$(df -P "$BUILD_ROOT" | awk 'NR==2{print int($4/1048576)}')
[ "$free_gb" -ge 40 ] || die "only $free_gb GB free under $BUILD_ROOT; 40 GB are needed"
mem_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
log "machine: $(nproc) CPUs, ${mem_mb} MiB RAM, ${free_gb} GB free in $BUILD_ROOT, swap $(awk '/SwapTotal/{print int($2/1024)}' /proc/meminfo) MiB"
[ "$mem_mb" -ge 7000 ] || die "less than 7 GB RAM"
avail_mb=$(awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo)
[ "$avail_mb" -ge 6000 ] || die "only ${avail_mb} MiB available now: close the browser and other big programs, then run again"

# 1. Tools, each checked
fetch_checked() {  # url sha256 file
  if [ -f "$3" ] && [ "$(sha256sum "$3" | cut -d' ' -f1)" = "$2" ]; then return; fi
  log "download $(basename "$3")"
  curl -fsSL -o "$3.part" "$1"
  [ "$(sha256sum "$3.part" | cut -d' ' -f1)" = "$2" ] || { rm -f "$3.part"; die "SHA-256 of $(basename "$3") does not match the pinned value"; }
  mv "$3.part" "$3"
}
fetch_checked "$BAZELISK_URL" "$BAZELISK_SHA" "$BUILD_ROOT/bazelisk"; chmod +x "$BUILD_ROOT/bazelisk"
fetch_checked "$COSIGN_URL" "$COSIGN_SHA" "$BUILD_ROOT/cosign"; chmod +x "$BUILD_ROOT/cosign"
fetch_checked "$LLVM_URL" "$LLVM_SHA" "$BUILD_ROOT/$LLVM_TAR"
if [ ! -f "$BUILD_ROOT/$LLVM_TAR.jsonl" ]; then curl -fsSL -o "$BUILD_ROOT/$LLVM_TAR.jsonl" "$LLVM_URL.jsonl"; fi
log "verify the LLVM release signature (Sigstore bundle, built by llvm-project's GitHub Actions)"
"$BUILD_ROOT/cosign" verify-blob "$BUILD_ROOT/$LLVM_TAR" --bundle "$BUILD_ROOT/$LLVM_TAR.jsonl" \
  --certificate-identity-regexp '^https://github.com/llvm/llvm-project/' --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  || die "the LLVM signature does not verify"
if [ ! -x "$BUILD_ROOT/llvm/bin/clang" ]; then
  log "unpack LLVM (12 GB)"; mkdir -p "$BUILD_ROOT/llvm"; tar -xJf "$BUILD_ROOT/$LLVM_TAR" -C "$BUILD_ROOT/llvm" --strip-components=1
fi

# 1b. tclsh: sqlite3 (a dependency since 2026-10) generates a header with a Tcl script, and the build's PATH is fixed. Debian's tcl8.6 is unpacked here, without root, behind a small wrapper.
TOOLS="$BUILD_ROOT/tools"
if [ ! -x "$TOOLS/bin/tclsh" ] && ! command -v tclsh >/dev/null; then
  log "tclsh: unpack Debian's tcl8.6 (no root)"
  mkdir -p "$TOOLS/bin" "$TOOLS/deb" && ( cd "$TOOLS/deb" && apt-get download tcl8.6 libtcl8.6 >/dev/null 2>&1 ) || die "cannot download tcl8.6 (or install tclsh: sudo apt install tcl)"
  for d in "$TOOLS"/deb/*.deb; do dpkg -x "$d" "$TOOLS/root"; done
  printf '#!/bin/sh\nexport LD_LIBRARY_PATH="%s/root/usr/lib/x86_64-linux-gnu${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" TCL_LIBRARY="%s/root/usr/share/tcltk/tcl8.6"\nexec "%s/root/usr/bin/tclsh8.6" "$@"\n' "$TOOLS" "$TOOLS" "$TOOLS" > "$TOOLS/bin/tclsh"
  chmod +x "$TOOLS/bin/tclsh"
fi

# 2. Source: upstream at the pinned revision, then our patches
SRC="$SRC_DIR"
if [ ! -d "$SRC/.git" ]; then
  log "clone workerd"; git clone -q "$UPSTREAM_URL" "$SRC"
fi
git -C "$SRC" remote set-url origin "$UPSTREAM_URL"
git -C "$SRC" cat-file -e "$UPSTREAM_REV^{commit}" 2>/dev/null || git -C "$SRC" fetch -q origin
git -C "$SRC" checkout -q "$UPSTREAM_REV"
git -C "$SRC" checkout -q -B koedodb-limits
git -C "$SRC" reset -q --hard "$UPSTREAM_REV"
git -C "$SRC" -c user.name=koedodb -c user.email=build@koedodb.invalid am -q "$KIT"/patches/*.patch || die "a patch does not apply to $UPSTREAM_REV"
log "source: $(git -C "$SRC" log --oneline -4 | tr '\n' ';')"

# 3. Bazel settings (same as the measured build)
RC="$BUILD_ROOT/koedodb.bazelrc"
cat > "$RC" <<RCEOF
build --jobs=$JOBS
build --local_resources=memory=$BAZEL_RAM_MB
build --local_resources=cpu=$JOBS
build --repo_env=CC=$BUILD_ROOT/llvm/bin/clang
build --action_env=PATH=$BUILD_ROOT/llvm/bin:$BUILD_ROOT/tools/bin:/usr/local/bin:/usr/bin:/bin
build --repository_cache=$BUILD_ROOT/repo-cache
build --disk_cache=$BUILD_ROOT/disk-cache
build --action_env=LD_LIBRARY_PATH=$BUILD_ROOT/llvm/lib/x86_64-unknown-linux-gnu
build --host_action_env=LD_LIBRARY_PATH=$BUILD_ROOT/llvm/lib/x86_64-unknown-linux-gnu
build --host_linkopt=-Wl,-rpath,$BUILD_ROOT/llvm/lib/x86_64-unknown-linux-gnu
build --linkopt=-l:libc++abi.a
build --linkopt=-l:libunwind.a
build --host_linkopt=-lc++abi
build --host_linkopt=-lunwind
RCEOF

# 4. Build, held to a memory cap, with the peak recorded
REPORT="$BUILD_ROOT/report$OUT_TAG.txt"
LOG="$BUILD_ROOT/build$OUT_TAG.log"
cd "$SRC"
export PATH="$BUILD_ROOT/llvm/bin:$BUILD_ROOT/tools/bin:/usr/local/bin:/usr/bin:/bin"
RUN=( nice -n 10 "$BUILD_ROOT/bazelisk" --host_jvm_args=-Xmx2g --output_user_root="$BUILD_ROOT/bazel-root" --bazelrc="$RC" build //src/workerd/server:workerd --config=opt )
WRAP=()
# The user manager says "starting" for as long as a first-login job of the desktop runs (seen on 2026-10-18), and "degraded" with a failed unit: neither means the caps do not work. What proves it
# is a scope that really gets memory.max (the same test as run-tests.sh). Without a cap the build does not start (it grew to 16 GB on this kind of machine once): ALLOW_UNCAPPED=1 to override.
cap=$(systemd-run --user --scope -q -p MemoryMax=64M -p MemorySwapMax=0 sh -c 'cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/memory.max' 2>/dev/null || true)
if [ "$cap" = 67108864 ]; then
  WRAP=( systemd-run --user --scope --quiet --unit="workerd-build-$$" -p MemoryMax="$SCOPE_MAX" -p MemorySwapMax=0 -p TasksMax=4096 )
  log "build under a hard cap of $SCOPE_MAX (systemd scope)"
else
  [ "${ALLOW_UNCAPPED:-0}" = 1 ] || die "a systemd scope with MemoryMax=64M shows memory.max='$cap': the hard cap does not work here, so the build does not start (ALLOW_UNCAPPED=1 starts it with only Bazel's own limits)"
  log "no hard cap (ALLOW_UNCAPPED=1): only Bazel's own limits ($BAZEL_RAM_MB MiB, $JOBS jobs). Keep an eye on the memory."
fi
(  # sampler: the lowest MemAvailable seen, and the scope's own peak when there is one
  low=999999; while sleep 5; do
    a=$(awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo); [ "$a" -lt "$low" ] && low=$a
    echo "lowest_MemAvailable_MiB=$low" > "$BUILD_ROOT/.sampler$OUT_TAG"
    cg=$(ls -d /sys/fs/cgroup/user.slice/user-$(id -u).slice/user@$(id -u).service/*/workerd-build-$$.scope 2>/dev/null | head -1)
    [ -n "$cg" ] && [ -r "$cg/memory.peak" ] && echo "scope_memory_peak_MiB=$(( $(cat "$cg/memory.peak") / 1048576 ))" >> "$BUILD_ROOT/.sampler$OUT_TAG"
  done ) & SAMPLER=$!
trap 'kill $SAMPLER 2>/dev/null || true' EXIT
t0=$(date +%s)
log "build started (first time: expect 1.5-2 hours on 2 cores / 4 threads); log: $LOG"
set +e
"${WRAP[@]}" "${RUN[@]}" > "$LOG" 2>&1
rc=$?
set -e
t1=$(date +%s)
kill $SAMPLER 2>/dev/null || true
BIN="$SRC/bazel-bin/src/workerd/server/workerd"
{
  echo "date: $(date -Is)"; echo "machine: $(nproc) CPUs ($(lscpu | awk -F: '/Model name/{gsub(/^ +/,"",$2); print $2}')), ${mem_mb} MiB RAM"
  echo "result: exit code $rc, $(( t1 - t0 )) seconds"
  echo "jobs=$JOBS bazel_ram_mb=$BAZEL_RAM_MB scope_max=$SCOPE_MAX"
  tail -n 3 "$BUILD_ROOT/.sampler$OUT_TAG" 2>/dev/null | sort -u
  [ -f "$BIN" ] && { echo "binary: $BIN"; sha256sum "$BIN"; ls -l "$BIN" | awk '{print "size_bytes=" $5}'; "$BIN" --version 2>&1 | head -1; }
  du -sh "$BUILD_ROOT" 2>/dev/null | awk '{print "disk_used=" $1}'
  grep -E "INFO: Elapsed time|INFO: [0-9]+ processes|Build completed" "$LOG" | tail -3
} | tee "$REPORT"
[ $rc -eq 0 ] || { echo "The build failed; the last lines of $LOG:"; tail -n 25 "$LOG"; exit $rc; }
cp "$BIN" "$BUILD_ROOT/workerd-patched$OUT_TAG"
log "patched binary: $BUILD_ROOT/workerd-patched$OUT_TAG"
cp "$BIN" "$KIT/workerd-linux-amd64"; sha256sum "$BIN" | cut -d' ' -f1 > "$KIT/workerd.sha256"
log "copied to $KIT/workerd-linux-amd64, checksum in $KIT/workerd.sha256 (run the tests before the image is built from it)"
if [ "${BASELINE:-0}" = 1 ]; then
  # The unmodified upstream at the same revision, for the before/after comparison (most of the build is cached: only what the patches touch is rebuilt).
  log "building the unmodified baseline (BASELINE=1)"
  git -C "$SRC" checkout -q "$UPSTREAM_REV"
  set +e; "${WRAP[@]}" "${RUN[@]}" >> "$LOG" 2>&1; rc2=$?; set -e
  if [ $rc2 -eq 0 ]; then cp "$BIN" "$BUILD_ROOT/workerd-baseline"; echo "baseline binary: $BUILD_ROOT/workerd-baseline" | tee -a "$REPORT"; else echo "the baseline build failed (see $LOG)" | tee -a "$REPORT"; fi
  git -C "$SRC" checkout -q koedodb-limits
fi
log "done. Send back $REPORT (and, if it failed, the end of $LOG)."
