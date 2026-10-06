Deno.serve(() => { Deno.exit(1); return new Response("never"); });
