// What can customer code do? Every probe reports what happened instead of failing.
async function attempt(f: () => unknown | Promise<unknown>) {
  try { const v = await f(); return { ok: true, value: typeof v === 'string' ? v.slice(0, 200) : v }; }
  catch (e) { return { ok: false, error: String(e).slice(0, 2500) }; }
}
Deno.serve(async (req: Request) => {
  const t = new URL(req.url).searchParams.get('t');
  const probes: Record<string, () => unknown | Promise<unknown>> = {
    readPasswd: () => Deno.readTextFile('/etc/passwd'),
    readProcEnviron: () => Deno.readTextFile('/proc/self/environ'),
    listRoot: async () => { const n: string[] = []; for await (const e of Deno.readDir('/')) n.push(e.name); return n.join(','); },
    listFunctions: async () => { const n: string[] = []; for await (const e of Deno.readDir('/f')) n.push(e.name); return n.join(','); },
    readOtherFunction: () => Deno.readTextFile('/f/hello/index.ts'),
    readSupervisor: () => Deno.readTextFile('/sup/supervisor.ts'),
    writeTmp: async () => { await Deno.writeTextFile('/tmp/x', 'x'); return 'wrote /tmp/x'; },
    writeRoot: async () => { await Deno.writeTextFile('/x', 'x'); return 'wrote /x'; },
    envAll: () => JSON.stringify(Deno.env.toObject()),
    spawn: async () => { const o = await new Deno.Command('/bin/ls', { args: ['/'] }).output(); return new TextDecoder().decode(o.stdout); },
    importPasswd: () => import('file:///etc/passwd'),
    importPid1Environ: () => import('file:///proc/1/environ'),                 // a string literal: loaded when the module graph is built, no read check
    importComputed: () => import('file://' + ['', 'etc', 'hostname'].join('/')), // computed, and no literal import of this file anywhere in the module: checked at run time, refused
    importSiblingFunction: () => import('file:///f/hello/index.ts'),
    ownEnv: () => Deno.env.get('GREETING'),
    otherEnv: () => Deno.env.get('SUPERVISOR_ONLY_SECRET'),
    exit: () => Deno.exit(1),
    kill: () => Deno.kill(1, 'SIGKILL'),
    fetchExternal: async () => (await fetch('https://example.com', { signal: AbortSignal.timeout(5000) })).status,
    fetchLoopback9000: async () => (await fetch('http://127.0.0.1:9000/hello', { signal: AbortSignal.timeout(5000) })).status,
    connectDocker: async () => { const c = await Deno.connect({ hostname: '172.17.0.1', port: 22, transport: 'tcp' }); c.close(); return 'connected'; },
    listen: () => { const l = Deno.listen({ port: 9999 }); l.close(); return 'listened'; },
    hostname: () => Deno.hostname(),
    osRelease: () => Deno.osRelease(),
    memoryUsage: () => Deno.memoryUsage(),
    ffi: () => (Deno as any).dlopen('/lib/x86_64-linux-gnu/libc.so.6', {}),
    evalCode: () => eval('1+1'),
    wasm: () => typeof WebAssembly.instantiate,
    cwd: () => Deno.cwd(),
    uid: () => (Deno as any).uid?.(),
  };
  if (!t || !probes[t]) return new Response(JSON.stringify(Object.keys(probes)), { status: 400 });
  return new Response(JSON.stringify(await attempt(probes[t])), { headers: { 'content-type': 'application/json' } });
});
