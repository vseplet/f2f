import type { ServerWebSocket } from "bun";
import type { PeerInfo, ServerMsg } from "./types";

// Peer is the live state for a connected WebSocket. The PeerInfo we expose
// to other peers is derived from this; campID is kept here (not in
// PeerInfo) because it's a server-side membership thing, not part of the
// peer's public identity.
export type Peer = {
  ws: ServerWebSocket<SocketData>;
  campID: string;
  info: PeerInfo;
};

// SocketData is attached to each WebSocket so we know which peer owns it
// without an extra lookup map. It's only populated after the hello handshake.
export type SocketData = {
  peer: Peer | null;
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

  // Add peer to its camp and assign a tunnel_ip from the camp's pool.
  // The chosen address is written back into peer.info.tunnel_ip so the
  // caller sees it for the welcome message. Throws if the pool is full.
  join(campID: string, peer: Peer): { existing: PeerInfo[]; tunnelIP: string } {
    let c = this.camps.get(campID);
    if (!c) {
      c = { peers: new Map(), allocated: new Set() };
      this.camps.set(campID, c);
    }
    const octet = this.nextOctet(c);
    if (octet < 0) throw new Error(`camp ${campID} is full`);
    c.allocated.add(octet);
    const tunnelIP = `${SUBNET_PREFIX}.${octet}`;
    peer.info.tunnel_ip = tunnelIP;
    const existing = Array.from(c.peers.values()).map((p) => p.info);
    c.peers.set(peer.info.name, peer);
    return { existing, tunnelIP };
  }

  private nextOctet(c: Camp): number {
    for (let i = FIRST_HOST; i <= LAST_HOST; i++) {
      if (!c.allocated.has(i)) return i;
    }
    return -1;
  }

  // Remove peer from its camp and release its tunnel_ip. Returns true if
  // anything was removed. Empty camps are dropped from the map so memory
  // doesn't leak on long-lived servers.
  leave(campID: string, name: string): boolean {
    const c = this.camps.get(campID);
    if (!c) return false;
    const peer = c.peers.get(name);
    if (!peer) return false;
    c.peers.delete(name);
    const octet = parseOctet(peer.info.tunnel_ip);
    if (octet >= 0) c.allocated.delete(octet);
    if (c.peers.size === 0) this.camps.delete(campID);
    return true;
  }

  // Find a specific peer in a camp (for direct signal forwarding).
  get(campID: string, name: string): Peer | undefined {
    return this.camps.get(campID)?.peers.get(name);
  }

  // Snapshot of every peer's PeerInfo in a camp. Empty array if the camp
  // doesn't exist (which is indistinguishable from "0 peers" in this model).
  list(campID: string): PeerInfo[] {
    const c = this.camps.get(campID);
    if (!c) return [];
    return Array.from(c.peers.values()).map((p) => p.info);
  }

  // True if a peer with this name already lives in the camp.
  has(campID: string, name: string): boolean {
    return this.camps.get(campID)?.peers.has(name) ?? false;
  }

  // Send a message to every peer in the camp *except* the excluded one.
  broadcast(campID: string, msg: ServerMsg, excludeName?: string): void {
    const c = this.camps.get(campID);
    if (!c) return;
    const data = JSON.stringify(msg);
    for (const p of c.peers.values()) {
      if (p.info.name === excludeName) continue;
      try {
        p.ws.send(data);
      } catch {
        // Dead socket. We'll find out via close handler; no need to act
        // here.
      }
    }
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
