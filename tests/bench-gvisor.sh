#!/bin/bash
# The same small work natively and under gVisor (runsc), to see what gVisor costs a Deno function: V8 compute, file system calls, HTTP over a Unix socket, memory allocation.
# Not part of tests/run.sh (it needs runsc on the machine). It uses `runsc do` (rootless, systrap): that is not Kubernetes with gVisor, only a first look. Run it twice and compare.
#   tests/bench-gvisor.sh
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="$(tr -d '[:space:]' < "$here/IMAGE")"
command -v runsc >/dev/null || { echo "runsc is not installed"; exit 2; }
w="${HOME}/.kbfn-bench$$"; mkdir -p "$w"; trap 'rm -rf "$w"' EXIT
id="$(docker create "$IMAGE")"; docker cp "$id:/usr/bin/deno" "$w/deno" >/dev/null; docker rm "$id" >/dev/null
cat > "$w/bench.ts" <<'EOF'
const t0 = performance.now(); let x = 0; for (let i = 0; i < 5e7; i++) x += Math.sqrt(i); const cpu = performance.now() - t0;
const t1 = performance.now(); const dir = Deno.makeTempDirSync();
for (let i = 0; i < 3000; i++) { Deno.writeTextFileSync(`${dir}/f${i}`, 'x'); Deno.readTextFileSync(`${dir}/f${i}`); }
const fs = performance.now() - t1;
const sock = `${dir}/s.sock`; const server = Deno.serve({ path: sock, onListen() {} }, () => new Response('ok'));
const client = Deno.createHttpClient({ proxy: { transport: 'unix', path: sock } });
const t2 = performance.now(); for (let i = 0; i < 3000; i++) await (await fetch('http://f/', { client })).text(); const http = performance.now() - t2;
const t3 = performance.now(); const big: Uint8Array[] = []; for (let i = 0; i < 20; i++) big.push(new Uint8Array(10 * 1024 * 1024).fill(1)); const mem = performance.now() - t3;
console.log(JSON.stringify({ cpu_ms: Math.round(cpu), fs_3000_files_ms: Math.round(fs), http_3000_requests_ms: Math.round(http), alloc_200MB_ms: Math.round(mem) }));
await server.shutdown(); Deno.removeSync(dir, { recursive: true });
EOF
export DENO_DIR="$w/cache"
for i in 1 2; do
  echo "native:  $("$w/deno" run --allow-all "$w/bench.ts")"
  echo "gVisor:  $(runsc --rootless --network=host --ignore-cgroups --platform=systrap do "$w/deno" run --allow-all "$w/bench.ts")"
done
