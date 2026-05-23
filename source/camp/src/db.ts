// Turso-backed persistent storage for sticky (camp_id, pub) → octet
// bindings. When TURSO_URL / TURSO_AUTH_TOKEN aren't set, falls back to
// a stub that returns "no binding" for every lookup — Hub then behaves
// as before (allocate fresh octet each time, no persistence).
//
// The schema is one row per (camp_id, pub) — pub is the peer's stable
// ed25519 identity, name is just a mutable alias stored alongside. The
// older `bindings` table (keyed by name) is left intact in case we ever
// need to look at the legacy state, but new writes go to `peer_bindings`.

import { createClient, type Client } from "@libsql/client";

let client: Client | null = null;

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS peer_bindings (
     camp_id   TEXT    NOT NULL,
     pub       TEXT    NOT NULL,
     name      TEXT    NOT NULL,
     octet     INTEGER NOT NULL,
     last_seen INTEGER NOT NULL,
     PRIMARY KEY (camp_id, pub)
   )`,
  `CREATE UNIQUE INDEX IF NOT EXISTS peer_bindings_camp_octet
     ON peer_bindings(camp_id, octet)`,
  `CREATE INDEX IF NOT EXISTS peer_bindings_last_seen
     ON peer_bindings(last_seen)`,
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
  if (!authToken) {
    console.log("db: TURSO_AUTH_TOKEN not set — bindings will not persist");
    return false;
  }
  console.log(`db: connecting to ${urlScheme(url)} host=${urlHost(url)} tokenLen=${authToken.length}`);
  try {
    client = createClient({ url, authToken });
    for (const sql of SCHEMA) {
      await client.execute(sql);
    }
    console.log(`db: connected`);
    return true;
  } catch (err) {
    const e = err as Error & { code?: string; cause?: unknown };
    console.error(`db: init failed: ${e.message}${e.code ? ` (code=${e.code})` : ""}`);
    if (e.cause) console.error(`db: cause: ${String(e.cause)}`);
    client = null;
    return false;
  }
}

function urlScheme(u: string): string {
  const i = u.indexOf("://");
  return i < 0 ? "?" : u.slice(0, i);
}
function urlHost(u: string): string {
  const i = u.indexOf("://");
  if (i < 0) return "?";
  const rest = u.slice(i + 3);
  const slash = rest.indexOf("/");
  return slash < 0 ? rest : rest.slice(0, slash);
}

// BindingRow is one persisted entry. Indexed by pub elsewhere.
export type BindingRow = {
  name: string;
  octet: number;
};

// loadBindings returns the full (pub → {name, octet}) map for a camp.
// Used by Hub on first contact with a camp to populate its in-memory
// cache. Empty map if DB isn't connected or the camp is new.
export async function loadBindings(campID: string): Promise<Map<string, BindingRow>> {
  const out = new Map<string, BindingRow>();
  if (!client) return out;
  try {
    const r = await client.execute({
      sql: "SELECT pub, name, octet FROM peer_bindings WHERE camp_id = ?",
      args: [campID],
    });
    for (const row of r.rows) {
      out.set(String(row.pub), {
        name: String(row.name),
        octet: Number(row.octet),
      });
    }
  } catch (err) {
    console.error(`db: loadBindings(${campID}) failed: ${(err as Error).message}`);
  }
  return out;
}

// saveBinding inserts or refreshes a binding. Idempotent. The caller
// is responsible for octet uniqueness within the camp; if the unique
// index trips (race with another concurrent INSERT), the error
// surfaces so the caller can retry with a different octet.
export async function saveBinding(campID: string, pub: string, name: string, octet: number): Promise<void> {
  if (!client) return;
  try {
    await client.execute({
      sql: `INSERT INTO peer_bindings (camp_id, pub, name, octet, last_seen)
            VALUES (?, ?, ?, ?, ?)
            ON CONFLICT (camp_id, pub)
            DO UPDATE SET last_seen = excluded.last_seen,
                          name      = excluded.name`,
      args: [campID, pub, name, octet, Date.now()],
    });
  } catch (err) {
    // Bubble up — typically a unique-octet conflict that the caller
    // resolves by picking a different octet.
    throw err;
  }
}

// touchBinding bumps last_seen on a known (camp, pub) row and updates
// the stored name (in case the peer renamed themselves). No-op if row
// doesn't exist or DB isn't connected.
export async function touchBinding(campID: string, pub: string, name: string): Promise<void> {
  if (!client) return;
  try {
    await client.execute({
      sql: "UPDATE peer_bindings SET last_seen = ?, name = ? WHERE camp_id = ? AND pub = ?",
      args: [Date.now(), name, campID, pub],
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
      sql: "DELETE FROM peer_bindings WHERE last_seen < ?",
      args: [cutoffMs],
    });
    return r.rowsAffected;
  } catch (err) {
    console.error(`db: cleanupStale failed: ${(err as Error).message}`);
    return 0;
  }
}
