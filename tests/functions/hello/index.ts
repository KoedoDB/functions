Deno.serve((req: Request) => new Response('hello ' + new URL(req.url).pathname));
