#!/bin/bash
# Runs the pinned Deno image (the file IMAGE) with the supervisor (supervisor/supervisor.ts) in Docker, as the Pod will run it: a non-root user, a read-only root file system, no capabilities,
# memory 512 MiB (the limit of the Functions Pod). It checks routing, the life cycle of a function, what a function can and cannot do, the limits, the warm module cache, and the modern
# JavaScript this runtime is chosen for.
#
#   tests/run.sh        # a few minutes; ends with "ALL OK"
#
# Needs Docker, python3 and curl, and the network (the npm: checks fetch packages from the npm registry). It uses no secrets: the "secrets" below are made up for the run.
#
# What is NOT checked here: gVisor (tests/bench-gvisor.sh measures it; the README has the numbers), Kubernetes (NetworkPolicy, the Secret mount), WebSocket and server-sent-event functions.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="$(tr -d '[:space:]' < "$here/IMAGE")"
run="kbfn$$"
work="${HOME}/.${run}"; rm -rf "$work"; mkdir -p "$work/cache"; chmod 777 "$work"
cleanup() { docker rm -f "${run}-fn" >/dev/null 2>&1; rm -rf "$work"; }
trap '[ -n "${KEEP:-}" ] || cleanup' EXIT
fail=0
check() {
  local out; out="$(eval "$2" 2>&1)"; local rc=$?
  if [ $rc = 0 ]; then echo "ok:   $1"; else echo "FAIL: $1"; fail=1; [ -n "${DEBUG:-}" ] && echo "      -> $(printf %s "$out" | head -20 | cut -c1-300)"; fi
}
note() { echo "note: $1"; }
step() { echo; echo "== $1"; }

# start [docker options]: the supervisor in a hardened container with the limits of the Pod; the functions of tests/functions are mounted read-only at /f
start() {
  docker rm -f "${run}-fn" >/dev/null 2>&1
  docker run -d --name "${run}-fn" --user 10001:10001 --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges --memory 512m --memory-swap 512m --cpus 1 -p 127.0.0.1::9000 \
    -e DENO_DIR=/tmp/deno -e FN_ENV_ALLOW=GREETING -e GREETING=hi -e SUPERVISOR_ONLY_SECRET=made-up-supervisor-secret "$@" \
    -v "$here/tests/functions:/f:ro" -v "$here/supervisor:/sup:ro" "$IMAGE" run --no-prompt --allow-net --allow-read=/f,/tmp,/cache --allow-write=/tmp --allow-run --allow-env /sup/supervisor.ts >/dev/null
  port="$(docker port "${run}-fn" 9000/tcp | head -1 | sed 's/.*://')"
  for _ in $(seq 60); do curl -s -m 1 "http://127.0.0.1:$port/_stats" >/dev/null && return 0; sleep 0.25; done; return 1
}
# req <path> [curl args]: CODE, BODY and HEADERS
req() { local path=$1; shift; CODE="$(curl -s -m 60 -D "$work/h" -o "$work/b" -w '%{http_code}' "$@" "http://127.0.0.1:${port}${path}")"; BODY="$(cat "$work/b")"; HEADERS="$(cat "$work/h")"; }
jget() { python3 -c "
import sys, json
d = json.load(sys.stdin)
for k in sys.argv[1].split('.'):
    d = d[k] if isinstance(d, dict) else d[int(k)]
print(d)" "$1"; }
probe() { req "/probe?t=$1"; printf %s "$BODY"; }          # what one probe of the probe function reports
denied() { probe "$1" | grep -q '"ok":false' && probe "$1" | grep -q 'NotCapable\|Requires read access'; }
alive() { [ "$(docker inspect -f '{{.State.Status}}' "${run}-fn")" = running ] && [ "$(curl -s -m 8 "http://127.0.0.1:$port/hello")" = "hello /" ]; }

step "The supervisor starts as the Pod will run it"
start; check "it starts" '[ -n "$port" ] && curl -s -m 2 "http://127.0.0.1:$port/_stats" | grep -q running'
check "it runs as user 10001, on a read-only root file system, with no capabilities, memory 512 MiB" '[ "$(docker inspect -f "{{.Config.User}}" '"$run"'-fn)" = 10001:10001 ] && [ "$(docker inspect -f "{{.HostConfig.ReadonlyRootfs}}" '"$run"'-fn)" = true ] && [ "$(docker inspect -f "{{.HostConfig.CapDrop}}" '"$run"'-fn)" = "[ALL]" ] && [ "$(docker inspect -f "{{.HostConfig.Memory}}" '"$run"'-fn)" = 536870912 ]'
tag="$(sed 's/^[^:]*:\([0-9.]*\)@.*/\1/' "$here/IMAGE")"
check "it is the Deno in IMAGE ($tag)" '[ "$(docker run --rm "$IMAGE" --version | head -1 | cut -d" " -f2)" = "$tag" ]'

step "Routing: a Supabase-style function, as it is"
check "a function answers at /<name>/<path>?<query> (the path inside the function is what follows the name)" 'req "/hello/a/b?x=1"; [ "$CODE" = 200 ] && [ "$BODY" = "hello /a/b" ]'
check "the method, the headers and the body get through, and so do the status and the headers of the answer" 'req "/echo/p?status=201&q=1" -X PUT -H "x-test: abc" -d "payload"; [ "$CODE" = 201 ] && [ "$(printf %s "$BODY" | jget method)" = PUT ] && [ "$(printf %s "$BODY" | jget body)" = payload ] && [ "$(printf %s "$BODY" | jget header)" = abc ] && [ "$(printf %s "$BODY" | jget query)" = "?status=201&q=1" ] && printf %s "$HEADERS" | grep -qi "x-from-function: yes"'
check "a large request body (5 MB) gets through" 'head -c 5000000 /dev/zero | tr "\0" "a" > "$work/big"; req "/echo/p" -X POST --data-binary @"$work/big"; [ "$(printf %s "$BODY" | jget body | wc -c)" -ge 5000000 ]'
check "a function that does not exist: 404" 'req /nope; [ "$CODE" = 404 ]'
check "a bad name is refused: upper case (400); dots, an encoded .., a long name (400 or 404)" 'req /Hello; [ "$CODE" = 400 ] || exit 1; for p in /.. /%2e%2e%2fetc /..%2f..%2fetc "/$(printf a%.0s $(seq 50))"; do req "$p"; [ "$CODE" = 400 ] || [ "$CODE" = 404 ] || exit 1; done'

step "Life cycle: started on the first request, stopped when idle"
start -e IDLE_MS=4000 -e REQUEST_MS=4000 >/dev/null
check "no function runs before the first request" '[ "$(curl -s "http://127.0.0.1:$port/_stats")" = "{\"running\":[]}" ]'
req /hello/x; cold="$(printf %s "$HEADERS" | grep -i x-fn-ms | tr -dc 0-9)"; req /hello/x; warm="$(printf %s "$HEADERS" | grep -i x-fn-ms | tr -dc 0-9)"
check "the first request starts it (a cold start under 5 s: ${cold} ms) and the next ones are fast (${warm} ms)" '[ "$cold" -lt 5000 ] && [ "$warm" -lt 100 ]'
sleep 7
check "after IDLE_MS without a request it is stopped" '[ "$(curl -s "http://127.0.0.1:$port/_stats")" = "{\"running\":[]}" ]'
req /hello/x; cold2="$(printf %s "$HEADERS" | grep -i x-fn-ms | tr -dc 0-9)"
check "and the next request starts it again (${cold2} ms)" '[ "$CODE" = 200 ]'
check "a function that exits: that request fails (502), the next request starts a fresh process, the others are not touched" 'req /exit; a=$CODE; req /hello/x; [ "$a" = 502 ] && [ "$CODE" = 200 ]'

step "What a function can and cannot do (Deno permissions)"
check "its own environment variable is readable, the supervisor's is not" '[ "$(probe ownEnv)" = "{\"ok\":true,\"value\":\"hi\"}" ] && denied otherEnv'
check "reading all of the environment is refused" 'denied envAll'
check "reading a file outside its own directory is refused (/etc/passwd, /proc/self/environ, /, a sibling function, the supervisor)" 'for p in readPasswd readProcEnviron listRoot listFunctions readOtherFunction readSupervisor; do denied $p || { echo "$p"; exit 1; }; done'
check "writing anywhere but its own scratch directory is refused (/x, /tmp/x)" 'denied writeRoot && denied writeTmp'
check "starting a program is refused" 'denied spawn && denied kill'
check "native code (FFI) is refused" 'denied ffi'
check "system information (hostname, OS release, uid) is refused" 'denied hostname && denied osRelease && denied uid'
note "KNOWN LIMIT: an import() whose specifier is a string literal (or a concatenation of literals) is loaded when the module graph is built, and --allow-read does not apply to it; a file that is in the graph that way can then be imported by a computed specifier too. A function can read the part of the first line of any file its user can read (about 80 characters around the syntax error) through the error message of that import: /etc/passwd, the start of /proc/1/environ (the environment of the Pod) and of the supervisor's /proc/<pid>/environ. The author of a function writes the literal, so this cannot be stopped inside the process. The wall is the Pod (and gVisor): a Pod holds one Project, and only secrets that Project's functions may read."
check "(that note) import('file:///etc/passwd') reaches the file: the error shows its first line" 'probe importPasswd | grep -q "root:x:0:0"'
check "(that note) import('file:///proc/1/environ') shows the start of the Pod's environment" 'probe importPid1Environ | grep -q "HOSTNAME="'
check "an import whose specifier is computed at run time, for a file that no literal import names, IS checked against --allow-read (refused)" 'denied importComputed'
note "KNOWN LIMIT: the network is not restricted by Deno (--allow-net is all of it). NetworkPolicy has to keep a function away from other Projects, the node and the metadata address."
check "(that note) a function can reach 127.0.0.1" 'probe fetchLoopback9000 | grep -q "\"ok\":true"'

step "Limits (memory 512 MiB)"
start -e IDLE_MS=60000 -e REQUEST_MS=4000 >/dev/null; for f in hello echo; do curl -s -m 10 -o /dev/null "http://127.0.0.1:$port/$f/a"; done
for t in cpu async sleep; do
  t0=$(date +%s); req "/abuse?t=$t"; t1=$(date +%s)
  check "an endless loop or a sleep ($t) is cancelled at REQUEST_MS: 504, in about 4 s, and the function process is killed" '[ "$CODE" = 504 ] && [ $((t1 - t0)) -le 8 ] && ! curl -s "http://127.0.0.1:$port/_stats" | grep -q abuse'
done
(curl -s -m 20 -o /dev/null "http://127.0.0.1:$port/abuse?t=cpu" &) ; sleep 0.5
t0=$(date +%s%N); req /hello/x; t1=$(date +%s%N)
check "while one function burns the CPU, another answers at once (under 500 ms)" '[ "$CODE" = 200 ] && [ $(( (t1 - t0) / 1000000 )) -lt 500 ]'
sleep 5
check "a 200 MB response is served" 'req "/abuse?t=big"; [ "$CODE" = 200 ] && [ "${#BODY}" -ge 200000000 ]'
step "Concurrency: requests in flight per function (MAX_CONCURRENT)"
note "The memory of a function grows with the requests it is serving at once (rate times latency), not with the rate. The supervisor counts them per function and answers 429 (Retry-After: 1) over the limit; the rate (requests per second) is limited at the gateway."
start -e MAX_CONCURRENT=10 -e IDLE_MS=60000 -e REQUEST_MS=10000 >/dev/null; curl -s -m 10 -o /dev/null "http://127.0.0.1:$port/sleepy/a"; curl -s -m 10 -o /dev/null "http://127.0.0.1:$port/hello/a"
for i in $(seq 1 25); do ( curl -s -o /dev/null -w '%{http_code}\n' -m 20 "http://127.0.0.1:$port/sleepy/a" > "$work/c$i" ) & done
sleep 0.5; req /hello/x; other=$CODE; wait
n200=$(cat $work/c[0-9]* | grep -c '^200$'); n429=$(cat $work/c[0-9]* | grep -c '^429$'); rm -f $work/c[0-9]*
check "25 requests at once for a function that sleeps 1.5 s, limit 10: exactly 10 are served, 15 get 429 ($n200 and $n429)" '[ "$n200" = 10 ] && [ "$n429" = 15 ]'
check "while it is full, another function answers (200)" '[ "$other" = 200 ]'
check "a 429 has Retry-After and the function is not touched; after the others finish, requests are served again" 'for i in $(seq 1 12); do ( curl -s -o /dev/null -m 20 "http://127.0.0.1:$port/sleepy/a" & ) ; done; sleep 0.4; req /sleepy/a; c=$CODE; printf %s "$HEADERS" | grep -qi "^retry-after: 1"; r=$?; sleep 3; req /sleepy/a; [ "$c" = 429 ] && [ $r = 0 ] && [ "$CODE" = 200 ]'
check "an answer that is still being sent counts as in flight: 10 streams of 4 s fill the limit (their headers are long gone), the 11th gets 429, and a stream that has ended frees its place" 'for i in $(seq 1 10); do ( curl -s -o /dev/null -m 20 "http://127.0.0.1:$port/stream/a" & ) ; done; sleep 1.5; req /stream/a; c=$CODE; sleep 4; req /stream/a; [ "$c" = 429 ] && [ "$CODE" = 200 ]'
sleep 5

note "The supervisor gives every function process a V8 heap limit of its own (HEAP_MB, 192 MiB here). Left to Deno, each process takes about half of the Pod's memory as its limit, so the processes of one Pod add up to more than the Pod has and the kernel kills one first (in 6 runs the kernel killed a process every time, and once it killed the Pod itself)."
survived=0; for i in 1 2 3 4 5 6; do start -e IDLE_MS=60000 -e REQUEST_MS=8000 >/dev/null; for f in hello echo; do curl -s -m 10 -o /dev/null "http://127.0.0.1:$port/$f/a"; done; req "/abuse?t=heap"; [ "$CODE" = 502 ] && [ "$(docker inspect -f '{{.State.OOMKilled}}' "${run}-fn")" = false ] && alive && survived=$((survived + 1)); done
check "the V8 heap filled up: V8 stops that function (502), the kernel kills nothing, and the Pod and the next request are fine: $survived of 6 runs" '[ "$survived" = 6 ]'
note "Memory outside of the V8 heap (ArrayBuffers) is NOT capped. It is eaten up to the Pod's limit, and the kernel kills the biggest process, which is usually the function; sometimes it kills the supervisor and the Pod's container stops. That is why this runs with --restart unless-stopped (what the kubelet does): the check is that the Pod serves again within 30 s, not that nothing was killed."
survived=0; restarted=0
for i in 1 2 3 4 5; do
  start -e IDLE_MS=60000 -e REQUEST_MS=8000 --restart unless-stopped >/dev/null; for f in hello echo; do curl -s -m 10 -o /dev/null "http://127.0.0.1:$port/$f/a"; done
  req "/abuse?t=mem"; back=0
  for _ in $(seq 30); do port="$(docker port "${run}-fn" 9000/tcp 2>/dev/null | head -1 | sed 's/.*://')"; [ -n "$port" ] && [ "$(curl -s -m 3 "http://127.0.0.1:$port/hello")" = "hello /" ] && { back=1; break; }; sleep 1; done
  [ "$back" = 1 ] && survived=$((survived + 1)); [ "$(docker inspect -f '{{.RestartCount}}' "${run}-fn")" != 0 ] && restarted=$((restarted + 1))
done
check "ArrayBuffers eaten up to the Pod limit: the Pod serves again within 30 s in every run: $survived of 5 runs ($restarted of them needed a restart of the container)" '[ "$survived" = 5 ]'

step "Warm the module cache: a heavy function starts at once, and nothing is downloaded at run time"
note "drizzle-orm pulls in a long list of npm packages: with a cold cache its first start takes 5-7 s. At deploy time the cache is warmed (deno cache); at run time CACHED_ONLY=1 forbids every download."
mkdir -p "$work/cache"; t0=$(date +%s)
docker run --rm --user "$(id -u):$(id -g)" -e DENO_DIR=/cache -v "$work/cache:/cache" -v "$here/tests/functions:/f:ro" "$IMAGE" cache /f/drizzle/index.ts >/dev/null 2>&1; t1=$(date +%s)
check "deno cache of the function fills a cache directory (the warm-up took $((t1 - t0)) s)" '[ "$(du -sk "$work/cache" | cut -f1)" -gt 5000 ]'
start -e DENO_DIR=/cache -e CACHED_ONLY=1 -v "$work/cache:/cache:ro" >/dev/null
req /drizzle/a; ms="$(printf %s "$HEADERS" | grep -i x-fn-ms | tr -dc 0-9)"
check "with the warm cache (read-only) and CACHED_ONLY=1 the heavy function answers on its first request (${ms} ms, under 2 s)" '[ "$CODE" = 200 ] && printf %s "$BODY" | grep -q "select" && [ "$ms" -lt 2000 ]'
t0=$(date +%s%N); req /uncached/a; t1=$(date +%s%N)
check "a module that is not in the cache fails at once (502 in $(( (t1 - t0) / 1000000 )) ms), it is not downloaded" '[ "$CODE" = 502 ] && [ $(( (t1 - t0) / 1000000 )) -lt 3000 ]'
check "and the function that was warmed still works afterwards" 'req /drizzle/a; [ "$CODE" = 200 ]'
docker rm -f "${run}-fn" >/dev/null 2>&1

step "Modern JavaScript, the reason for this runtime (the same file in Supabase edge-runtime v1.77.4 gives the values in the README)"
start -e IDLE_MS=60000 -e REQUEST_MS=60000 >/dev/null
req /features; echo "$BODY" > "$work/features.json"
fj() { python3 -c "import sys,json; print(json.load(open('$work/features.json'))[sys.argv[1]])" "$1"; }
check "the versions are the ones in IMAGE (Deno $tag, V8 15, TypeScript 6)" '[ "$(fj deno)" = "$tag" ] && [ "$(fj v8 | cut -d. -f1)" = 15 ] && [ "$(fj ts | cut -d. -f1)" = 6 ]'
for f in "Promise.try" "Iterator helpers (.map on iterators)" "Set.prototype.union" "Array.fromAsync" "Object.groupBy" "RegExp.escape" "Error.isError" "Math.sumPrecise" "Float16Array" "Uint8Array.fromBase64" "Temporal" "URL.parse" "URLPattern" "Intl.DurationFormat" "WebCrypto Ed25519" "node:sqlite" "node:zlib zstd"; do
  check "$f" '[ "$(fj "$f")" = True ]'
done
check "WebCrypto AES-GCM refuses a forged authentication tag" '[ "$(fj "WebCrypto AES-GCM rejects a forged tag")" = True ]'
for p in "zod@4" "hono@4" "@supabase/supabase-js@2" "openai@6" "stripe" "drizzle-orm" "jose"; do
  check "npm:$p can be imported (dynamic import: Supabase edge-runtime refuses it)" '[ "$(fj "npm:$p" | cut -c1-2)" = ok ]'
done

echo
[ "$fail" = 0 ] && echo "ALL OK" || { echo "SOME CHECKS FAILED"; exit 1; }
