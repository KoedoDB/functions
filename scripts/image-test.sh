#!/bin/bash
# Checks the image the way the Pod runs it: two containers sharing a network and a tmpfs, the spawner as root with exactly four capabilities and a read-only root file system, the router as
# user 10001 with none, mutual TLS on the control API, a memory limit on each. Two customers are put through the control API, one of them misbehaves.
#
#   docker build -t koedodb-functions . && scripts/image-test.sh koedodb-functions
#
# It needs docker, curl, openssl and base64. Ends with "ALL OK"; anything else exits 1 after saying what failed. The containers and the volume are removed at the end.
set -u
IMAGE="${1:-koedodb-functions}"
PORT="${PORT:-19000}"
CPORT="${CPORT:-19443}"
work="$(mktemp -d)"
sp="fn-spawner-$$"
rt="fn-router-$$"
vol="fn-sockets-$$"
fail=0
bad() { echo "FAIL: $*"; fail=1; }
ok() { echo "ok:   $*"; }
cleanup() { docker rm -f "$sp" "$rt" >/dev/null 2>&1; docker volume rm "$vol" >/dev/null 2>&1; rm -rf "$work"; }
trap cleanup EXIT

# ---- the image itself
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -s /etc/ssl/certs/ca-certificates.crt' && ok "the CA certificates are in the image (workerd aborts without them)" || bad "no /etc/ssl/certs/ca-certificates.crt"
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'command -v node npm npx corepack >/dev/null 2>&1' && bad "node or npm is in the image" || ok "no node, npm, npx or corepack in the image"
docker run --rm --entrypoint /bin/sh "$IMAGE" -c 'test -z "$(ls -A /var/lib/apt/lists 2>/dev/null)"' && ok "the apt lists are empty" || bad "the apt lists are not empty"
docker run --rm --entrypoint /sup/workerd "$IMAGE" --version >/dev/null 2>&1 && ok "workerd runs on the image's glibc" || bad "workerd does not run"
echo "image size: $(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.0f MiB", $1/1048576}')"

# ---- the certificates of the control API: a CA, the spawner's server certificate, the Regional Agent's client certificate (common name regional-agent)
c="$work/certs"; mkdir -p "$c"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$c/ca.key" -out "$c/ca.pem" -subj /CN=test-ca -days 2 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$c/server.key" -out "$c/server.csr" -subj /CN=spawner 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$c/server.ext"
openssl x509 -req -in "$c/server.csr" -CA "$c/ca.pem" -CAkey "$c/ca.key" -CAcreateserial -out "$c/server.pem" -days 2 -extfile "$c/server.ext" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$c/agent.key" -out "$c/agent.csr" -subj /CN=regional-agent 2>/dev/null
printf 'extendedKeyUsage=clientAuth\n' > "$c/agent.ext"
openssl x509 -req -in "$c/agent.csr" -CA "$c/ca.pem" -CAkey "$c/ca.key" -CAcreateserial -out "$c/agent.pem" -days 2 -extfile "$c/agent.ext" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout "$c/other.key" -out "$c/other.csr" -subj /CN=someone-else 2>/dev/null
openssl x509 -req -in "$c/other.csr" -CA "$c/ca.pem" -CAkey "$c/ca.key" -CAcreateserial -out "$c/other.pem" -days 2 -extfile "$c/agent.ext" 2>/dev/null
chmod -R a+rX "$c"

# ---- the Pod: the spawner publishes the ports, the router joins its network namespace; /sockets is one tmpfs for both, /state is the spawner's own
docker volume create --driver local --opt type=tmpfs --opt device=tmpfs --opt o=size=64m,mode=0755 "$vol" >/dev/null
docker run -d --name "$sp" --entrypoint /sup/supervisor --user 0:0 --read-only --security-opt no-new-privileges --cap-drop ALL --cap-add CHOWN --cap-add KILL --cap-add SETGID --cap-add SETUID --memory 1g --memory-swap 1g --pids-limit 1024 \
  --tmpfs /state:rw,mode=0711,size=256m -v "$vol":/sockets -v "$c":/certs:ro \
  -e CONTROL_CERT=/certs/server.pem -e CONTROL_KEY=/certs/server.key -e CONTROL_CLIENT_CA=/certs/ca.pem -e CONTROL_ALLOW_CN=regional-agent \
  -p "127.0.0.1:$PORT:9000" -p "127.0.0.1:$CPORT:9443" "$IMAGE" spawner >/dev/null || { bad "the spawner does not start"; exit 1; }
docker run -d --name "$rt" --entrypoint /sup/supervisor --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges --memory 128m --memory-swap 128m --pids-limit 256 \
  -e REQUEST_MS=30000 -v "$vol":/sockets --network "container:$sp" "$IMAGE" router >/dev/null || { bad "the router does not start"; docker logs "$sp" 2>&1 | tail -5; exit 1; }

agent=(curl -s -m 30 --cacert "$c/ca.pem" --cert "$c/agent.pem" --key "$c/agent.key")
for i in $(seq 100); do "${agent[@]}" -o /dev/null "https://127.0.0.1:$CPORT/healthz" && curl -s -o /dev/null -m 1 "http://127.0.0.1:$PORT/_healthz" && break; sleep 0.2; done
[ "$("${agent[@]}" "https://127.0.0.1:$CPORT/healthz")" = ok ] && ok "the control API answers over mutual TLS" || { bad "the control API does not answer"; docker logs "$sp" 2>&1 | tail -5; exit 1; }

# ---- the Pod's rules
[ "$(docker exec "$sp" sh -c 'grep ^CapEff /proc/1/status | cut -f2')" = 00000000000000e1 ] && ok "the spawner has CHOWN, KILL, SETGID and SETUID, and nothing else" || bad "the spawner's capabilities: $(docker exec "$sp" sh -c 'grep ^CapEff /proc/1/status')"
[ "$(docker exec "$rt" sh -c 'grep ^CapEff /proc/1/status | cut -f2')" = 0000000000000000 ] && ok "the router has no capability" || bad "the router has capabilities"
[ "$(docker exec "$rt" id -u)" = 10001 ] && ok "the router runs as user 10001" || bad "the router's user is not 10001"
docker exec "$sp" sh -c 'touch /x' 2>/dev/null && bad "the spawner's root file system is writable" || ok "the root file system of the spawner is read-only"
docker exec "$rt" sh -c 'touch /sockets/x' 2>/dev/null && bad "the router can write into /sockets" || ok "the router cannot write into the shared directory"
docker exec "$rt" sh -c 'ls /state' 2>/dev/null && bad "the router sees /state" || ok "the router has no /state"
curl -s -o /dev/null -m 5 -w '%{http_code}' --cacert "$c/ca.pem" "https://127.0.0.1:$CPORT/v1/customers" | grep -qv '^200$' && ok "no client certificate: refused" || bad "the control API answered without a client certificate"
code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' --cacert "$c/ca.pem" --cert "$c/other.pem" --key "$c/other.key" "https://127.0.0.1:$CPORT/v1/customers")"
[ "$code" = 403 ] && ok "a certificate of the right CA with another name: 403" || bad "another name: $code"
code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$CPORT/v1/customers")"
[ "$code" != 200 ] && ok "no TLS: refused ($code)" || bad "the control API answered without TLS"
[ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://127.0.0.1:$PORT/v1/customers")" != 200 ] && ok "the data port has no control API" || bad "the data port answers /v1/customers"

# ---- two customers: acme (the functions that misbehave) and globex (the neighbour), put through the control API
b64() { printf '%s' "$1" | base64 -w0; }
put() { # id, JSON body
  "${agent[@]}" -o /dev/null -w '%{http_code}' -X PUT --data "$2" "https://127.0.0.1:$CPORT/v1/customers/$1"
}
acme="{\"functions\":{\"hello\":{\"bundle\":\"$(b64 "let n = 0; export default (req, env) => new Response('hello ' + (++n) + ' ' + (env.API_KEY ?? '-'));")\"},\"loop\":{\"bundle\":\"$(b64 'export default () => { for (;;) {} };')\"},\"heap\":{\"bundle\":\"$(b64 'export default () => { const keep = []; for (;;) keep.push(new Array(100000).fill(keep.length)); };')\"}},\"secrets\":{\"API_KEY\":\"sekrit-acme\"}}"
globex="{\"functions\":{\"count\":{\"bundle\":\"$(b64 'let n = 0; export default () => new Response(String(++n));')\"}},\"secrets\":{\"API_KEY\":\"sekrit-globex\"}}"
[ "$(put acme "$acme")" = 200 ] && ok "acme is put" || bad "put acme"
[ "$(put globex "$globex")" = 200 ] && ok "globex is put" || bad "put globex"
[ "$(put broken '{"functions":{"x":{"bundle":"'"$(b64 'export default (')"'"}}}')" = 422 ] && ok "a bundle that does not load is refused at the put (422)" || bad "a broken bundle was accepted"
[ "$(put broken '{"functions":{"x":{"bundle":"'"$(b64 'x')"'"}},"uid":0}')" = 400 ] && ok "a put with an unknown field is refused (400)" || bad "an unknown field was accepted"

get() { curl -s -m "${2:-10}" -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$1"; }
body() { curl -s -m 10 "http://127.0.0.1:$PORT$1"; }
[ "$(body /acme/hello/)" = "hello 1 sekrit-acme" ] && ok "acme answers, with its own secret (a cold start)" || bad "acme hello: $(body /acme/hello/)"
[ "$(body /globex/count/)" = 1 ] && ok "the neighbour is a process of its own" || bad "the neighbour's first answer is not 1"

# ---- the wall between the two (the first customer is user 20000, the second 20001), tried from inside the spawner's container as one customer's user
workerd_uids() { docker exec "$sp" sh -c 'for p in /proc/[0-9]*; do if [ "$(cat $p/comm 2>/dev/null)" = workerd ]; then awk "/^Uid:/{print \$2}" $p/status; fi; done' | sort -u | tr '\n' ' '; }
u="$(workerd_uids)"
[ "$u" = "20000 20001 " ] && ok "the two workerd processes run as users 20000 and 20001" || bad "the users of the workerd processes: $u"
other() { docker exec --user 20000:20000 "$sp" sh -c "$1" 2>&1; }
other 'ls /sockets' | grep -qi 'permission denied' && ok "a customer cannot look into the shared sockets directory" || bad "a customer can list /sockets"
other 'ls /state' | grep -qi 'permission denied' && ok "a customer cannot list /state" || bad "a customer can list /state"
gpid=$(docker exec "$sp" sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = workerd ] && [ "$(awk "/^Uid:/{print \$2}" $p/status)" = 20001 ] && basename $p; done' | head -1)
other "kill -9 $gpid" | grep -qi 'not permitted' && ok "a customer cannot kill the neighbour's process" || bad "a customer could signal the neighbour"
other "cat /proc/$gpid/environ" | grep -qi 'permission denied' && ok "a customer cannot read the neighbour's environment (its secrets)" || bad "a customer can read the neighbour's environment"
[ "$(docker exec --user 20001:20001 "$sp" sh -c "cat /proc/$gpid/environ" | tr '\0' '\n' | grep -c 'FNSECRET_API_KEY=sekrit-globex')" = 1 ] && ok "(the neighbour itself can read it: the refusals are real)" || bad "the neighbour cannot read its own environment"
[ "$(docker exec "$sp" sh -c 'grep -rl sekrit /state /sockets 2>/dev/null | wc -l')" = 0 ] && ok "no secret is in a file in /state or /sockets" || bad "a secret is on disk"
[ "$(docker logs "$sp" 2>&1 | grep -c sekrit)" = 0 ] && ok "no secret is in the spawner's log" || bad "a secret is in the log"

# ---- a loop: stopped by the looks in about ten seconds (the router lets a request run for 30 s here, so that the request timeout does not get there first), the neighbour does not feel it, the next request is a fresh process
t0=$(date +%s)
get /acme/loop/ 30 >/dev/null
stopped=$(( $(date +%s) - t0 ))
[ "$stopped" -ge 5 ] && [ "$stopped" -le 25 ] && ok "the loop was stopped after about ${stopped} s" || bad "the loop was stopped after ${stopped} s"
docker logs "$sp" 2>&1 | grep -q 'customer=acme reason=runaway' && ok "the log says why" || bad "no runaway kill in the log"
[ "$(body /globex/count/)" = 2 ] && ok "the neighbour kept its process while acme looped" || bad "the neighbour was restarted or hurt: $(body /globex/count/)"
[ "$(body /acme/hello/)" = "hello 1 sekrit-acme" ] && ok "the next request of acme runs in a fresh process" || bad "acme after the loop: $(body /acme/hello/)"

# ---- a memory bomb: killed at the limit (256 MiB), the neighbour is not hurt
get /acme/heap/ 20 >/dev/null
for i in $(seq 40); do docker logs "$sp" 2>&1 | grep -q 'customer=acme reason=memory' && break; sleep 0.5; done
docker logs "$sp" 2>&1 | grep -q 'customer=acme reason=memory' && ok "the memory bomb was killed at the limit" || bad "no memory kill in the log"
[ "$(body /globex/count/)" = 3 ] && ok "the neighbour is still the same process after the bomb" || bad "the neighbour after the bomb: $(body /globex/count/)"
[ "$(body /acme/hello/)" = "hello 1 sekrit-acme" ] && ok "acme starts again after the bomb" || bad "acme after the bomb: $(body /acme/hello/)"

echo "memory: the spawner $(docker exec "$sp" sh -c 'grep ^VmRSS /proc/1/status' | tr -s ' \t' ' '), the router $(docker exec "$rt" sh -c 'grep ^VmRSS /proc/1/status' | tr -s ' \t' ' ') (resident, with two customers put)"

# ---- delete: gone, and nothing of the user is left
[ "$("${agent[@]}" -o /dev/null -w '%{http_code}' -X DELETE "https://127.0.0.1:$CPORT/v1/customers/acme")" = 204 ] && ok "acme is deleted" || bad "delete acme"
[ "$(get /acme/hello/)" = 404 ] && ok "a deleted customer answers 404" || bad "a deleted customer answers $(get /acme/hello/)"
[ "$(workerd_uids)" = "20001 " ] && ok "no process of acme's user is left" || bad "processes of acme's user are left: $(workerd_uids)"

[ "$(docker inspect "$sp" --format '{{.State.Running}}')" = true ] && [ "$(docker inspect "$rt" --format '{{.State.Running}}')" = true ] && ok "the spawner and the router are still running" || bad "one of them stopped"
if [ "$fail" = 0 ]; then echo "ALL OK"; else docker logs "$sp" 2>&1 | tail -20; docker logs "$rt" 2>&1 | tail -10; exit 1; fi
