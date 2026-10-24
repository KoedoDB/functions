# Limits of one function

A customer's functions run in **one workerd process** (one Worker per function). The open-source workerd has no limit for a single Worker: a loop in one function stops the customer's other functions, and a memory bomb takes the whole process. KoedoDB builds workerd with a small patch (`workerd/patches/`, about 860 lines, the upstream release is in `workerd/UPSTREAM`) that adds a `limits` field to a Worker. The supervisor writes it for every function.

| Limit | Default | Maximum | What happens when it is reached |
|---|---|---|---|
| `heap_mb` (V8 old generation) | 256 | 512 | the request that filled the heap ends with 500; the process lives on |
| `cpu_ms` (CPU time of one request, thread CPU clock, summed over the runs of JavaScript) | 1000 | 5000 | 500 after about that time; the isolate is resumed for the next request |
| `wall_ms` (wall-clock time of one request) | 8000 | 9000 | 500 (the router gives up on a request after `REQUEST_MS`, 10 s, and kills the process: the function's limit must come first) |
| `external_mb` (ArrayBuffer and typed arrays) | 128 | 512 | the allocation fails with a `RangeError` the function can catch |
| `subrequests` (per request: fetch, WebSocket, cache, `connect()`) | 50 | 1000 | `Too many subrequests.` |
| `concurrent_subrequests` (open at once, over all requests of the function) | 32 | 256 | `Too many concurrent subrequests.` |

A put may set any of them for a function (`"functions": {"name": {"bundle": "…", "limits": {"cpu_ms": 200}}}`); a field left out is the default (never "no limit"); a value above the maximum is refused (400). The defaults and maximums are the supervisor's settings (`FN_*`, `FN_MAX_*`, [shared-vm.md](shared-vm.md)).

## Around it: the outer wall

The customer's **whole process** stays inside a cgroup (`memory.max` 1 GiB, `cpu.max`, `pids.max`) when the Pod gives the spawner one, and the spawner's looks (resident memory above `WD_MEMORY_MIB`, nearly all of the CPU quota for `WD_RUNAWAY_LOOKS` looks, about 10 s) kill a process that a function's own limit did not stop. That kills the customer's other functions too (and only theirs: other customers never feel it).

## TurboFan is off

`TerminateExecution`, which the CPU, wall-time and heap limits use to stop a function, cannot interrupt every loop TurboFan compiles. `for (;;) new Array(1000).fill(0)` ran for ever with `cpu_ms` at 1000, and as a heap bomb (`keep.push(new Array(1000).fill(0))`) the heap limit was reached 47 times while the process grew to 800 MB, because the termination never arrived. With `--no-turbofan` (Ignition, Sparkplug and Maglev stay) the same loops stop at the limit, so the supervisor passes it to every workerd (`WD_V8_FLAGS`, `none` for no flag). The price is the speed of long compute (see the numbers below); a request handler mostly waits for I/O.

## Tests

`internal/spawner/fnlimits_test.go` (needs the patched binary: `WORKERD_BIN`, run by `scripts/test-in-docker.sh`). Every test makes **one** function misbehave while another function of the same customer and a function of another customer are called all the time; it checks the answer, that the process is still the same process (nothing was killed from outside), and that the neighbours never failed.

Last run (2026-10-24, workerd 1.20261023.1 + the patch, in the test container: root, four capabilities, `--memory 3g`, on a 4-core build machine; `ok` for all of `internal/spawner`, 60+ tests with `-race`):

| One function does this | Result | Time | Its other function, and the other customer |
|---|---|---|---|
| `for (;;) {}` with `cpu_ms` 150 | 500 | 156 ms | slowest answer 156 ms (it waits for the one thread), the other customer 4 ms, no failures |
| a loop of `await Promise.resolve()` | 500 | 156 ms | the same |
| an allocating loop (`new Array(1000).fill(0)`), a catastrophic regular expression, a sort of 30 million numbers | 500 | 155 to 171 ms | the same |
| `for (;;) {}` with the default `cpu_ms` (1000) | 500 | 1.006 s | slowest 1.006 s, no failures |
| a timer that outlives `wall_ms` 600 | 500 | 617 ms | 12 ms, no failures |
| a heap bomb (small objects), `heap_mb` 64 | 500 | 939 ms | the process is the same one, the others never failed |
| arrays kept by a loop, `heap_mb` 64 | 500 | 339 ms | the same |
| ArrayBuffers (8 MiB each), `external_mb` 32 | the function catches `RangeError` after 24 MiB | 14 ms | 15 ms, no failures |
| 40 `fetch` in a row with `subrequests` 5 | 5 went out, 35 refused | 12 ms | no failures |
| 40 `fetch` at once with `concurrent_subrequests` 4 | 36 refused | 9 ms | no failures |
| 200 `connect()` with `concurrent_subrequests` 4 (control: 256) | 4 returned, 196 threw (control: 200 returned) | | no failures |
| a WebAssembly memory bomb | 500 at the function's `cpu_ms` (filling memory is a busy loop too) | 1.0 s | no failures |

In every case the customer's workerd process is **the same process** afterwards (nothing was killed from outside, the spawner's log has no kill), the function answers again, and a new request to the other function and to the other customer's function is served. The "slowest answer" column is the one cost: workerd runs the customer's functions on one thread, so a looping function holds that customer's other functions up for up to its `cpu_ms`; **other customers are other processes and never wait** (3 to 10 ms).

Without TurboFan's interrupt checks (`WD_V8_FLAGS=none`), `new Array(1000).fill(0)` in a loop is **not** stopped by `cpu_ms` or `heap_mb`; the outer wall kills the process after about 9 to 12 s, and only that customer's process (also tested). That is why TurboFan is off.

**Speed with TurboFan off** (same binary, a 4-core i5, one request at a time): `fib(32)` 37 ms and 39 ms, 20 rounds of `JSON.parse(JSON.stringify())` of 20000 objects 120 ms and 120 ms, an empty request about the same; **a tight numeric loop of 3·10⁸ additions 0.76 s with TurboFan and 1.73 s without** (2.3 times). Not measured: a real application, throughput under load.

## What is not covered

- **WebAssembly memory is not counted** by `external_mb` (V8 allocates it outside the ArrayBuffer allocator). A 128 MiB `WebAssembly.Memory` is created with `external_mb` at 32. The outer wall stops a bomb. (workerd refuses to compile Wasm from bytes at run time, so a function can only get a module the platform bundled.)
- **`connect()` of `cloudflare:sockets`** goes through the same counter as `fetch` in this build: it stops at `concurrent_subrequests` (tested). It is not counted separately and WebSockets and the cache API were read, not run.
- A bug in workerd or V8 that runs native code in the process gives the attacker that process: **all functions and secrets of the same customer**. The wall to other customers is the user id, the socket and the VM ([shared-vm.md](shared-vm.md)), not these limits.
- The heap limit counts V8's old generation only; new space, code and Wasm are outside it. A function that fills them is stopped by the outer wall.
- Side channels (timing) are not addressed.
- `cpu_ms` is thread CPU time, garbage collection included; `wall_ms` ends at the request's end (`waitUntil` work after it is not counted).
- A limit that is reached answers 500 `Internal Server Error`; the reason is only in the log.
- One thread runs all of a customer's functions, so a looping function holds the others up for up to its `cpu_ms` (measured above).
- Other loops V8 cannot be interrupted in may exist; the outer wall is the answer to them. Only the cases above were run.
