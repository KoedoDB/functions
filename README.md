# KoedoDB function runtime

KoedoDB runs a project's functions (Supabase-style: one HTTP handler per function) with **Node.js** and a small supervisor of its own, written in Go. This repository holds the supervisor, the runner it starts for each function, the image, and the tests.

This is not an official Node.js or Supabase repository, and KoedoDB is not affiliated with either.

## What a function is

A function is **one JavaScript file** (ESM), `index.mjs`, that a customer bundles on their own computer or in their CI (esbuild, for example), with its dependencies inside. It exports a handler by default:

```js
export default (req) => new Response('hello ' + new URL(req.url).pathname);
```

The handler takes a standard `Request` and returns a `Response` (or a promise of one). An object with a `fetch` method works too (a Hono app, for one). A handler that throws, or does not return a `Response`, answers 500. A bundle that does not load stops its process, and the supervisor answers 502.

The bundles are in a directory, `<FUNCTIONS_DIR>/<name>/index.mjs`. A request for `/<name>/...` goes to that function.

## The supervisor

`cmd/supervisor` listens on `:9000` and **runs every function in a Node process of its own**, started on the function's first request and stopped when it has been idle (`IDLE_MS`) or to make room for another. The process listens on a Unix socket (a short path: 107 bytes at most), so two functions never fight for a port. It is trusted platform code; the functions are not. A function process gets:

- a clean environment: its socket, and only the variables named in `FN_ENV_ALLOW` (the project's secrets), nothing else;
- Node's permission model (`--permission`): it reads its own directory, the runner, `/tmp` and its own scratch directory, and writes `/tmp` and its scratch directory; no child process, no worker, no native addon;
- a V8 heap limit (`--max-old-space-size`, `HEAP_MB`);
- no core files: the supervisor sets `RLIMIT_CORE` to 0 (a function that runs out of heap aborts, and its core file would be written into the Pod's memory).

The answers the supervisor gives itself:

| Status | When |
|---|---|
| 404 | no such function (a bad name, or no bundle) |
| 429 | a function already serves `MAX_CONCURRENT` requests (with `Retry-After`) |
| 503 | there is no room within `FN_WAIT_MS` to start the process (with `Retry-After`) |
| 502 | the process did not start, or died |
| 504 | a request took longer than `REQUEST_MS`; its process is killed |

### Memory: the LRU of resident processes

`internal/residents` keeps the processes that are resident within a **memory budget** (`FN_BUDGET_MIB`, else the container's cgroup limit less `SUPERVISOR_RESERVE_MIB`). A process counts as the larger of its measured RSS and `FN_ESTIMATE_MIB` (so a process that has just started does not look small). When a function needs room, the least recently used process that is idle is stopped (not before `FN_MIN_RESIDENCY_MS`); a function with a request in flight is never stopped. Requests for functions that cannot start yet wait (up to `FN_WAIT_MS`, at most `FN_MAX_WAITERS` of them), in order; then 503.

The supervisor itself takes about 10 MiB.

## Settings

All by environment variables; a value that is not a number is an error at start.

| Variable | Default | Meaning |
|---|---|---|
| `FUNCTIONS_DIR` | `/f` | where the bundles are |
| `CONTROL_DIR` | `/sockets` | sockets and scratch directories of the processes (a tmpfs of their own) |
| `RUNNER` | `/sup/runner.mjs` | the runner |
| `NODE_BIN` | `node` | the node executable |
| `LISTEN` | `:9000` | address |
| `IDLE_MS` | 30000 | stop a process with no request for this long |
| `REQUEST_MS` | 10000 | cancel a request (504) and kill its process |
| `START_MS` | 30000 | how long to wait for a process to listen |
| `HEAP_MB` | 144 | V8 old-space limit of each process (3/8 of a 384 MiB Pod) |
| `MAX_CONCURRENT` | 20 | requests one function serves at once |
| `FN_ENV_ALLOW` | | comma-separated names a function may read |
| `FN_BUDGET_MIB` | cgroup limit less the reserve | memory the processes may use together |
| `SUPERVISOR_RESERVE_MIB` | 30 | what the supervisor takes of the Pod |
| `FN_ESTIMATE_MIB` | 105 | what a process counts as before it is measured |
| `FN_MIN_RESIDENCY_MS` | 1000 | a process is not stopped to make room before this |
| `FN_WAIT_MS` | 5000 | how long a request waits for room |
| `FN_MAX_WAITERS` | 20 | how many may wait |

## The image

```
docker build -t koedodb-functions .
```

Both base images (`golang`, `node:24-bookworm-slim`) are pinned by digest. Run it as the Pod does: a non-root user (10001), a read-only root file system, no capabilities; the bundles in `/f` (read-only), a tmpfs at `/sockets`, and a disk `emptyDir` at `/tmp` (the functions' own scratch space, shared by the functions of the Pod).

To update Node, change the tag and the digest together, and read its [release notes](https://github.com/nodejs/node/releases) and [security advisories](https://github.com/nodejs/node/security/advisories) first.

## What the permission model does not do

- **The network is not restricted by Node.** A function can reach `127.0.0.1`, the node, other Pods. NetworkPolicy has to keep it away from other projects, the node and the metadata address.
- **Memory outside of the V8 heap** (`Buffer`, `ArrayBuffer`) is not capped by `HEAP_MB`; the container's limit and the budget are what stop it.
- WebSocket and server-sent events are not supported (the supervisor proxies whole HTTP exchanges); not tested.
- **The wall is the Pod** (a VM under Kata Containers): one Pod holds one project, and only secrets that project's functions may read.

## Tests

```
go vet ./... && go test -race ./...
```

They need Node (24) on the machine; a test is skipped without it. They check, among others: routing (path, query, method, headers, body), the life cycle (nothing runs before the first request, idle stop, restart), a clean environment, the permission model, the limits (endless loop → 504 and the others do not feel it, 429, 503 with `Retry-After`), a function over its heap limit dies and the next request is served by a new process, no core file is written, and the LRU (budget, order, in-flight processes are never stopped, waiting and the queue limit).

## License

Copyright (c) 2026 RadarWorks LLC. The files in this repository (the supervisor, the runner, the tests and this documentation) are licensed under the [MIT License](LICENSE). Node.js is MIT-licensed too; the image holds the unmodified official `node` image.
