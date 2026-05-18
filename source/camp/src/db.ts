// Turso-backed persistent storage for sticky (camp_id, name) → octet
// bindings. When TURSO_URL / TURSO_AUTH_TOKEN aren't set, falls back to
// a stub that returns "no binding" for every lookup — Hub then behaves
// as before (allocate fresh octet each time, no persistence).
//
// The schema is kept dead simple: one row per (camp, name) with the
// current octet and a last_seen ms timestamp. Stale rows are pruned
// periodically so a long-lived camp doesn't accumulate forever.

import { createClient, type Client } from "@libsql/client";

let client: Client | null = null;

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS bindings (
     camp_id   TEXT    NOT NULL,
     name      TEXT    NOT NULL,
     octet     INTEGER NOT NULL,
     last_seen INTEGER NOT NULL,
     PRIMARY KEY (camp_id, name)
   )`,
  `CREATE UNIQUE INDEX IF NOT EXISTS bindings_camp_octet
     ON bindings(camp_id, octet)`,
  `CREATE INDEX IF NOT EXISTS bindings_last_seen
     ON bindings(last_seen)`,
];

// initDB connects to Turso if creds are present and applies the schema.
// Safe to call when creds are missing — returns false and subsequent
// calls degrade gracefully.
export async function initDB(): Promise<boolean> {
  const url = Bun.env.TURSO_URL?.trim();
  const authToken = Bun.env.TURSO_AUTH_TOKEN?.trim();
  if (!url) {
    console.log("db: TURSO_URL not set — bindings will not persist");
    return false;
  }
  try {
    client = createClient({ url, authToken });
    for (const sql of SCHEMA) {
      await client.execute(sql);
    }
    console.log(`db: connected to ${redactURL(url)}`);
    return true;
  } catch (err) {
    console.error(`db: init failed: ${(err as Error).message}`);
    client = null;
    return false;
  }
}

// loadBindings returns the full (name → octet) map for a camp. Used by
// Hub on first contact with a camp to populate its in-memory cache.
// Empty map if DB isn't connected or the camp is new.
export async function loadBindings(campID: string): Promise<Map<string, number>> {
  const out = new Map<string, number>();
  if (!client) return out;
  try {
    const r = await client.execute({
      sql: "SELECT name, octet FROM bindings WHERE camp_id = ?",
      args: [campID],
    });
    for (const row of r.rows) {
      out.set(String(row.name), Number(row.octet));
    }
  } catch (err) {
    console.error(`db: loadBindings(${campID}) failed: ${(err as Error).message}`);
  }
  return out;
}

// saveBinding inserts or refreshes a binding's last_seen. Idempotent.
// Caller is responsible for octet uniqueness within the camp; if the
// unique index trips (race with another concurrent INSERT), the error
// surfaces so the caller can retry with a different octet.
export async function saveBinding(campID: string, name: string, octet: number): Promise<void> {
  if (!client) return;
  try {
    await client.execute({
      sql: `INSERT INTO bindings (camp_id, name, octet, last_seen)
            VALUES (?, ?, ?, ?)
            ON CONFLICT (camp_id, name)
            DO UPDATE SET last_seen = excluded.last_seen`,
      args: [campID, name, octet, Date.now()],
    });
  } catch (err) {
    // Bubble up — typically a unique-octet conflict that the caller
    // resolves by picking a different octet.
    throw err;
  }
}

// touchBinding bumps last_seen on a known (camp, name) row. No-op if
// row doesn't exist or DB isn't connected. Run on every announce so
// active bindings stay fresh.
export async function touchBinding(campID: string, name: string): Promise<void> {
  if (!client) return;
  try {
    await client.execute({
      sql: "UPDATE bindings SET last_seen = ? WHERE camp_id = ? AND name = ?",
      args: [Date.now(), campID, name],
    });
  } catch (err) {
    console.error(`db: touchBinding failed: ${(err as Error).message}`);
  }
}

// cleanupStale drops bindings whose last_seen is older than the cutoff.
// Called from a periodic timer in server.ts.
export async function cleanupStale(cutoffMs: number): Promise<number> {
  if (!client) return 0;
  try {
    const r = await client.execute({
      sql: "DELETE FROM bindings WHERE last_seen < ?",
      args: [cutoffMs],
    });
    return r.rowsAffected;
  } catch (err) {
    console.error(`db: cleanupStale failed: ${(err as Error).message}`);
    return 0;
  }
}

function redactURL(u: string): string {
  return u.replace(/(authToken=)[^&]+/, "$1***");
}
