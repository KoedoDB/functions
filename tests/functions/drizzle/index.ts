// A heavy function: drizzle-orm pulls in a long list of npm packages. Without a warm module cache its first start takes several seconds.
import { pgTable, serial, text } from 'npm:drizzle-orm/pg-core';
import { drizzle } from 'npm:drizzle-orm/pg-proxy';
import { eq } from 'npm:drizzle-orm';
const t = pgTable('t', { id: serial('id').primaryKey(), name: text('name') });
const db = drizzle(async () => ({ rows: [] }));
Deno.serve(() => Response.json(db.select().from(t).where(eq(t.id, 1)).toSQL()));
