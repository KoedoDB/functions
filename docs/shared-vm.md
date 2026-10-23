# Several customers in one VM

One microVM (one Pod of two containers) runs the functions of **many customers**. This page is how the supervisor keeps them apart, what it needs from the Pod, the API the platform uses to put a customer's functions into it, and, at the end, what it does **not** guarantee.

The wall between two customers in the VM is made of ordinary Unix parts, each of them small and tested: **a user id and a group id of the customer's own** (and nothing else shared), **a socket nobody else can reach**, **secrets that are in the memory of the one process that needs them**, and **limits the customer's process cannot change**. The wall between the VM and the rest of the platform is the VM itself.

## The pieces

```
        Envoy (App Host)                       Regional Agent
              │ http :9000                          │ https :9443, mutual TLS
   ┌──────────▼───────────────────────────────────────▼────────────────── one VM, one Pod ──┐
   │  router  (user 10001, no capability)            spawner  (root, 4 capabilities)        │
   │   - routes /<customer>/<function>/…             - holds bundles and secrets (memory)   │
   │   - counts, limits, 429/503/504                 - starts workerd as the customer's user│
   │   - holds nothing                               - looks at limits, kills               │
   │        │  unix: /sockets/spawner.sock  (acquire, release)  ▲                           │
   │        └────────────────────────────────────────────────────┘                          │
   │        │  unix: /sockets/w-<customer>.<gen>.<n>.sock                                   │
   │   ┌────▼────┐ ┌─────────┐ ┌─────────┐                                                  │
   │   │workerd  │ │workerd  │ │workerd  │   one process per customer, user 20000, 20001, …  │
   │   │ acme    │ │ globex  │ │ initech │   (no groups, no capabilities, no_new_privs)      │
   │   └─────────┘ └─────────┘ └─────────┘                                                  │
   └────────────────────────────────────────────────────────────────────────────────────────┘
```

One binary, `/sup/supervisor`, three modes: `spawner`, `router` and `child` (what a customer's process is for a moment before it becomes workerd). The two containers use the same image.

- **spawner** is the only privileged part and the only part that knows a bundle or a secret. It runs as root with `CAP_CHOWN`, `CAP_KILL`, `CAP_SETGID` and `CAP_SETUID` and **nothing else** (the test suite runs with exactly these four; the spawner refuses to start without them and logs a warning if it has more). It has two doors: the **control API** (HTTPS, mutual TLS, for the Regional Agent) and the **internal socket** (a Unix socket only the router's user can connect to).
- **router** runs as an ordinary user (10001) with no capability and holds no bundle, no secret and no authority over customers. It faces the edge, routes, counts and limits. If it were compromised it could start and stop customers' processes and talk to their sockets, which is all it can reach.
- **workerd** is the unmodified official binary, one process for each customer, **all of the customer's functions inside it** (each function is a Worker of that process). It is started on the customer's first request and stopped when idle or to make room (`internal/residents`: a least-recently-used pool under a memory budget).

## What a customer's process gets

Everything below is what the spawner arranges **before workerd runs**; a customer's code never has the chance to do it differently.

| | |
|---|---|
| **user and group** | `UID_BASE + n` for customer n, group the same number, **no supplementary groups** (`setgroups` with an empty list), set by the kernel at `fork`. Never 0, never the router's user. |
| **capabilities, new privileges** | none (the user change from root clears them), and `no_new_privs` is set by the child before it executes workerd: a setuid binary or a file capability cannot give more. The child refuses to run as root, with a group, or with any capability. |
| **its socket** | the spawner makes the listening socket (`/sockets/w-<customer>.<generation>.<n>.sock`, `root:<router's group>`, mode 0660) and hands it to workerd as **file descriptor 3** (`workerd serve --socket-fd http=3`). The directory is `root:<router's group>`, mode 0750, so **no customer can even look into it**: it cannot connect to another customer's socket, and cannot take a path away. The socket is private (0700 root) until it has been given to the group, so there is no moment in which another customer could connect. |
| **its files** | a run directory `root:<its group>`, mode 0750, with the configuration and the bundles as `root:<its group>` 0440: **it can read, never write**. Both are **deleted as soon as workerd answers** (it has compiled them): the directory it keeps is empty. The parent directories are 0711, so a customer can reach its own directory and cannot list anything. |
| **its secrets** | in the **environment of its own process only**, under the name `FNSECRET_<NAME>` (so a secret called `LD_PRELOAD` is a plain binding for the function and never the dynamic loader's variable), and delivered to the function as a workerd binding (`fromEnvironment`). Not in a file, not on a command line, not in the log, not in the configuration. Only the process's own user can read `/proc/<pid>/environ`; the spawner, with its four capabilities, cannot (the tests check this). |
| **limits** | memory (RSS above `WD_MEMORY_MIB`, looked at every `WD_MEMORY_LOOK_MS`) and CPU ("nearly all of the quota for `WD_RUNAWAY_LOOKS` looks of `WD_LOOK_MS`", about 10 s) enforced by the spawner's looks with SIGKILL of the process group; `RLIMIT_NPROC` (a user's processes and threads: the customer's own, since users are not shared), `RLIMIT_CORE` 0, `RLIMIT_MEMLOCK` 0, `RLIMIT_NOFILE` 4096; and, when the spawner has a writable cgroup v2 directory (`WD_CGROUP_ROOT`), a cgroup of its own with `memory.max`, `cpu.max` and `pids.max`, into which the process is cloned directly (no moment outside of the limits). The limits are the platform's maximum; a put may ask for less, never more. |
| **its network** | workerd's `fetch` allows public addresses only (the tests check that `127.0.0.1`, `::1`, the router's port and the control API's port are refused). **The Pod's NetworkPolicy is what keeps a function away from the node and other Pods**: see "What the Pod must provide". |

When the **last process** of a customer has gone (it was stopped, it was killed, the customer was deleted), the spawner kills **everything else that runs as that user** (`killAllOf`), and a user id is given to another customer only when the customer is deleted **and** nothing of the user is left. If a process would not die, the number is kept back for good and logged.

## Putting a customer's functions (the control API)

HTTPS with **mutual TLS and nothing else**: TLS 1.3, a client certificate signed by `CONTROL_CLIENT_CA` whose common name is in `CONTROL_ALLOW_CN`. There is no plain mode and no token; without the certificates the spawner does not start. A certificate of the right CA with another name gets 403.

```
PUT    /v1/customers/<id>
       {"functions": {"hello": {"bundle": "<base64 of index.mjs>"}},
        "secrets":   {"API_KEY": "…"},
        "limits":    {"memory_mib": 128, "cpu_quota_pct": 50, "pids": 32}}      limits are optional
DELETE /v1/customers/<id>                    204, or 404 if there is no such customer
GET    /v1/customers                         [{"id","generation","functions":[…],"secrets":<count>,"running":bool}]  (never the values)
GET    /v1/stats                             the pool's statistics
GET    /healthz
```

- **A put replaces the customer's whole definition** (all functions, all secrets, the limits): it is idempotent, and the Agent can send the same put again. The answer is the new `generation`.
- **The body is exactly a definition**: unknown fields, a second JSON value, base64 that is not standard, a name that is not `[a-z0-9-]{1,40}` (customer or function), a secret name that is not `[A-Za-z_][A-Za-z0-9_]{0,63}`, a secret over 8 KiB or with a NUL byte, more than `MAX_FUNCTIONS` (50) functions, a bundle over `MAX_BUNDLE_BYTES` (5 MiB), more than `MAX_SECRETS` (64) secrets, limits above the platform's maximum: all are refused (400, 413), and nothing changes. Everything held (bundles and secrets of all customers) is capped by `MAX_STATE_BYTES` (256 MiB, 507 beyond it), customers by `MAX_CUSTOMERS`/`UID_COUNT` (507); a few puts at a time (429 beyond, with `Retry-After`).
- **The bundles are checked before anything changes**: a throwaway workerd, as the customer's user, with the same configuration and no secrets, no request that reaches the customer's code (a bundle is imported when its first request comes). If a bundle does not load, the put is refused with **422** and `{"error": "…", "functions": {"<name>": "does not load"}}`: the customer keeps its running version and, for a new customer, nothing is left behind (no user id, no directory). A bundle that is the same as the customer's current one is not checked again. A probe that uses more memory than the limit while compiling is killed (written, not tested: no small bundle makes workerd's parser use that much).
- **A new generation drains the old one.** Processes are named `<customer>.<generation>`. After a put, new requests start (or use) the new generation's process; the old generation's process finishes what it serves and is stopped when nothing is in flight (checked every second). For a moment a customer may have two processes (the pool counts both).
- **A delete kills at once**: every process of the customer's user, every generation, then the user id is freed.
- **The state is in memory only.** If the spawner container restarts, it knows no customer: requests get 404 until the Regional Agent puts them again. The Agent compares `GET /v1/customers` with what should be there (reconcile) after the Pod becomes ready and on a timer, and puts what is missing or older.

## The internal socket (the router's door)

`/sockets/spawner.sock`, `root:<router's group>`, 0660, **and** a check of the peer's user id (`SO_PEERCRED`) for every connection: any user but `ROUTER_UID` is closed (a second wall behind the permissions of the directory).

```
POST /v1/acquire?customer=<id>&function=<name>   200 {"lease","socket","functions"}  | 404 no such customer or function | 503 busy (Retry-After) | 502 did not start
POST /v1/release?lease=<id>&outcome=ok|timeout|broken   204   (timeout and broken: the process is killed)
GET  /v1/ping
```

`acquire` starts nothing for a function that is not there (so a request for a name that is no function cannot be used to start processes). A lease that the router never releases (it crashed) is released after `LEASE_TTL_MS`. The router asks for a lease for each request and releases it when the response is done, with `timeout` if it had to cancel the request (an overrun kills the customer's process: the next request starts a fresh one) and `broken` if the process failed it.

## What the Pod must provide

- **Two containers, one image**, `/sup/supervisor spawner` and `/sup/supervisor router`, in the **same Pod** (the same VM).
- **spawner**: `runAsUser: 0`, `allowPrivilegeEscalation: false`, `capabilities: {drop: [ALL], add: [CHOWN, KILL, SETGID, SETUID]}`, `readOnlyRootFilesystem: true`, seccomp `RuntimeDefault`; volumes `/state` (an emptyDir, `medium: Memory`, only this container mounts it: the run directories, a few MiB at a time), `/sockets` (an emptyDir, `medium: Memory`, shared with the router) and the certificates (a Secret: the server certificate and key and the client CA, only this container). Ports 9443 (the control API). **No hostPath, no hostNetwork, no extra capability, no `SYS_ADMIN`/`DAC_OVERRIDE`/`SYS_PTRACE`.**
- **router**: `runAsUser: 10001`, `runAsGroup: 10001`, `runAsNonRoot: true`, `capabilities: {drop: [ALL]}`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`; `/sockets` only (no `/state`, no certificates). Port 9000.
- **Users**: customers use `UID_BASE` (20000) up to `UID_BASE + UID_COUNT` (1000 numbers) and the router 10001: no other process of the VM may use these numbers (the image has nothing else in it). The guest has no user namespace.
- **The VM's memory** is the budget: `FN_BUDGET_MIB` is the Pod's memory limit less `SUPERVISOR_RESERVE_MIB` (60). A customer's process is about 50 MiB idle and counts as at least `FN_ESTIMATE_MIB`; at 100 customers in a 1.5 GiB budget the pool keeps about 27 resident and stops the least recently used for the next (measured in the tests).
- **NetworkPolicy** (outside the VM, on the Pod): ingress to 9000 from the edge only and to 9443 from the Regional Agent only; **egress to the public internet only** (not the node, not other Pods, not the metadata address, not the cluster's ranges). workerd's own default refuses private addresses, but that is a second line, not the first.
- **Probes**: `GET :9000/_healthz` (the router asks the spawner) is both liveness and readiness. A ready Pod may still be empty: the Regional Agent puts the customers.
- `LEASE_TTL_MS` (20 s) must be longer than the router's `REQUEST_MS` (10 s).
- `WD_CGROUP_ROOT` is set when the guest's agent hands the spawner a delegated cgroup v2 directory: the code is in and tested against a directory; it has **not** been tried on a real delegated cgroup in a guest yet.

## Security properties, and what is not guaranteed

What the tests check (`internal/spawner/integration_test.go`, run as root with exactly the four capabilities and a read-only root file system; `scripts/image-test.sh` checks the same in the built image with two containers):

- A customer's process runs as its own user and group, with no supplementary group, no capability, `no_new_privs`, `RLIMIT_CORE` 0, `RLIMIT_MEMLOCK` 0, `RLIMIT_NPROC`, a run directory it cannot write and that is empty.
- **As one customer's user**, each of these fails with a kernel permission error: list the sockets directory; connect to another customer's socket; connect to the spawner's socket; list the state directory; list another customer's run directory or read a file in it; read another customer's `/proc/<pid>/environ`, `maps` or `fd`; `kill -9` or `kill -STOP` another customer's process; `chown`, `chmod` or write into another customer's directory. The same commands succeed for the owner (so the refusals are not an accident of the commands).
- Each customer answers with its own identity and its own secret; at **100 customers** (2 rounds, 200 starts, 173 evictions) there were **no wrong answers**, no two customers shared a user id or a process.
- Secrets are not in any file under `/state` or `/sockets`, not in the log, not in a command line, not in the output of a refused put; the process's environment has them under `FNSECRET_`; a secret named `LD_PRELOAD` does not reach the loader.
- A function cannot reach `127.0.0.1`, `::1`, the router's port or the control API's port with `fetch` (a refusal or an error status, never a 200).
- A loop is killed in about 10 s and a memory bomb at the limit, **while the neighbour answers every request**; an overrun is a 504 and a fresh process; an idle customer is stopped and, with it, whatever else ran as its user; a delete leaves no process of the user, no directory, no socket, and the user id is given again.
- The control API refuses: no client certificate, a certificate of another CA, another common name (403), TLS 1.2, no TLS. The internal socket refuses root and any customer's user.
- A put that does not load is refused with the function's name and changes nothing; a new generation is served from the next request and the old process finishes its request first.

**What is not guaranteed** (and what to do about it):

- **The guest's kernel is shared by all customers in the VM.** A kernel vulnerability reachable from a workerd process (an unprivileged user's system calls) is a way across the wall. This is the cost of putting several customers in one VM; the VM's kernel is the minimal one cchv ships (no modules, no `io_uring`, no `userfaultfd`), kept up to date, and the Pod's seccomp profile narrows the system calls. A customer whose code needs more than this wall gets a VM of its own (an App).
- **No PID or mount namespace and no `hidepid`.** Making them needs `CAP_SYS_ADMIN`, which the spawner does not have on purpose. A customer's process (if workerd itself were exploited) can list `/proc` and read the command line of every process: `workerd serve --socket-fd http=3 /state/run/<customer>.<gen>.<n>/config.capnp`, which names the other customers, but holds no secret. It can read world-readable files of the image.
- **The router can reach every customer's socket and the spawner's internal socket**: it is trusted platform code. A function is a public URL: the router does not authenticate callers. Reaching the router is the edge's job and the NetworkPolicy's.
- **workerd's `fetch` is not a firewall.** The Pod's NetworkPolicy is.
- **The CPU limit is a look**: a process that stays under 85 % of its quota is never killed, and one above it for 10 s is; between two looks it may use a whole core. With a delegated cgroup the kernel enforces `cpu.max` as well.
- **The memory limit overshoots between two looks** (250 ms): a bomb fills hundreds of MiB in a second. The VM's own limit is the last line, and the guest's OOM killer may then choose a different customer's process. With a delegated cgroup the kernel enforces `memory.max` per customer.
- **Compiling a bundle runs workerd's parser as the customer's user in the VM**: a parser bug is in the same class as the shared kernel.
- **Secrets live in the spawner's memory and in the process's environment**, in a VM whose memory the host can read: there is no encryption at rest or in memory (the platform's decision).
- **A customer can see that it is not alone** (`/proc`, user ids), and **a flood by one customer** is bounded by the router's per-function (`MAX_CONCURRENT`), per-customer (`CUSTOMER_CONCURRENT`) and total (`MAX_INFLIGHT`) limits and the pool's `FN_WAIT_MS`/`FN_MAX_WAITERS`, not by a quota of calls: the edge counts and bills calls.
- **State is lost when the spawner restarts**; the Regional Agent has to put the customers again.
- **Not tried**: a real cchv VM (the tests run in a container with the same capabilities and read-only root; a VM's `/proc`, cgroup and kernel differ), a delegated cgroup in a guest (`WD_CGROUP_ROOT`), WebSockets, requests of more than a few MiB.

## Settings

All by environment variables; a value that is not a number is an error at start, and the settings that make the isolation have no "off".

### spawner

| Variable | Default | Meaning |
|---|---|---|
| `CONTROL_DIR` | `/sockets` | the directory shared with the router (`root:ROUTER_GID` 0750; its contents are removed at start) |
| `STATE_DIR` | `/state` | the spawner's own scratch (run directories; its contents are removed at start) |
| `WORKERD_BIN` | `/sup/workerd` | the workerd executable (every user must be able to run it) |
| `CHILD_BIN` | the spawner's own executable | what becomes workerd (`<CHILD_BIN> child …`) |
| `WD_COMPAT_DATE` | `2026-10-01` | the compatibility date of every function |
| `ROUTER_UID`, `ROUTER_GID` | 10001, 10001 | who may connect to the internal socket; the group of the sockets |
| `UID_BASE`, `UID_COUNT` | 20000, 1000 | the users of the customers (at least 1000: never root or a system user; not the router's) |
| `CONTROL_LISTEN` | `:9443` | the control API |
| `CONTROL_CERT`, `CONTROL_KEY`, `CONTROL_CLIENT_CA` | **required** | the server certificate and key, and the CA of the client certificates |
| `CONTROL_ALLOW_CN` | **required** | the common names that may call (comma-separated) |
| `IDLE_MS` | 30000 | stop a process with no request for this long |
| `START_MS` | 30000 | how long to wait for a process to answer |
| `LEASE_TTL_MS` | 20000 | release a lease the router has not released |
| `FN_BUDGET_MIB` | the cgroup limit less the reserve, else 1024 | memory the processes may use together |
| `SUPERVISOR_RESERVE_MIB` | 60 | what the two supervisors take |
| `FN_ESTIMATE_MIB`, `FN_MIN_RESIDENCY_MS`, `FN_WAIT_MS`, `FN_MAX_WAITERS` | 50, 1000, 5000, 20 | the pool (`internal/residents`) |
| `MAX_CUSTOMERS`, `MAX_FUNCTIONS`, `MAX_SECRETS`, `MAX_BUNDLE_BYTES`, `MAX_STATE_BYTES`, `MAX_PROBES` | 1000, 50, 64, 5 MiB, 256 MiB, 4 | what a put may hold |
| `WD_MEMORY_MIB`, `WD_CPU_QUOTA_PCT`, `WD_PIDS` | 256, 100, 64 | the platform's maximum limits of one customer's process (a put may ask for less) |
| `WD_LOOK_MS`, `WD_RUNAWAY_LOOKS`, `WD_MEMORY_LOOK_MS` | 2000, 5, 250 | the looks |
| `WD_CGROUP_ROOT` | | a writable cgroup v2 directory handed down to the spawner |

### router

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:9000` | address |
| `CONTROL_DIR` | `/sockets` | the shared directory |
| `REQUEST_MS` | 10000 | cancel a request (504) and have its process killed |
| `ACQUIRE_MS` | 40000 | how long to wait for the spawner (room, a start) |
| `MAX_CONCURRENT` | 20 | requests one function serves at once (429 beyond) |
| `CUSTOMER_CONCURRENT` | 40 | requests one customer is served at once, all its functions (429) |
| `MAX_INFLIGHT` | 400 | requests in all (503 with `Retry-After`) |

The answers the router gives itself: 400 a bad name, 404 no such customer or function, 429 too many for a function or a customer, 502 the process did not start or died or the spawner does not answer, 503 busy (no room within `FN_WAIT_MS`, or `MAX_INFLIGHT`), 504 a request took longer than `REQUEST_MS` (its process is killed; the next request starts a new one). Headers that start with `X-Koedodb-` are removed from a request, so that the edge's own cannot be forged, and `X-Forwarded-For` is not passed on.
