// Loaded (--preload) into every function process before the function's own code. It gives a Supabase function the one global it may call that plain Deno does not have:
//
//   EdgeRuntime.waitUntil(promise)   keep working after the response has been sent
//
// A Deno process lives on after a response, so an un-awaited promise runs to its end by itself. What the shim adds is (1) the name, (2) a failing task is logged and does not
// bring the process down (an uncaught rejection ends a Deno process, and with it every request that is running in it), and (3) the number of tasks that are still running is
// written to a file the supervisor reads, so that it does not stop the process as idle while a task is still working (up to MAX_BACKGROUND_MS).
const address = Deno.env.get('DENO_SERVE_ADDRESS') ?? '';
const scratch = address.startsWith('unix:') ? address.slice(5).replace(/\.sock$/, '') : '';   // /tmp/fn-<name>, the function's own scratch directory
const pending = new Set<Promise<unknown>>();
const publish = () => { if (scratch) { try { Deno.writeTextFileSync(`${scratch}/.pending`, String(pending.size)); } catch { /* the supervisor will see no file: nothing pending */ } } };
(globalThis as Record<string, unknown>).EdgeRuntime = {
  waitUntil(task: unknown) {
    const tracked: Promise<unknown> = Promise.resolve(task)
      .catch((error) => console.error('EdgeRuntime.waitUntil: the task failed:', error))
      .finally(() => { pending.delete(tracked); publish(); });
    pending.add(tracked);
    publish();
  },
};
