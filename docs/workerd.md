# How the functions run

The supervisor runs a customer's functions on the **unmodified** [workerd](https://github.com/cloudflare/workerd), **one workerd process for each customer**, all of that customer's functions
(Workers) in it. It is the only runtime. The limits that workerd does not have are applied from outside the process.

## How it works

The supervisor is two parts, a spawner and a router, for a VM that holds several customers: [shared-vm.md](shared-vm.md). In short:

- A customer is put through the spawner's control API: its functions (one `index.mjs` each), its secrets and its limits, all held in the spawner's memory. A throwaway workerd, as the customer's user, checks that
  the bundles load before anything changes (a bundle that does not load is refused, 422, naming the function).
- On the customer's first request the spawner writes a configuration (`internal/wdconfig`) and starts `workerd serve` as the customer's user, with a clean environment (only the secrets, as `FNSECRET_<NAME>`), a router
  Worker that sends `/<function>/<rest>` to the function's Worker as `/<rest>`, and a Worker for each function that wraps the bundle. The bundle's contract is a default export that is a function
  `(request, env) => Response`, or an object with a `fetch` method; `env` and `process.env` carry the customer's secrets (`fromEnvironment` bindings). The listening socket is made by the spawner and given to
  workerd as file descriptor 3.
- A bundle is imported at the first request, not when workerd starts: a bundle with no default export, or that throws when it loads, answers 500. A bundle that does **not load** (a syntax error, a module that is not
  there) would stop workerd at its start and take the customer's other functions with it. There is no other JavaScript parser in the image, so workerd is the check: at the put, a throwaway workerd with the same configuration,
  no secrets and no request but one to a name that is no function (so none of the customer's code runs); it must still be there a moment after it answered.
- One workerd process serves all of a customer's functions: a request that runs over `REQUEST_MS` (504), or a process that dies (502), costs the customer's other requests in flight too. The next request starts
  a fresh process (cold start about 170 ms, measured). A process with no request for `IDLE_MS` is stopped; processes within the memory budget are an LRU (`internal/residents`).

## The limits, and where each comes from

workerd has no CPU time limit and no memory limit of its own (the limit enforcer of the open-source server does nothing). So:

| Limit | With a cgroup (`WD_CGROUP_ROOT`) | Without one (a hardened Pod today) |
|---|---|---|
| Memory (`WD_MEMORY_MIB`, 256) | `memory.max` and no swap: the kernel stops it at once | the supervisor reads the resident memory every `WD_MEMORY_LOOK_MS` (250) and kills above the limit. **A bomb can overshoot between two looks** (a process fills hundreds of MiB in a second); the Pod's own memory limit is the last line, and the kernel kills the largest process of the Pod, which is workerd when the Pod holds one customer |
| CPU (`WD_CPU_QUOTA_PCT`, 100) | `cpu.max`: a loop cannot use more than the quota | the Pod's CPU limit is the quota of the whole Pod |
| A loop | `WD_RUNAWAY_LOOKS` (5) looks of `WD_LOOK_MS` (2 s) at 85% of the quota or more: killed with SIGKILL (a looping workerd does not see SIGTERM) after about **10 s**, using one core meanwhile | the same rule, from `/proc/<pid>/stat` |
| Processes (`WD_PIDS`, 64) | `pids.max` | the Pod's limit |
| Own user id (`UID_BASE`) | each customer runs as `UID_BASE + n` (the spawner is root with `CAP_SETUID` and `CAP_SETGID`) | the same: it does not depend on the cgroup |

Measured with this layout on a host (systemd's units and cgroups instead of the supervisor's): 100 customers, 3 hours, about 24.5 MiB of cgroup memory for each running workerd (an idle one is about 50 MiB resident,
15 MiB in its cgroup), cold start 167 ms median, a memory bomb stopped at the 256 MiB cgroup limit within 1.5 s, hostile neighbours never felt by the others.

## What the spawner needs from the guest

The spawner is root in its own container with four capabilities, `CAP_CHOWN`, `CAP_KILL`, `CAP_SETGID` and `CAP_SETUID`, and no more. The guest of a microVM (`crates/agent/src/spec.rs` of `containerd-cloudhypervisor`) allows
these four in its list of capabilities a container may have (the list has no `CAP_SYS_ADMIN`, `CAP_MKNOD` or `CAP_SYS_PTRACE`) and sets `no_new_privs` on every container, which the tests run with. Pod admission (the KoedoDB
workload policy) is where root and these four capabilities have to be allowed for the platform's own Functions Pod and for nothing else.

What the guest does **not** give a container, and the spawner does not need: a cgroup file system (so no sub-cgroups: the looks are the limit, see the table), a user namespace, `CAP_SYS_ADMIN`.

**Checked on 2026-10-23 in a Pod of the old kind in a microVM** (non-root user 10001, all capabilities dropped, read-only root file system): `/sys/fs/cgroup` is an empty directory, `/proc/self/cgroup` shows only the
container's own cgroup (`0::/<id>`), `/proc/mounts` has `sysfs ... ro` and no cgroup file system, `CapEff`, `CapPrm` and `CapBnd` are all 0, `mkdir /sys/fs/cgroup/x` fails, `max_user_namespaces` is 0.

For the kernel to enforce memory, CPU and processes per customer, the guest's agent can hand the spawner a **delegated cgroup v2 directory** (a small feature of the agent: a child cgroup below the container's own,
with `cgroup.procs`, `cgroup.threads` and `cgroup.subtree_control` writable by the container and the controller files of that cgroup itself kept root-owned, so that it cannot raise its own limit); `WD_CGROUP_ROOT` is then that directory.
That code path is unit-tested against a directory and has **not** run on a real delegated cgroup in a guest yet.

## Open points

- The cgroup code path (`WD_CGROUP_ROOT`: `memory.max`, `cpu.max`, `pids.max`, a process cloned straight into its cgroup) has not run on a real delegated cgroup yet.
- workerd's outbound `fetch` is workerd's default (public addresses). The network policy of the Pod is what keeps a function away from other Pods and the node.
- WebSockets and long-lived streams are not tested.
- The image is about 220 MiB: Debian's slim base, the CA certificates, the supervisor (about 5 MB) and the official workerd binary (about 135 MB). Nothing else: no Node, no npm.
