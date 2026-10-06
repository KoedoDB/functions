// The headers go out at once; the body takes 4 seconds (a small chunk every 200 ms): a request that is still in flight after its headers have been sent.
Deno.serve(() => {
  let i = 0;
  const body = new ReadableStream<Uint8Array>({
    async pull(c) { await new Promise((r) => setTimeout(r, 200)); if (++i > 20) c.close(); else c.enqueue(new TextEncoder().encode('chunk ' + i + '\n')); },
  });
  return new Response(body, { headers: { 'content-type': 'text/plain' } });
});
