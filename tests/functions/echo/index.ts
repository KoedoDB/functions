// Returns what it was sent, so the tests can see that the method, the headers and the body get through the supervisor.
Deno.serve(async (req: Request) => {
  const body = await req.text();
  return new Response(JSON.stringify({ method: req.method, path: new URL(req.url).pathname, query: new URL(req.url).search, body, header: req.headers.get('x-test') }),
    { status: Number(new URL(req.url).searchParams.get('status') ?? 200), headers: { 'content-type': 'application/json', 'x-from-function': 'yes' } });
});
