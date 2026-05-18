import type { PeerInfo } from "./types";

// Peer is the live state for a registered peer. We key by camp_id+name
// in the parent Hub; the campID field here is mainly for outbound
// PeerInfo construction (we never publish it, but the Hub keeps a
// reverse map for eviction).
export type Peer = {
  campID: string;
  info: PeerInfo;
  lastSeen: number; // epoch ms
};

// Each camp is its own /24 overlay (10.99.0.0/24): peers reserve a
// 10.99.0.X within the camp when they join, and release it when they
// leave. The 0th address is reserved as the subnet's network address;
// .255 is reserved as broadcast. Two camps can both use .1, .2 — they
// never share a subnet between them at the wire level.
const SUBNET_PREFIX = "10.99.0";
const FIRST_HOST = 1;
const LAST_HOST = 254;

type Camp = {
  peers: Map<string, Peer>;
  allocated: Set<number>; // last octet of in-use IPs
};

export class Hub {
  private camps = new Map<string, Camp>();

  // Upsert is the announce-driven path: either refresh an existing
  // peer's endpoint+lastSeen, or join a brand-new one. Returns the
  // PeerInfo to send back in the announce reply. Throws if the camp is
  // full when joining for the first time.
  upsert(campID: string, name: string, publicIP: string, udpPort: number): PeerInfo {
    let c = this.camps.get(campID);
    if (!c) {
      c = { peers: new Map(), allocated: new Set() };
      this.camps.set(campID, c);
    }
    const now = Date.now();
    const existing = c.peers.get(name);
    if (existing) {
      existing.info.public_ip = publicIP;
      existing.info.udp_port = udpPort;
      existing.info.udp_endpoint = `${publicIP}:${udpPort}`;
      existing.lastSeen = now;
      return existing.info;
    }
    const octet = this.nextOctet(c);
    if (octet < 0) throw new Error(`camp ${campID} is full`);
    c.allocated.add(octet);
    const info: PeerInfo = {
      name,
      public_ip: publicIP,
      udp_port: udpPort,
      udp_endpoint: `${publicIP}:${udpPort}`,
      tunnel_ip: `${SUBNET_PREFIX}.${octet}`,
      joined_at: now,
    };
    c.peers.set(name, { campID, info, lastSeen: now });
    return info;
  }

  private nextOctet(c: Camp): number {
    for (let i = FIRST_HOST; i <= LAST_HOST; i++) {
      if (!c.allocated.has(i)) return i;
    }
    return -1;
  }

  // Find a specific peer in a camp. Currently unused but cheap to keep
  // around for future signal forwarding etc.
  get(campID: string, name: string): Peer | undefined {
    return this.camps.get(campID)?.peers.get(name);
  }

  // True if a peer with this name already lives in the camp. Used to
  // distinguish first-time joins from refreshes when validating.
  has(campID: string, name: string): boolean {
    return this.camps.get(campID)?.peers.has(name) ?? false;
  }

  // Snapshot of every peer's PeerInfo in a camp. Empty array if the
  // camp doesn't exist (which is indistinguishable from "0 peers" in
  // this model).
  list(campID: string): PeerInfo[] {
    const c = this.camps.get(campID);
    if (!c) return [];
    return Array.from(c.peers.values()).map((p) => p.info);
  }

  // Evict peers whose lastSeen is older than threshold. Called on a
  // timer so stale entries don't keep tunnel_ips reserved or pollute
  // the per-camp HTTP view.
  evictStale(threshold: number): number {
    let removed = 0;
    for (const [campID, c] of this.camps) {
      for (const [name, p] of c.peers) {
        if (p.lastSeen < threshold) {
          c.peers.delete(name);
          const octet = parseOctet(p.info.tunnel_ip);
          if (octet >= 0) c.allocated.delete(octet);
          removed++;
          console.log(`evict: ${name}@${campID} (idle)`);
        }
      }
      if (c.peers.size === 0) this.camps.delete(campID);
    }
    return removed;
  }

  // Snapshot of the whole hub — used by /api/stats for ops visibility.
  stats() {
    const camps: Array<{ id: string; peers: string[] }> = [];
    for (const [id, camp] of this.camps.entries()) {
      camps.push({ id, peers: Array.from(camp.peers.keys()) });
    }
    return { camps, total_peers: camps.reduce((s, c) => s + c.peers.length, 0) };
  }
}

function parseOctet(ip: string | undefined): number {
  if (!ip) return -1;
  const parts = ip.split(".");
  if (parts.length !== 4) return -1;
  const n = Number(parts[3]);
  return Number.isInteger(n) && n >= 0 && n <= 255 ? n : -1;
}
