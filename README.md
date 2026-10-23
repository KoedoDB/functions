# KoedoDB function runtime

KoedoDB runs a project's functions (Supabase-style: one HTTP handler per function) on the unmodified [workerd](https://github.com/cloudflare/workerd), one process for each customer, with a small supervisor of its own, written in Go. This repository holds the supervisor, the image, and the tests.

This is not an official workerd, Cloudflare or Supabase repository, and KoedoDB is not affiliated with any of them.

## What a function is

A function is **one JavaScript file** (ESM), `index.mjs`, that a customer bundles on their own computer or in their CI (esbuild, for example), with its dependencies inside. It exports a handler by default:

```js
export default (req) => new Response('hello ' + new URL(req.url).pathname);
```

The handler takes a standard `Request` and returns a `Response` (or a promise of one). An object with a `fetch` method works too (a Hono app, for one). A handler that throws, or does not return a `Response`, answers 500. A bundle that does not load stops its process, and the supervisor answers 502.

The bundles are in a directory, `<FUNCTIONS_DIR>/<name>/index.mjs`. A request for `/<name>/...` goes to that function.

## The supervisor

`cmd/supervisor` listens on `:9000` and runs **one workerd process for each customer**, with all of the customer's functions in it, started on the customer's first request and stopped when it has been idle (`IDLE_MS`) or to make room for another. workerd listens on a Unix socket (a short path: 107 bytes at most). The supervisor is trusted platform code; the functions are not. A customer's process gets:

- a clean environment: the variables a function may read (`FN_ENV_ALLOW`, and a customer's own `secrets.json`) are in its configuration and nowhere else;
- limits applied from outside, because workerd has no CPU time limit and no memory limit of its own: its resident memory is looked at every `WD_MEMORY_LOOK_MS` and it is killed above `WD_MEMORY_MIB` (256), and a process that uses nearly all of its CPU quota for `WD_RUNAWAY_LOOKS` looks of `WD_LOOK_MS` (about 10 s) is killed too (SIGKILL: a looping workerd ignores SIGTERM); with a cgroup (`WD_CGROUP_ROOT`) the kernel enforces memory, CPU and processes as well. `docs/workerd.md` says which limit comes from where, and what a hardened Pod can and cannot do;
- no core files: the supervisor sets `RLIMIT_CORE` to 0.

The answers the supervisor gives itself:

| Status | When |
|---|---|
| 404 | no such function (a bad name, or no bundle) |
| 429 | a function already serves `MAX_CONCURRENT` requests (with `Retry-After`) |
| 503 | there is no room within `FN_WAIT_MS` to start the customer's process (with `Retry-After`) |
| 502 | the process did not start or died, or the bundle does not load |
| 504 | a request took longer than `REQUEST_MS`; the customer's process is killed (the next request starts a new one) |

### Memory: the LRU of resident processes

`internal/residents` keeps the customers' processes that are resident within a **memory budget** (`FN_BUDGET_MIB`, else the container's cgroup limit less `SUPERVISOR_RESERVE_MIB`). A process counts as the larger of its measured resident memory and `FN_ESTIMATE_MIB` (so a process that has just started does not look small). When a function needs room, the least recently used process that is idle is stopped (not before `FN_MIN_RESIDENCY_MS`); a function with a request in flight is never stopped. Requests for functions that cannot start yet wait (up to `FN_WAIT_MS`, at most `FN_MAX_WAITERS` of them), in order; then 503.

The supervisor itself takes about 10 MiB.

## Settings

All by environment variables; a value that is not a number is an error at start.

| Variable | Default | Meaning |
|---|---|---|
| `FUNCTIONS_DIR` | `/f` | where the bundles are |
| `CONTROL_DIR` | `/sockets` | sockets and scratch directories of the processes (a tmpfs of their own) |
| `LISTEN` | `:9000` | address |
| `IDLE_MS` | 30000 | stop a process with no request for this long |
| `REQUEST_MS` | 10000 | cancel a request (504) and kill its process |
| `START_MS` | 30000 | how long to wait for a process to listen |
| `MAX_CONCURRENT` | 20 | requests one function serves at once |
| `FN_ENV_ALLOW` | | comma-separated names a function may read |
| `FN_BUDGET_MIB` | cgroup limit less the reserve | memory the processes may use together |
| `SUPERVISOR_RESERVE_MIB` | 30 | what the supervisor takes of the Pod |
| `FN_ESTIMATE_MIB` | 50 | what a process counts as before it is measured |
| `FN_MIN_RESIDENCY_MS` | 1000 | a process is not stopped to make room before this |
| `FN_WAIT_MS` | 5000 | how long a request waits for room |
| `FN_MAX_WAITERS` | 20 | how many may wait |
| `WORKERD_BIN` | `/sup/workerd` | the workerd executable (the image has the official release) |
| `CUSTOMERS_DIR` | | `<dir>/<customer>/<function>/index.mjs`, routes `/<customer>/<function>/...`; empty: one customer, `default`, with `FUNCTIONS_DIR` |
| `WD_COMPAT_DATE` | `2026-10-01` | the compatibility date of every function |
| `WD_MEMORY_MIB` | 256 | a process above this resident memory is killed (and `memory.max` of its cgroup) |
| `WD_CPU_QUOTA_PCT` | 100 | the CPU a process may use, % of one core (`cpu.max`; and what "nearly all" means to the runaway rule) |
| `WD_LOOK_MS` | 2000 | one look at the CPU |
| `WD_RUNAWAY_LOOKS` | 5 | looks in a row at 85% of the quota or more before the process is killed (5 x 2 s = about 10 s) |
| `WD_MEMORY_LOOK_MS` | 250 | how often the resident memory is read |
| `WD_PIDS` | 64 | `pids.max` of the cgroup |
| `WD_CGROUP_ROOT` | | a writable cgroup v2 directory; each customer's process gets a cgroup in it |
| `WD_UID_BASE`, `WD_UID_COUNT` | 0, 4096 | workerd: customer n runs as `WD_UID_BASE + n` (the supervisor must be root); 0: no switch |

## The image

```
docker build -t koedodb-functions .
scripts/image-test.sh koedodb-functions
```

The image is Debian's slim base (pinned by digest), `ca-certificates` (workerd aborts at start without the system's certificates), the supervisor, and the official `workerd` binary: about 220 MiB, no Node, no npm. The binary comes from the npm package at the version and integrity hashes of `workerd/package-lock.json`, in a builder stage that is not in the image; `docker build` runs `workerd --version` so that it stops when the binary does not run on the base's glibc.

Run it as the Pod does: a non-root user (10001), a read-only root file system, no capabilities. **The only writable place is a tmpfs at `/sockets`** (the sockets, configurations and scratch directories of the customers' processes; the supervisor makes the directories below it, so the tmpfs must be writable by user 10001: `uid=10001,mode=0700`). The bundles are read-only, at `/f` (one customer) or below `CUSTOMERS_DIR` (many). `scripts/image-test.sh` runs the image like that and tries a loop and a memory bomb.

To update workerd, change `workerd/package.json`, run `npm install --package-lock-only --ignore-scripts` there, and read the [release notes](https://github.com/cloudflare/workerd/releases) and [security advisories](https://github.com/cloudflare/workerd/security/advisories) first.

## What the limits do not do

- **The network is not restricted by workerd.** A function can reach `127.0.0.1`, the node, other Pods. NetworkPolicy has to keep it away from other projects, the node and the metadata address.
- **A memory bomb can overshoot** between two looks at the resident memory (a process fills hundreds of MiB in a second); the Pod's own limit is the last line.
- WebSocket and server-sent events are not supported (the supervisor proxies whole HTTP exchanges); not tested.
- **The wall is the Pod** (a microVM): one Pod holds one project today, and only secrets that project's functions may read. Several customers in one VM need a cgroup and a user id for each (`docs/workerd.md`).

## Tests

```
go vet ./... && go test -race ./...
```

With `WORKERD_BIN` (or `workerd` on the PATH) the tests that run a real workerd run too (the official binary: `cd workerd && npm ci --ignore-scripts`); they are skipped without it. They check, among others: routing (path, query, method, headers, body), the life cycle (nothing runs before the first request, idle stop, restart), a clean environment, the check of the bundles (a bad bundle is left out and the customer's other functions run), the limits (a loop is stopped by the looks and the neighbour never feels it, a memory bomb is killed at the limit, 429, 504, 503 with `Retry-After`), the settings, and the LRU (budget, order, in-flight processes are never stopped, waiting and the queue limit). `scripts/image-test.sh` tests the image.

## License

Copyright (c) 2026 RadarWorks LLC. The files in this repository (the supervisor, the tests and this documentation) are licensed under the [MIT License](LICENSE). The image holds the unmodified official `workerd` binary (Apache-2.0, from the npm package, at the version and integrity hashes of `workerd/package-lock.json`) and Debian's slim base image with its `ca-certificates` package.
