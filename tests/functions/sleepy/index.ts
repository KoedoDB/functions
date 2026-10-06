Deno.serve(async () => { await new Promise((r) => setTimeout(r, 1500)); return new Response("slept"); });
