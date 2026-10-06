// A function with its own deno.json (an import map): Deno would write a deno.lock next to it, and the directory is read-only.
import { z } from 'zod';
Deno.serve(() => Response.json(z.object({ a: z.number() }).safeParse({ a: 1 })));
