# The workerd runtime

`RUNTIME=workerd` runs a customer's functions on the **unmodified** [workerd](https://github.com/cloudflare/workerd), **one workerd process for each customer**, all of that customer's functions
(Workers) in it. The limits that workerd does not have are applied from outside the process. The default, `RUNTIME=node`, is unchanged: a Node process for each function (README).

## How it works

- The functions are in `<FUNCTIONS_DIR>/<function>/index.mjs` (one customer: `default`), or, with `CUSTOMERS_DIR`, in `<CUSTOMERS_DIR>/<customer>/<function>/index.mjs` and the routes are
  `/<customer>/<function>/...`. A customer's secrets are the variables named in `FN_ENV_ALLOW` and, with customers, the keys of `<customer>/secrets.json` on top of them.
- On the first request of a customer the supervisor writes a configuration (`BuildWorkerdConfig`) and starts `workerd serve` with a clean environment: a router Worker that sends
  `/<function>/<rest>` to the function's Worker as `/<rest>`, and a Worker for each function that wraps the bundle. The bundle has the same contract as in the Node runtime (a default export that is a function
  `(request) => Response`, or an object with a `fetch` method), and `process.env` carries the allowed variables. workerd listens on a Unix socket; the supervisor proxies to it.
- A bundle is imported at the first request, not when workerd starts: a bundle with no default export, or that throws when it loads, answers 500. A bundle that does **not parse** would stop workerd
  at its start and take the customer's other functions with it, so with Node present each bundle is parsed first (`node --check`) and one that does not parse is left out of the configuration and answers 502.
- One workerd process serves all of a customer's functions: a request that runs over `REQUEST_MS` (504), or a process that dies (502), costs the customer's other requests in flight too. The next request starts
  a fresh process (cold start about 170 ms, measured). A process with no request for `IDLE_MS` is stopped; processes within the memory budget are an LRU, as in the Node runtime (`internal/residents`).
- The answers are the Node runtime's: 404, 429 above `MAX_CONCURRENT` for a function, 503 with `Retry-After` when there is no room, 502, 504, and the `x-fn-ms` header.

## The limits, and where each comes from

workerd has no CPU time limit and no memory limit of its own (the limit enforcer of the open-source server does nothing). So:

| Limit | With a cgroup (`WD_CGROUP_ROOT`) | Without one (a hardened Pod today) |
|---|---|---|
| Memory (`WD_MEMORY_MIB`, 256) | `memory.max` and no swap: the kernel stops it at once | the supervisor reads the resident memory every `WD_MEMORY_LOOK_MS` (250) and kills above the limit. **A bomb can overshoot between two looks** (a process fills hundreds of MiB in a second); the Pod's own memory limit is the last line, and the kernel kills the largest process of the Pod, which is workerd when the Pod holds one customer |
| CPU (`WD_CPU_QUOTA_PCT`, 100) | `cpu.max`: a loop cannot use more than the quota | the Pod's CPU limit is the quota of the whole Pod |
| A loop | `WD_RUNAWAY_LOOKS` (5) looks of `WD_LOOK_MS` (2 s) at 85% of the quota or more: killed with SIGKILL (a looping workerd does not see SIGTERM) after about **10 s**, using one core meanwhile | the same rule, from `/proc/<pid>/stat` |
| Processes (`WD_PIDS`, 64) | `pids.max` | the Pod's limit |
| Own user id (`WD_UID_BASE`) | each customer runs as `WD_UID_BASE + n` (the supervisor must be root) | not possible: the Pod has no `CAP_SETUID` |

Measured with this layout on a host (systemd's units and cgroups instead of the supervisor's): 100 customers, 3 hours, about 24.5 MiB of cgroup memory for each running workerd (an idle one is about 50 MiB resident,
15 MiB in its cgroup), cold start 167 ms median, a memory bomb stopped at the 256 MiB cgroup limit within 1.5 s, hostile neighbours never felt by the others.

## Can the supervisor make cgroups and switch users inside a hardened Pod?

**Not today, and the policy is not to weaken for it.** A Pod of ours runs as a non-root user with every capability dropped, a read-only root file system and no privilege escalation, and the guest that
runs the containers of a microVM builds the container like this (`crates/agent/src/spec.rs` of `containerd-cloudhypervisor`):

- the mounts are the guest's own list: `/proc`, `/dev`, `/dev/pts`, `/dev/shm` and `/sys` as a **read-only sysfs**. There is **no cgroup file system** in the container (`/sys/fs/cgroup` is an empty directory of the
  sysfs), and a volume cannot be mounted below `/sys` or `/proc`;
- the namespaces are `mount` and `pid` only: no cgroup namespace and no user namespace (`user.max_user_namespaces` is 0);
- the container's own cgroup is made with the Pod's memory, CPU and pids limits (so the Pod's limits are enforced), but nothing in it is writable by the container;
- the capabilities are the Pod's, cut to a short list; the Pod drops all of them, so there is no `CAP_SETUID`, `CAP_SETGID` or `CAP_SYS_ADMIN`.

**Checked on 2026-10-23 in a Pod of that kind in a microVM** (non-root user 10001, all capabilities dropped, read-only root file system): `/sys/fs/cgroup` is an empty directory, `/proc/self/cgroup` shows only the
container's own cgroup (`0::/<id>`), `/proc/mounts` has `sysfs ... ro` and no cgroup file system, `CapEff`, `CapPrm` and `CapBnd` are all 0, `mkdir /sys/fs/cgroup/x` fails, `max_user_namespaces` is 0.

So inside such a Pod the supervisor cannot create sub-cgroups, move a process into one, or run a process as another user. What does work is everything this document calls "without a cgroup": the Pod's own
limits, and the supervisor's looks at `/proc`. **For a Pod that holds one customer (the model of this repository today) that is complete**: the Pod is the customer's cgroup.

For a VM that holds several customers, one of three things (with their risks):

1. **(a) A second container of the Pod that is privileged inside the VM** (never on the host): it would need `CAP_SYS_ADMIN` and `CAP_SETUID`, which the admission policy forbids and the guest's allow list does not
   contain, and a container with `CAP_SYS_ADMIN` in a VM that also runs customers' code can attack the guest kernel. Needs a policy exception, a larger guest attack surface. **Not recommended.**
2. **(b) A small cgroup service in the guest agent** (root in the VM, already PID 1): the supervisor asks over a Unix socket for "a child cgroup with these limits" and for "start this process in it as
   uid N"; the agent checks with `SO_PEERCRED` that the caller is in a container of the Pod and that the cgroup is below the caller's own cgroup (a child can only subdivide what its parent has, so no escape),
   and returns a directory file descriptor (`WD_CGROUP_ROOT` is then that directory). Small, and the privilege stays in the code we already trust as PID 1, but it is new code in the agent and a new interface to review.
   **This is what `WD_CGROUP_ROOT` and `WD_UID_BASE` are ready for.** Recommended for a VM with several customers.
3. **(c) The VM boundary, the Pod's limits and the supervisor's looks only** (what is implemented): no new privilege. Customers in one VM share the guest kernel, the memory of the VM and one uid; the memory
   rule can overshoot, one customer's bomb can make the guest's OOM killer choose another customer's process, and a loop costs up to 10 s of a core. Fine for a Pod with one customer; **not enough for
   several customers in one VM**. `RLIMIT_AS` cannot help (V8 reserves address space), `RLIMIT_NPROC` is per user id (one uid for all), `RLIMIT_CPU` counts a process's whole life.

## Open points

- The cgroup code path (`WD_CGROUP_ROOT`: `memory.max`, `cpu.max`, `pids.max`, a process cloned straight into its cgroup) is unit-tested against a directory and has not run on a real delegated cgroup yet; do that once (b) exists.
- Customers in one VM need per-customer secrets and the uid switch (`WD_UID_BASE`, root); the secrets file is read by the supervisor and is in the configuration, which only the supervisor and workerd can read.
- workerd's outbound `fetch` is workerd's default (public addresses). The network policy of the Pod is still what keeps a function away from other Pods and the node.
- WebSockets and long-lived streams are not tested.
- The image holds both runtimes (the official workerd binary is about 140 MB): an image for one runtime only is a build argument away if the size matters.
