// Supabase's background-task API: answer now, finish the work after the answer.
Deno.serve((req) => {
  const ms = Number(new URL(req.url).searchParams.get('ms') ?? 3000);
  (globalThis as any).EdgeRuntime.waitUntil((async () => { await new Promise((r) => setTimeout(r, ms)); console.log('WAITUNTIL-DONE ' + ms); })());
  return new Response('accepted', { status: 202 });
});
