// KoedoDB function runtime: routes /<name>/... to the function in /f/<name>/index.ts.
//
// A function is a plain Supabase-style file: `Deno.serve(handler)` in /f/<name>/index.ts. Each function runs in its own Deno process, started on its first request and stopped when
// it has been idle. The process listens on a Unix socket (DENO_SERVE_ADDRESS), so two functions never fight for a port. This file is trusted platform code; the functions are not.
//
// Settings (environment of the supervisor):
//   FN_ENV_ALLOW  comma-separated names of the variables a function may read (the Project's function secrets); nothing else is passed on
//   IDLE_MS       stop a function process after this long without a request (default 30000)
//   REQUEST_MS    a request that takes longer is cancelled and its function process is killed (default 10000)
//   HEAP_MB       V8 old-space limit of each function process (default 192, for a Pod of 512 MiB). Set it: left to Deno, every process takes about half of the Pod's memory limit as its own
//                 limit, so the processes of one Pod add up to more than the Pod has, and the kernel kills one before V8 stops it. It does NOT cover memory outside the V8 heap
//                 (ArrayBuffers): see the README, "Memory".
//   START_MS      how long to wait for a function process to listen (default 30000); a function whose modules are not cached yet can take several seconds
//   CACHED_ONLY   1: a function process never downloads a module (--cached-only). The cache is warmed at deploy time (README, "Warm the cache"); a module that is not in it fails at once
const FUNCTIONS = '/f';
const IDLE_MS = Number(Deno.env.get('IDLE_MS') ?? 30000);
const REQUEST_MS = Number(Deno.env.get('REQUEST_MS') ?? 10000);
const HEAP_MB = Number(Deno.env.get('HEAP_MB') ?? 192);   // 0: leave it to Deno
const START_MS = Number(Deno.env.get('START_MS') ?? 30000);
const CACHED_ONLY = Deno.env.get('CACHED_ONLY') === '1';
const ALLOWED_ENV = (Deno.env.get('FN_ENV_ALLOW') ?? '').split(',').map((s) => s.trim()).filter(Boolean);

type Fn = { child: Deno.ChildProcess; client: Deno.HttpClient; last: number; ready: Promise<void> };
const running = new Map<string, Fn>();

async function waitForSocket(sock: string, child: Deno.ChildProcess) {
  let exited = false; child.status.then(() => { exited = true; });
  for (let waited = 0; waited < START_MS; waited += 10) {
    if (exited) throw new Error('function exited while starting');      // e.g. a module that is not in the cache (CACHED_ONLY)
    try { const c = await Deno.connect({ transport: 'unix', path: sock }); c.close(); return; } catch { /* not listening yet */ }
    await new Promise((r) => setTimeout(r, 10));
  }
  throw new Error('function did not start');
}

function stop(name: string, fn: Fn) {
  try { fn.child.kill('SIGKILL'); } catch { /* already gone */ }
  if (running.get(name) === fn) running.delete(name);
}

function start(name: string): Fn {
  const dir = `${FUNCTIONS}/${name}`, sock = `/tmp/fn-${name}.sock`, scratch = `/tmp/fn-${name}`;
  try { Deno.removeSync(sock); } catch { /* none */ }
  Deno.mkdirSync(scratch, { recursive: true });
  // The function gets a clean environment: its socket address, the module cache, and the variables it is allowed to read.
  const env: Record<string, string> = { DENO_SERVE_ADDRESS: `unix:${sock}`, DENO_DIR: Deno.env.get('DENO_DIR') ?? '/tmp/deno', NO_COLOR: '1' };
  for (const k of ALLOWED_ENV) { const v = Deno.env.get(k); if (v !== undefined) env[k] = v; }
  const args = ['run', '--no-prompt', '--allow-net', `--allow-read=${dir},${sock},${scratch}`, `--allow-write=${sock},${scratch}`,
    `--allow-env=${['DENO_SERVE_ADDRESS', ...ALLOWED_ENV].join(',')}`,
    ...(HEAP_MB > 0 ? [`--v8-flags=--max-old-space-size=${HEAP_MB}`] : []), ...(CACHED_ONLY ? ['--cached-only'] : []), `${dir}/index.ts`];
  const child = new Deno.Command(Deno.execPath(), { args, clearEnv: true, env, stdout: 'inherit', stderr: 'inherit' }).spawn();
  const client = Deno.createHttpClient({ proxy: { transport: 'unix', path: sock } });
  const fn: Fn = { child, client, last: Date.now(), ready: waitForSocket(sock, child) };
  fn.ready.catch(() => {});
  child.status.then(() => { if (running.get(name) === fn) running.delete(name); client.close(); });
  running.set(name, fn);
  return fn;
}

setInterval(() => { for (const [n, f] of running) if (Date.now() - f.last > IDLE_MS) stop(n, f); }, 1000);

Deno.serve({ port: 9000 }, async (req) => {
  const url = new URL(req.url);
  if (url.pathname === '/_stats') return Response.json({ running: [...running.keys()] });
  const [, name, ...rest] = url.pathname.split('/');
  if (!name || !/^[a-z0-9-]{1,40}$/.test(name)) return new Response('bad name', { status: 400 });
  try { if (!Deno.statSync(`${FUNCTIONS}/${name}/index.ts`).isFile) throw 0; } catch { return new Response('no such function', { status: 404 }); }
  const t0 = performance.now();
  const fn = running.get(name) ?? start(name);
  try { await fn.ready; } catch { stop(name, fn); return new Response('function failed to start', { status: 502 }); }
  fn.last = Date.now();
  const timer = AbortSignal.timeout(REQUEST_MS);
  try {
    const res = await fetch(new Request('http://function/' + rest.join('/') + url.search, req), { client: fn.client, signal: timer } as RequestInit);
    const headers = new Headers(res.headers); headers.set('x-fn-ms', String(Math.round(performance.now() - t0)));
    return new Response(res.body, { status: res.status, headers });
  } catch {
    stop(name, fn);   // an overrun, or a function that died: the next request starts a fresh process
    return new Response(timer.aborted ? 'function timed out' : 'function failed', { status: timer.aborted ? 504 : 502 });
  } finally { fn.last = Date.now(); }
});
