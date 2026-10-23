# KoedoDB function runtime

KoedoDB runs a project's functions (Supabase-style: one HTTP handler per function) on the unmodified [workerd](https://github.com/cloudflare/workerd), one process for each customer, **with several customers in one microVM**, under a small supervisor of our own written in Go. This repository holds the supervisor, the image, and the tests.

This is not an official workerd, Cloudflare or Supabase repository, and KoedoDB is not affiliated with any of them.

## What a function is

A function is **one JavaScript file** (ESM), `index.mjs`, that a customer bundles on their own computer or in their CI (esbuild, for example), with its dependencies inside. It exports a handler by default:

```js
export default (req, env) => new Response('hello ' + new URL(req.url).pathname + ' ' + env.API_KEY);
```

The handler takes a standard `Request` and returns a `Response` (or a promise of one). An object with a `fetch` method works too (a Hono app, for one). `env.NAME` and `process.env.NAME` are the customer's secrets. A handler that throws, or does not return a `Response`, answers 500. A bundle that does not load is refused when the customer puts it (422).

A request for `/<customer>/<function>/...` goes to that function.

## The supervisor

One binary, `/sup/supervisor`, two containers of a Pod (**[docs/shared-vm.md](docs/shared-vm.md)** has all of this in detail):

- **`spawner`**: root with `CAP_CHOWN`, `CAP_KILL`, `CAP_SETGID` and `CAP_SETUID` and nothing else. The only part that holds the customers' bundles and secrets (in memory) and starts workerd, **each customer's process as a user and group of its own**, with no groups, no capabilities and `no_new_privs`, a read-only and empty directory, its listening socket handed down as a file descriptor, its secrets in its own environment only, and limits from outside. Its doors: a **control API** (HTTPS, mutual TLS) to put and delete a customer, and a Unix socket only the router's user can open.
- **`router`**: an ordinary user (10001), no capability, holds nothing. Routes `/<customer>/<function>/...`, counts, limits, answers 404, 429, 502, 503 and 504.

```
PUT /v1/customers/<id>   {"functions": {"hello": {"bundle": "<base64 index.mjs>"}}, "secrets": {"API_KEY": "…"}, "limits": {"memory_mib": 128}}
```

A put replaces the customer's whole definition and is checked first (a throwaway workerd as the customer's user): a bundle that does not load is refused with the function's name and nothing changes. A new definition is a new generation: the next request uses it and the old process finishes what it serves.

### Limits

workerd has no CPU time limit and no memory limit of its own, so they come from outside the process: the spawner looks at each process (`WD_MEMORY_LOOK_MS`: resident memory above `WD_MEMORY_MIB`, 256, is killed; `WD_RUNAWAY_LOOKS` looks of `WD_LOOK_MS` at 85 % of the CPU quota, about 10 s, is killed: SIGKILL, because a looping workerd ignores SIGTERM), `RLIMIT_NPROC` (per user, so the customer's own), no core files, and, with a delegated cgroup (`WD_CGROUP_ROOT`), the kernel enforces `memory.max`, `cpu.max` and `pids.max` too. `docs/workerd.md` says which limit comes from where.

### Memory: the LRU of resident processes

`internal/residents` keeps the customers' processes that are resident within a **memory budget** (`FN_BUDGET_MIB`, else the container's cgroup limit less `SUPERVISOR_RESERVE_MIB`). A process counts as the larger of its measured resident memory and `FN_ESTIMATE_MIB` (so a process that has just started does not look small). When a function needs room, the least recently used process that is idle is stopped (not before `FN_MIN_RESIDENCY_MS`); a customer with a request in flight is never stopped. Requests that cannot start yet wait (up to `FN_WAIT_MS`, at most `FN_MAX_WAITERS` of them), in order; then 503.

The spawner takes about 17 MiB and the router about 13 MiB resident (measured, two customers put).

## What it does not do

[docs/shared-vm.md](docs/shared-vm.md) ends with "Security properties, and what is not guaranteed". The short version: the guest's kernel is shared by every customer in the VM; there is no PID or mount namespace (that would need `CAP_SYS_ADMIN`); the network is the Pod's NetworkPolicy's job; the CPU and memory limits are looks (a cgroup makes them the kernel's); the state is in memory and the Regional Agent puts the customers again after a restart; WebSockets and long-lived streams are not tested.

## Settings

All by environment variables, in [docs/shared-vm.md](docs/shared-vm.md#settings). A value that is not a number is an error at start; the control API's certificates and the users of the customers have no default that works without them.

## The image

```
docker build -t koedodb-functions .
scripts/image-test.sh koedodb-functions
```

The image is Debian's slim base (pinned by digest), `ca-certificates` (workerd aborts at start without the system's certificates), the supervisor, and the official `workerd` binary: about 220 MiB, no Node, no npm. The binary comes from the npm package at the version and integrity hashes of `workerd/package-lock.json`, in a builder stage that is not in the image; `docker build` runs `workerd --version` so that it stops when the binary does not run on the base's glibc.

`scripts/image-test.sh` runs the image as the Pod does: two containers (the spawner as root with exactly four capabilities, `no-new-privileges` and a read-only root file system; the router as user 10001 with none), sharing a network and a tmpfs, with mutual TLS on the control API. It puts two customers, checks the wall between them (as one customer's user: the sockets directory, the state directory, the neighbour's environment and process), a loop, a memory bomb, and a delete.

To update workerd, change `workerd/package.json`, run `npm install --package-lock-only --ignore-scripts` there, and read the [release notes](https://github.com/cloudflare/workerd/releases) and [security advisories](https://github.com/cloudflare/workerd/security/advisories) first.

## Tests

```
go vet ./... && go test -race ./...          # unit tests; the integration tests are skipped without root and a workerd
scripts/test-in-docker.sh                    # all of them, as the Pod runs the code (needs docker)
```

`scripts/test-in-docker.sh` builds the tests with the race detector and runs them in the image, as root with `--cap-drop ALL --cap-add CHOWN --cap-add KILL --cap-add SETGID --cap-add SETUID`, a read-only root file system and `no-new-privileges`: 60 tests, among them the wall between two customers (fourteen things one customer cannot do to another, each refused by the kernel), a hundred customers in one VM (no wrong answer, the pool making room), secrets (not on disk, not in the log, not in a command line), the control API (no certificate, another CA, another name, TLS 1.2, no TLS), the internal socket, puts that do not load, a new generation draining the old, delete, a loop stopped in about ten seconds with the neighbour never feeling it, a memory bomb, an overrun, an idle stop. The unit tests need no root: the settings, the limits, the route, the configuration, the router against a fake spawner, and the LRU.

## License

Copyright (c) 2026 RadarWorks LLC. The files in this repository (the supervisor, the tests and this documentation) are licensed under the [MIT License](LICENSE). The image holds the unmodified official `workerd` binary (Apache-2.0, from the npm package, at the version and integrity hashes of `workerd/package-lock.json`) and Debian's slim base image with its `ca-certificates` package.
