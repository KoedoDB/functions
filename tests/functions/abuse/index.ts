Deno.serve(async (req: Request) => {
  const t = new URL(req.url).searchParams.get('t');
  if (t === 'mem') { const a: Uint8Array[] = []; for (let i = 0; i < 100000; i++) a.push(new Uint8Array(10 * 1024 * 1024).fill(1)); return new Response('survived'); }
  if (t === 'heap') { const a: number[][] = []; for (let i = 0; i < 100000; i++) a.push(new Array(1_000_000).fill(i)); return new Response('survived heap'); }
  if (t === 'wasm') { const m = new WebAssembly.Memory({ initial: 60000 }); return new Response('wasm ' + m.buffer.byteLength); }
  if (t === 'cpu') { const e = Date.now() + 30000; while (Date.now() < e) {} return new Response('survived cpu'); }
  if (t === 'async') { const e = Date.now() + 30000; while (Date.now() < e) { await Promise.resolve(); } return new Response('survived async'); }
  if (t === 'sleep') { await new Promise((r) => setTimeout(r, 30000)); return new Response('slept'); }
  if (t === 'big') { return new Response('x'.repeat(200 * 1024 * 1024)); }
  return new Response('ok');
});
