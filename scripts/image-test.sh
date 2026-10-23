#!/bin/bash
# Checks the image the way the Pod runs it: a non-root user, a read-only root file system, no capabilities, a memory limit, only /sockets writable.
#
#   docker build -t koedodb-functions . && scripts/image-test.sh koedodb-functions
#
# It needs docker and curl. Ends with "ALL OK"; anything else exits 1 after saying what failed. The container is removed at the end.
set -u
IMAGE="${1:-koedodb-functions}"
PORT="${PORT:-19000}"
work="$(mktemp -d)"
name="fn-image-test-$$"
fail=0
bad() { echo "FAIL: $*"; fail=1; }
ok() { echo "ok:   $*"; }
cleanup() { docker rm -f "$name" >/dev/null 2>&1; rm -rf "$work"; }
trap cleanup EXIT

# ---- the image itself
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -s /etc/ssl/certs/ca-certificates.crt' && ok "the CA certificates are in the image (workerd aborts without them)" || bad "no /etc/ssl/certs/ca-certificates.crt"
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'command -v node npm npx corepack >/dev/null 2>&1' && bad "node or npm is in the image" || ok "no node, npm, npx or corepack in the image"
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -z "$(ls -A /var/lib/apt/lists 2>/dev/null)"' && ok "the apt lists are empty" || bad "the apt lists are not empty"
docker run --rm --entrypoint /sup/workerd "$IMAGE" --version >/dev/null 2>&1 && ok "workerd runs on the image's glibc" || bad "workerd does not run"
uid="$(docker run --rm --entrypoint /usr/bin/id "$IMAGE" -u)"; [ "$uid" = 10001 ] && ok "the user is $uid" || bad "the user is $uid, want 10001"
echo "image size: $(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.0f MiB", $1/1048576}')"

# ---- two customers: acme (the functions that misbehave) and globex (the neighbour)
mkdir -p "$work/c/acme/hello" "$work/c/acme/loop" "$work/c/acme/heap" "$work/c/globex/count"
cat > "$work/c/acme/hello/index.mjs" <<'JS'
let n = 0;
export default () => new Response('hello ' + (++n) + '\n');
JS
cat > "$work/c/acme/loop/index.mjs" <<'JS'
export default () => { for (;;) {} };
JS
cat > "$work/c/acme/heap/index.mjs" <<'JS'
export default () => { const keep = []; for (;;) keep.push(new Array(100000).fill(keep.length)); };
JS
cat > "$work/c/globex/count/index.mjs" <<'JS'
let n = 0;
export default () => new Response(String(++n));
JS
chmod -R a+rX "$work/c"

docker run -d --name "$name" --read-only --user 10001:10001 --cap-drop ALL --security-opt no-new-privileges --memory 1g --memory-swap 1g --pids-limit 512 \
  --tmpfs /sockets:rw,uid=10001,gid=10001,mode=0700,size=64m -v "$work/c:/c:ro" -e CUSTOMERS_DIR=/c -p "127.0.0.1:$PORT:9000" "$IMAGE" >/dev/null || { bad "the container does not start"; exit 1; }
for i in $(seq 50); do curl -s -o /dev/null -m 1 "http://127.0.0.1:$PORT/_stats" && break; sleep 0.2; done

get() { curl -s -m "${2:-10}" -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$1"; }
body() { curl -s -m 10 "http://127.0.0.1:$PORT$1"; }
[ "$(get /acme/hello/)" = 200 ] && ok "hello answers 200 (a cold start)" || bad "hello: $(get /acme/hello/)"
[ "$(body /globex/count/)" = 1 ] && ok "the neighbour is a process of its own" || bad "the neighbour's first answer is not 1"

# ---- a loop: stopped in about ten seconds, the neighbour does not feel it, the next request is a fresh process
t0=$(date +%s)
get /acme/loop/ 3 >/dev/null
stopped=""
for i in $(seq 60); do
  if docker logs "$name" 2>&1 | grep -q 'customer=acme reason=runaway'; then stopped=$(( $(date +%s) - t0 )); break; fi
  sleep 1
done
[ -n "$stopped" ] && [ "$stopped" -le 25 ] && ok "the loop was stopped after about ${stopped} s" || bad "the loop was not stopped in time (${stopped:-never})"
[ "$(body /globex/count/)" = 2 ] && ok "the neighbour kept its process while acme looped" || bad "the neighbour was restarted or hurt: $(body /globex/count/)"
[ "$(body /acme/hello/)" = "hello 1" ] && ok "the next request of acme runs in a fresh process" || bad "acme after the loop: $(body /acme/hello/)"

# ---- a memory bomb: killed at the limit (256 MiB), the neighbour is not hurt
get /acme/heap/ 10 >/dev/null
for i in $(seq 40); do docker logs "$name" 2>&1 | grep -q 'customer=acme reason=memory' && break; sleep 0.5; done
docker logs "$name" 2>&1 | grep -q 'customer=acme reason=memory' && ok "the memory bomb was killed at the limit" || bad "no memory kill in the log"
[ "$(body /globex/count/)" = 3 ] && ok "the neighbour is still the same process after the bomb" || bad "the neighbour after the bomb: $(body /globex/count/)"
[ "$(body /acme/hello/)" = "hello 1" ] && ok "acme starts again after the bomb" || bad "acme after the bomb: $(body /acme/hello/)"

[ "$(docker inspect "$name" --format '{{.State.Running}}')" = true ] && ok "the supervisor is still running" || bad "the supervisor stopped"
if [ "$fail" = 0 ]; then echo "ALL OK"; else docker logs "$name" 2>&1 | tail -20; exit 1; fi
