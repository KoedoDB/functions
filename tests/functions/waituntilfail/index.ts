// A background task that fails, and a task that is not even a promise.
Deno.serve(() => {
  (globalThis as any).EdgeRuntime.waitUntil((async () => { await new Promise((r) => setTimeout(r, 300)); throw new Error('WAITUNTIL-TASK-FAILED'); })());
  (globalThis as any).EdgeRuntime.waitUntil(undefined);
  return new Response('accepted', { status: 202 });
});
