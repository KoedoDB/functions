# Functions runtime

KoedoDB runs a project's Supabase-style functions (`Deno.serve(handler)` in `index.ts`) with **plain [Deno](https://deno.com/)** and a small supervisor of its own. The Deno image is upstream's, pinned by digest; this directory holds the supervisor (about 100 lines of TypeScript), the tests, and what the tests and the measurements showed.

This is not an official Deno or Supabase repository, and KoedoDB is not affiliated with either.

## Why plain Deno, and not Supabase's edge-runtime

Supabase runs its Edge Functions on [`supabase/edge-runtime`](https://github.com/supabase/edge-runtime) (MIT). It is built for many customers in one process, with a memory limit per worker and a virtual file system. KoedoDB gives each project its own Pod (and gVisor), and wants newer libraries than Supabase offers, so it runs a current Deno instead. Same file, run in both (the latest edge-runtime, v1.77.4, 2026-10-01, reports "compatible with Deno 2.1.4"):

| | edge-runtime v1.77.4 | Deno 2.9.7 (this repository) |
|---|---|---|
| V8, TypeScript | 11.6, 5.1.6 | 15.0, 6.0.3 |
| `RegExp.escape`, `Error.isError`, `Math.sumPrecise`, `Uint8Array.fromBase64` | no | yes |
| `node:sqlite`, zstd in `node:zlib` | no | yes |
| `await import('npm:zod@4')` (a dynamic import) | refused | works |
| `import 'npm:zod@4'` (static), hono, supabase-js, openai, stripe, drizzle-orm, jose | works | works |
| Promise.try, Iterator helpers, Set methods, Array.fromAsync, Object.groupBy, Temporal, URLPattern, Ed25519 | yes | yes |
| AES-GCM refuses a forged authentication tag | yes | yes |
| a limit of memory per worker, a virtual file system | **yes** | no (see "Known limits") |

## The image

The file `IMAGE` holds the image, by tag and by digest of the multi-architecture index:

```
denoland/deno:2.9.7@sha256:fa335acdf6b72106eda2cb6a8cb5f4187e7630e357467489db4b2e7352d5e432
```

To update it, change the tag and the digest together (`docker buildx imagetools inspect denoland/deno:<tag>`), then run `tests/run.sh`. Read the [release notes](https://github.com/denoland/deno/releases) and the [security advisories](https://github.com/denoland/deno/security-advisories) first (Deno has had 30; many are about its permission flags on other platforms).

## What a function looks like

```
/f/<name>/index.ts      →  served at  /<name>/<anything>?<query>
```

```ts
// /f/hello/index.ts: a Supabase-style function, as it is
Deno.serve((req: Request) => new Response('hello ' + new URL(req.url).pathname));
```

The path inside the function is what follows the name (`/hello/a/b` is `/a/b` inside). A name is lower case letters, digits and `-`, 40 characters at most.

## How it is run

```sh
docker run --user 10001:10001 --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
  --memory 512m --cpus 1 -e DENO_DIR=/tmp/deno -e FN_ENV_ALLOW=API_KEY -e API_KEY=... \
  -v ./functions:/f:ro -v ./supervisor:/sup:ro <IMAGE> \
  run --no-prompt --allow-net --allow-read=/f,/tmp,/cache --allow-write=/tmp --allow-run --allow-env /sup/supervisor.ts
```

The supervisor is trusted platform code. It listens on 9000, and **runs every function in its own Deno process**, started on the function's first request and killed after it has been idle. The process listens on a Unix socket (`DENO_SERVE_ADDRESS`), so two functions never fight for a port. A function process gets a clean environment, and permissions only for its own directory, its socket and its scratch directory: `--allow-net`, `--allow-read=<dir>,<sock>,<scratch>`, `--allow-write=<sock>,<scratch>`, `--allow-env=<the allowed names>`, no `--allow-run`, no `--allow-ffi`, no `--allow-sys`.

| Setting (environment of the supervisor) | Default | What it does |
|---|---|---|
| `FN_ENV_ALLOW` | none | Comma-separated names of the variables a function may read (the project's function secrets). Nothing else is passed on. |
| `IDLE_MS` | 30000 | A function process with no request for this long is killed. |
| `REQUEST_MS` | 10000 | A request that takes longer is cancelled (504) and **the function process is killed** (every request running in it fails). |
| `HEAP_MB` | 192 | The V8 heap limit of each function process (for a Pod of 512 MiB). **Set it** (see "Memory"). |
| `START_MS` | 30000 | How long to wait for a process to start listening. |
| `CACHED_ONLY` | off | `1`: a function process never downloads a module (`--cached-only`). |

`GET /_stats` lists the functions that are running. Every answer has an `x-fn-ms` header (milliseconds in the supervisor, including a cold start).

### Warm the cache

A heavy function (drizzle-orm pulls in dozens of npm packages) takes 5-7 seconds to start with a cold module cache, and then fails if the start is waited for only 5 seconds. At deploy time, fill the cache; at run time, forbid downloads:

```sh
docker run --rm -e DENO_DIR=/cache -v ./cache:/cache -v ./functions:/f:ro <IMAGE> cache /f/<name>/index.ts   # deploy time
# run time: -e DENO_DIR=/cache -e CACHED_ONLY=1 -v ./cache:/cache:ro   (and --allow-read=/cache for the supervisor)
```

With the warmed (read-only) cache the drizzle function answers its first request in about 0.2 s; a module that is not in the cache fails in about 40 ms (502) instead of being downloaded.

## Known limits (the tests record them)

- **A literal `import('file:///…')` ignores `--allow-read`.** Deno loads an import whose specifier is a string literal (or a concatenation of literals) when it builds the module graph, with no read check, and a file that is in the graph that way can then be imported by a computed specifier too. The error message of the import shows part of the first line of the file (about 80 characters around the syntax error): `/etc/passwd`, the start of `/proc/1/environ` (the Pod's environment) and of the supervisor's. A computed specifier for a file that no literal names is checked and refused. The author of a function writes the literal, so the process cannot stop it. **The wall is the Pod (and gVisor): one Pod holds one project, and only secrets that project's functions may read.** edge-runtime stops this with its virtual file system.
- **The network is not restricted by Deno.** A function can reach `127.0.0.1`, the node, other Pods. NetworkPolicy has to keep it away from other projects, the node and the metadata address.
- **Memory:** see below.
- Not supported by the supervisor, and not tested: WebSocket and server-sent-event functions (the supervisor proxies with `fetch`).

## Memory

- **Set the V8 heap limit per function process (`HEAP_MB`).** Left to Deno, every process takes about half of the container's memory limit as its own limit, so the processes of one Pod (the supervisor and its functions) add up to more than the Pod has, and the kernel kills one before V8 stops it: with the supervisor and two functions in a 512 MiB Pod, six runs of a function that fills its heap ended six times in a kernel kill, and once the Pod itself was killed. With `HEAP_MB=192`, V8 stops only that function (`Reached heap limit`, 502) and the kernel kills nothing: 8 of 8 runs. (Deno 2.4.1 needed `DENO_USE_CGROUPS=1` to see the container's limit at all; 2.9.7 sees it by default.)
- **Memory outside of the V8 heap (`Uint8Array`, `ArrayBuffer`, buffers of a response) is not capped.** A function that eats it up to the Pod's limit is killed by the kernel, which kills the biggest process, usually that function; sometimes it kills the supervisor and the container stops, and the kubelet starts it again. `oom_score_adj` is not used: it only chooses who dies, the Pod is out of memory either way. A hard limit per function needs a Pod (or cgroup) per function, or edge-runtime's per-worker limit.
- Measured (one laptop, Docker): resident memory per function process: nothing but `Deno.serve` 36 MiB, hono 43, jose 49, zod 4 50, supabase-js 69, drizzle 93 (179 while loading); six functions and the supervisor in one Pod: 234 MiB in the cgroup, of which the supervisor itself is 55 MiB.

## gVisor

`tests/bench-gvisor.sh` runs the same small work natively and under `runsc do` (rootless, systrap): V8 compute is the same speed (116 ms against 111 ms), file system calls about 5 times slower, HTTP over a Unix socket about 2.6 times slower, memory allocation about 2 times slower, and the memory seems about 25% larger. These are two runs on a laptop, not Kubernetes with gVisor.

## What the tests check

`tests/run.sh` (61 checks) runs the pinned image with the supervisor, hardened (uid 10001, read-only root, `--cap-drop ALL`, memory 512 MiB), and checks:

- routing: the path, the query, the method, the headers and the body (also 5 MB), the status of the answer, 404, bad names;
- the life cycle: nothing runs before the first request, a cold start, idle stop, restart, a function that exits;
- permissions: its own environment variable and no other, files, `/proc`, writing, programs, FFI, system information are refused; the known limits above are checked as they are, so a change is noticed;
- limits: an endless loop or a sleep is cancelled (504) and the other functions do not feel it, a 200 MB answer, a filled heap (8 of 8 with `HEAP_MB`), buffers eaten up to the limit (the Pod serves again within 30 s);
- the warm cache: a heavy function answers at once from a read-only cache, an uncached module fails at once;
- modern JavaScript: the versions, 17 language and runtime features, AES-GCM, and 7 npm packages imported dynamically.

It needs Docker, Python 3 and curl, and the network (the npm packages), and uses no secrets: the "secrets" are made up for the run.

## Not checked

gVisor in Kubernetes, NetworkPolicy, the Secret mount, WebSocket and server-sent events, a slow memory leak next to heavy functions, and speed under load (requests per second, p99) against edge-runtime.

## License

Copyright (c) 2026 RadarWorks LLC. The files in this repository (`IMAGE`, the supervisor, the tests and this documentation) are licensed under the [MIT License](LICENSE). Deno is MIT-licensed too; the image is upstream's.
