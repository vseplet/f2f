import { loadBindings, saveBinding, touchBinding, type BindingRow } from "./db";
import type { PeerInfo } from "./types";

// Peer is the live state for a registered peer.
export type Peer = {
  campID: string;
  info: PeerInfo;
  lastSeen: number; // epoch ms
};

// Each camp is its own /24 overlay (10.99.0.0/24). The mapping
// pub → {name, octet} is sticky across leave/rejoin: Hub holds it in
// `bindings` in-memory (write-through to Turso), so a peer that
// reconnects gets back the same tunnel_ip they had before. Name is
// just a mutable alias stored alongside.
const SUBNET_PREFIX = "10.99.0";
const FIRST_HOST = 1;
const LAST_HOST = 254;

type Camp = {
  peers: Map<string, Peer>;          // keyed by pub
  bindings: Map<string, BindingRow>; // keyed by pub
  loaded: boolean;                   // bindings have been hydrated from db
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
      for (const [pub, row] of persisted) {
        if (!c.bindings.has(pub)) c.bindings.set(pub, row);
      }
      c.loaded = true;
    }
    return c;
  }

  // Upsert is the announce-driven path: refresh an existing peer or
  // welcome a new one. The peer's octet is sticky via `bindings` —
  // same pub in same camp → same tunnel_ip across sessions. Name is
  // just stored alongside; renames are free. Throws if the camp's /24
  // pool is exhausted on a fresh allocation.
  async upsert(campID: string, pub: string, name: string, publicIP: string, udpPort: number): Promise<PeerInfo> {
    const c = await this.ensureLoaded(campID);
    const now = Date.now();
    const existing = c.peers.get(pub);
    if (existing) {
      existing.info.name = name;
      existing.info.public_ip = publicIP;
      existing.info.udp_port = udpPort;
      existing.info.udp_endpoint = `${publicIP}:${udpPort}`;
      existing.info.online = true;
      existing.info.last_seen_at = now;
      existing.lastSeen = now;
      const row = c.bindings.get(pub);
      if (row && row.name !== name) row.name = name;
      void touchBinding(campID, pub, name);
      return existing.info;
    }
    // First time we see this pub in this camp's live state — figure
    // out which octet they get.
    const prior = c.bindings.get(pub);
    let octet = prior ? prior.octet : -1;
    if (octet < 0) {
      // No prior binding (in-memory or db). Allocate next free.
      octet = this.nextFreeOctet(c);
      if (octet < 0) throw new Error(`camp ${campID} is full`);
      c.bindings.set(pub, { name, octet });
    } else if (!isOctetTaken(c, octet, pub)) {
      // Prior binding is still ours to use; update name if changed.
      if (prior && prior.name !== name) {
        c.bindings.set(pub, { name, octet });
      }
    } else {
      // Octet is taken by someone else currently (unusual: only
      // happens if a binding was reassigned out-of-band, or a long-dead
      // entry never got cleaned). Reallocate.
      octet = this.nextFreeOctet(c);
      if (octet < 0) throw new Error(`camp ${campID} is full`);
      c.bindings.set(pub, { name, octet });
    }
    const info: PeerInfo = {
      name,
      pub,
      public_ip: publicIP,
      udp_port: udpPort,
      udp_endpoint: `${publicIP}:${udpPort}`,
      tunnel_ip: `${SUBNET_PREFIX}.${octet}`,
      joined_at: now,
      online: true,
      last_seen_at: now,
    };
    c.peers.set(pub, { campID, info, lastSeen: now });

    // Write-through to Turso. Tolerate failures (we still have the
    // binding in-memory for this session).
    saveBinding(campID, pub, name, octet).catch((err) => {
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
    for (const row of c.bindings.values()) taken.add(row.octet);
    for (let i = FIRST_HOST; i <= LAST_HOST; i++) {
      if (!taken.has(i)) return i;
    }
    return -1;
  }

  // Find a specific peer (live) in a camp.
  get(campID: string, pub: string): Peer | undefined {
    return this.camps.get(campID)?.peers.get(pub);
  }

  // True if a peer with this pub is currently in the camp.
  has(campID: string, pub: string): boolean {
    return this.camps.get(campID)?.peers.has(pub) ?? false;
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
    for (const [pub, row] of c.bindings) {
      if (c.peers.has(pub)) continue;
      out.push({
        name: row.name,
        pub,
        public_ip: "",
        tunnel_ip: `${SUBNET_PREFIX}.${row.octet}`,
        joined_at: 0,
        online: false,
        last_seen_at: 0,
      });
    }
    return out;
  }

  // Evict peers whose lastSeen is older than threshold. The binding
  // stays (so the same pub returns to the same tunnel_ip on next
  // announce) — only the live-peer entry is removed. The camp itself
  // is kept around as long as it has bindings; empty camps are
  // dropped purely from the live map.
  evictStale(threshold: number): number {
    let removed = 0;
    for (const [campID, c] of this.camps) {
      for (const [pub, p] of c.peers) {
        if (p.lastSeen < threshold) {
          c.peers.delete(pub);
          removed++;
          console.log(`evict: ${p.info.name}@${campID} pub=${pub.slice(0, 16)} (idle)`);
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

  // Stats — names of live peers per camp. Kept in the same shape as
  // before so dashboards don't break.
  stats() {
    const camps: Array<{ id: string; peers: string[] }> = [];
    for (const [id, camp] of this.camps.entries()) {
      const names: string[] = [];
      for (const p of camp.peers.values()) names.push(p.info.name);
      camps.push({ id, peers: names });
    }
    return { camps, total_peers: camps.reduce((s, c) => s + c.peers.length, 0) };
  }
}

function isOctetTaken(c: Camp, octet: number, exceptPub: string): boolean {
  for (const [pub, row] of c.bindings) {
    if (pub !== exceptPub && row.octet === octet) return true;
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
