import { loadBindings, saveBinding, touchBinding } from "./db";
import type { PeerInfo } from "./types";

// Peer is the live state for a registered peer.
export type Peer = {
  campID: string;
  info: PeerInfo;
  lastSeen: number; // epoch ms
};

// Each camp is its own /24 overlay (10.99.0.0/24). The mapping
// name → octet is sticky across leave/rejoin: Hub holds it in
// `bindings` in-memory (write-through to Turso), so a peer that
// reconnects gets back the same tunnel_ip they had before.
const SUBNET_PREFIX = "10.99.0";
const FIRST_HOST = 1;
const LAST_HOST = 254;

type Camp = {
  peers: Map<string, Peer>;
  bindings: Map<string, number>; // name → octet, sticky
  loaded: boolean;               // bindings have been hydrated from db
};

export class Hub {
  private camps = new Map<string, Camp>();

  // ensureLoaded lazy-loads the camp's persisted bindings on first
  // touch. Subsequent calls are no-ops.
  private async ensureLoaded(campID: string): Promise<Camp> {
    let c = this.camps.get(campID);
    if (!c) {
      c = { peers: new Map(), bindings: new Map(), loaded: false };
      this.camps.set(campID, c);
    }
    if (!c.loaded) {
      const persisted = await loadBindings(campID);
      // If we already had in-memory entries (from concurrent upserts
      // racing the load), keep ours and merge.
      for (const [name, octet] of persisted) {
        if (!c.bindings.has(name)) c.bindings.set(name, octet);
      }
      c.loaded = true;
    }
    return c;
  }

  // Upsert is the announce-driven path: refresh an existing peer or
  // welcome a new one. The peer's octet is sticky via `bindings` —
  // same name in same camp → same tunnel_ip across sessions. Throws
  // if the camp's /24 pool is exhausted on a fresh allocation.
  async upsert(campID: string, name: string, publicIP: string, udpPort: number): Promise<PeerInfo> {
    const c = await this.ensureLoaded(campID);
    const now = Date.now();
    const existing = c.peers.get(name);
    if (existing) {
      existing.info.public_ip = publicIP;
      existing.info.udp_port = udpPort;
      existing.info.udp_endpoint = `${publicIP}:${udpPort}`;
      existing.info.online = true;
      existing.info.last_seen_at = now;
      existing.lastSeen = now;
      void touchBinding(campID, name);
      return existing.info;
    }
    // First time we see this name in this camp's live state — figure
    // out which octet they get.
    let octet = c.bindings.get(name) ?? -1;
    if (octet < 0 || c.bindings.size === 0) {
      // No prior binding (in-memory or db). Allocate next free.
      octet = this.nextFreeOctet(c);
      if (octet < 0) throw new Error(`camp ${campID} is full`);
      c.bindings.set(name, octet);
    } else if (!isOctetTaken(c, octet, name)) {
      // Prior binding is still ours to use.
    } else {
      // Octet is taken by someone else currently (unusual: only
      // happens if a name was reassigned out-of-band, or a long-dead
      // entry never got cleaned). Reallocate.
      octet = this.nextFreeOctet(c);
      if (octet < 0) throw new Error(`camp ${campID} is full`);
      c.bindings.set(name, octet);
    }
    const info: PeerInfo = {
      name,
      public_ip: publicIP,
      udp_port: udpPort,
      udp_endpoint: `${publicIP}:${udpPort}`,
      tunnel_ip: `${SUBNET_PREFIX}.${octet}`,
      joined_at: now,
      online: true,
      last_seen_at: now,
    };
    c.peers.set(name, { campID, info, lastSeen: now });

    // Write-through to Turso. Tolerate failures (we still have the
    // binding in-memory for this session).
    saveBinding(campID, name, octet).catch((err) => {
      console.error(`db: saveBinding(${campID},${name}) failed: ${(err as Error).message}`);
    });
    return info;
  }

  private nextFreeOctet(c: Camp): number {
    const taken = new Set<number>();
    for (const p of c.peers.values()) {
      const o = parseOctet(p.info.tunnel_ip);
      if (o >= 0) taken.add(o);
    }
    // Also avoid octets that have a binding (sticky for absent peers).
    for (const o of c.bindings.values()) taken.add(o);
    for (let i = FIRST_HOST; i <= LAST_HOST; i++) {
      if (!taken.has(i)) return i;
    }
    return -1;
  }

  // Find a specific peer (live) in a camp.
  get(campID: string, name: string): Peer | undefined {
    return this.camps.get(campID)?.peers.get(name);
  }

  // True if a peer with this name is currently in the camp.
  has(campID: string, name: string): boolean {
    return this.camps.get(campID)?.peers.has(name) ?? false;
  }

  // Snapshot of every peer in a camp — online (currently announcing)
  // plus offline (in the sticky-binding catalog but not announcing).
  // Offline entries lack public_ip/udp_endpoint; consumers must treat
  // them as "known but unreachable" until they re-announce.
  list(campID: string): PeerInfo[] {
    const c = this.camps.get(campID);
    if (!c) return [];
    const out: PeerInfo[] = [];
    for (const p of c.peers.values()) out.push(p.info);
    for (const [name, octet] of c.bindings) {
      if (c.peers.has(name)) continue;
      out.push({
        name,
        public_ip: "",
        tunnel_ip: `${SUBNET_PREFIX}.${octet}`,
        joined_at: 0,
        online: false,
        last_seen_at: 0,
      });
    }
    return out;
  }

  // Evict peers whose lastSeen is older than threshold. The binding
  // stays (so the same name returns to the same tunnel_ip on next
  // announce) — only the live-peer entry is removed. The camp itself
  // is kept around as long as it has bindings; empty camps are
  // dropped purely from the live map.
  evictStale(threshold: number): number {
    let removed = 0;
    for (const [campID, c] of this.camps) {
      for (const [name, p] of c.peers) {
        if (p.lastSeen < threshold) {
          c.peers.delete(name);
          removed++;
          console.log(`evict: ${name}@${campID} (idle)`);
        }
      }
      // Drop the camp object if it has neither live peers nor any
      // sticky bindings — nothing to remember.
      if (c.peers.size === 0 && c.bindings.size === 0) {
        this.camps.delete(campID);
      }
    }
    return removed;
  }

  // Stats — kept in sync with the previous shape so external callers
  // don't break.
  stats() {
    const camps: Array<{ id: string; peers: string[] }> = [];
    for (const [id, camp] of this.camps.entries()) {
      camps.push({ id, peers: Array.from(camp.peers.keys()) });
    }
    return { camps, total_peers: camps.reduce((s, c) => s + c.peers.length, 0) };
  }
}

function isOctetTaken(c: Camp, octet: number, exceptName: string): boolean {
  for (const [name, o] of c.bindings) {
    if (name !== exceptName && o === octet) return true;
  }
  return false;
}

function parseOctet(ip: string | undefined): number {
  if (!ip) return -1;
  const parts = ip.split(".");
  if (parts.length !== 4) return -1;
  const n = Number(parts[3]);
  return Number.isInteger(n) && n >= 0 && n <= 255 ? n : -1;
}
