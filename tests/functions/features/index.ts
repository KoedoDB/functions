// The same file runs in both runtimes: which language/runtime features and which npm packages work?
const checks: Record<string, () => unknown> = {
  'Promise.try': () => typeof (Promise as any).try === 'function',
  'Promise.withResolvers': () => typeof (Promise as any).withResolvers === 'function',
  'Iterator helpers (.map on iterators)': () => typeof [1].values().map === 'function',
  'Set.prototype.union': () => typeof (new Set() as any).union === 'function',
  'Array.fromAsync': () => typeof (Array as any).fromAsync === 'function',
  'Object.groupBy': () => typeof (Object as any).groupBy === 'function',
  'RegExp.escape': () => typeof (RegExp as any).escape === 'function',
  'Error.isError': () => typeof (Error as any).isError === 'function',
  'Math.sumPrecise': () => typeof (Math as any).sumPrecise === 'function',
  'Float16Array': () => typeof (globalThis as any).Float16Array === 'function',
  'Uint8Array.fromBase64': () => typeof (Uint8Array as any).fromBase64 === 'function',
  'Symbol.dispose (using)': () => typeof (Symbol as any).dispose === 'symbol',
  'Temporal': () => typeof (globalThis as any).Temporal === 'object',
  'URL.parse': () => typeof (URL as any).parse === 'function',
  'URLPattern': () => typeof (globalThis as any).URLPattern === 'function',
  'Intl.DurationFormat': () => typeof (Intl as any).DurationFormat === 'function',
  'Response.prototype.bytes': () => typeof (Response.prototype as any).bytes === 'function',
  'WebSocketStream': () => typeof (globalThis as any).WebSocketStream === 'function',
  'import.meta.dirname': () => typeof (import.meta as any).dirname === 'string',
  'CompressionStream zstd?': () => { try { new (CompressionStream as any)('zstd'); return true; } catch { return false; } },
  'WebCrypto Ed25519': async () => { try { await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']); return true; } catch { return false; } },
  'WebCrypto AES-GCM rejects a forged tag': async () => {
    const k = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, ['encrypt', 'decrypt']);
    const iv = new Uint8Array(12); const ct = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, k, new TextEncoder().encode('secret')));
    ct[ct.length - 1] ^= 1;
    try { await crypto.subtle.decrypt({ name: 'AES-GCM', iv }, k, ct); return false; } catch { return true; }
  },
  'node:sqlite': async () => typeof (await import('node:sqlite')).DatabaseSync === 'function',
  'node:test': async () => typeof (await import('node:test')).describe === 'function',
  'node:zlib zstd': async () => typeof (await import('node:zlib') as any).zstdCompressSync === 'function',
};
const packages = ['zod@4', 'hono@4', '@supabase/supabase-js@2', 'openai@6', 'stripe', '@modelcontextprotocol/sdk', 'ai', 'drizzle-orm', 'ky', 'jose'];
Deno.serve(async () => {
  const out: Record<string, unknown> = { deno: (globalThis as any).Deno?.version?.deno, v8: (globalThis as any).Deno?.version?.v8, ts: (globalThis as any).Deno?.version?.typescript };
  for (const [k, f] of Object.entries(checks)) { try { out[k] = await f(); } catch (e) { out[k] = 'ERR ' + String(e).slice(0, 80); } }
  for (const p of packages) {
    const t = performance.now();
    try { const m = await import('npm:' + p); out['npm:' + p] = `ok (${Object.keys(m).length} exports, ${Math.round(performance.now() - t)} ms)`; }
    catch (e) { out['npm:' + p] = 'ERR ' + String(e).replace(/\s+/g, ' ').slice(0, 140); }
  }
  return Response.json(out);
});
