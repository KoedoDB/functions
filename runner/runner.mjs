// The KoedoDB function runner: trusted platform code that serves one function's bundle on a Unix socket.
//
//   FN_SOCKET=<path> node --permission --allow-fs-read=... runner.mjs <bundle>
//
// The bundle is one JavaScript file (ESM) that a customer made (esbuild, on their computer or in their CI). It exports a function by default: (request: Request) => Response | Promise<Response>
// (or an object with a `fetch` method, such as a Hono app's). The runner turns an incoming HTTP request into a standard Request, calls it, and writes the Response back, streaming the body.
// A function that throws gets a 500; one that does not return a Response too. A failure while the bundle is loaded stops the process, and the supervisor says 502.
import http from 'node:http';
import { Readable } from 'node:stream';
import { pathToFileURL } from 'node:url';

const bundle = process.argv[2];
const socket = process.env.FN_SOCKET;
if (!bundle || !socket) {
  console.error('usage: FN_SOCKET=<path> node runner.mjs <bundle>');
  process.exit(2);
}

function pickHandler(mod) {
  const d = mod.default;
  if (typeof d === 'function') return d;
  if (d && typeof d.fetch === 'function') return (req) => d.fetch(req);
  return null;
}

const mod = await import(pathToFileURL(bundle).href);
const handler = pickHandler(mod);
if (!handler) {
  console.error('the bundle must export default a function (request) => Response, or an object with a fetch method');
  process.exit(3);
}

const server = http.createServer(async (nreq, nres) => {
  let res;
  try {
    const headers = new Headers();
    for (let i = 0; i < nreq.rawHeaders.length; i += 2) headers.append(nreq.rawHeaders[i], nreq.rawHeaders[i + 1]);
    const body = nreq.method === 'GET' || nreq.method === 'HEAD' ? undefined : Readable.toWeb(nreq);
    const req = new Request('http://function' + nreq.url, { method: nreq.method, headers, body, duplex: 'half' });
    res = await handler(req);
    if (!(res instanceof Response)) throw new TypeError('the function must return a Response');
  } catch (err) {
    console.error('function error:', err && err.stack ? err.stack : String(err));
    if (!nres.headersSent) nres.writeHead(500, { 'content-type': 'text/plain' });
    nres.end('function error');
    return;
  }
  const out = [];
  for (const [k, v] of res.headers) if (k !== 'set-cookie') out.push([k, v]);
  for (const c of res.headers.getSetCookie()) out.push(['set-cookie', c]);
  nres.writeHead(res.status, out);
  if (!res.body || nreq.method === 'HEAD') {
    nres.end();
    return;
  }
  Readable.fromWeb(res.body).on('error', () => nres.destroy()).pipe(nres);
});

server.listen(socket);

// The supervisor stops a process with SIGTERM first: finish what is in flight (a moment), then go.
process.on('SIGTERM', () => {
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 1000).unref();
});
