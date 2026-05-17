import type { ServerWebSocket } from "bun";
import type { PeerInfo, ServerMsg } from "./types";

// Peer is the live state for a connected WebSocket. The PeerInfo we expose
// to other peers is derived from this; room is kept here (not in PeerInfo)
// because it's a server-side membership thing, not part of the peer's
// public identity.
export type Peer = {
  ws: ServerWebSocket<SocketData>;
  room: string;
  info: PeerInfo;
};

// SocketData is attached to each WebSocket so we know which peer owns it
// without an extra lookup map. It's only populated after the hello handshake.
export type SocketData = {
  peer: Peer | null;
};

// Each room is its own /24 overlay (10.99.0.0/24): peers reserve a
// 10.99.0.X within the room when they join, and release it when they
// leave. The 0th address is reserved as the subnet's network address;
// .255 is reserved as broadcast. Two rooms can both use .1, .2 — they
// never share a subnet between them at the wire level.
const SUBNET_PREFIX = "10.99.0";
const FIRST_HOST = 1;
const LAST_HOST = 254;

type Room = {
  peers: Map<string, Peer>;
  allocated: Set<number>; // last octet of in-use IPs
};

export class Hub {
  private rooms = new Map<string, Room>();

  // Add peer to its room and assign a tunnel_ip from the room's pool.
  // The chosen address is written back into peer.info.tunnel_ip so the
  // caller sees it for the welcome message. Throws if the pool is full.
  join(roomName: string, peer: Peer): { existing: PeerInfo[]; tunnelIP: string } {
    let r = this.rooms.get(roomName);
    if (!r) {
      r = { peers: new Map(), allocated: new Set() };
      this.rooms.set(roomName, r);
    }
    const octet = this.nextOctet(r);
    if (octet < 0) throw new Error(`room ${roomName} is full`);
    r.allocated.add(octet);
    const tunnelIP = `${SUBNET_PREFIX}.${octet}`;
    peer.info.tunnel_ip = tunnelIP;
    const existing = Array.from(r.peers.values()).map((p) => p.info);
    r.peers.set(peer.info.name, peer);
    return { existing, tunnelIP };
  }

  private nextOctet(r: Room): number {
    for (let i = FIRST_HOST; i <= LAST_HOST; i++) {
      if (!r.allocated.has(i)) return i;
    }
    return -1;
  }

  // Remove peer from its room and release its tunnel_ip. Returns true if
  // anything was removed. Empty rooms are dropped from the map so memory
  // doesn't leak on long-lived servers.
  leave(roomName: string, name: string): boolean {
    const r = this.rooms.get(roomName);
    if (!r) return false;
    const peer = r.peers.get(name);
    if (!peer) return false;
    r.peers.delete(name);
    const octet = parseOctet(peer.info.tunnel_ip);
    if (octet >= 0) r.allocated.delete(octet);
    if (r.peers.size === 0) this.rooms.delete(roomName);
    return true;
  }

  // Find a specific peer in a room (for direct signal forwarding).
  get(roomName: string, name: string): Peer | undefined {
    return this.rooms.get(roomName)?.peers.get(name);
  }

  // True if a peer with this name already lives in the room.
  has(roomName: string, name: string): boolean {
    return this.rooms.get(roomName)?.peers.has(name) ?? false;
  }

  // Send a message to every peer in the room *except* the excluded one.
  broadcast(roomName: string, msg: ServerMsg, excludeName?: string): void {
    const r = this.rooms.get(roomName);
    if (!r) return;
    const data = JSON.stringify(msg);
    for (const p of r.peers.values()) {
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
    const rooms: Array<{ name: string; peers: string[] }> = [];
    for (const [name, room] of this.rooms.entries()) {
      rooms.push({ name, peers: Array.from(room.peers.keys()) });
    }
    return { rooms, total_peers: rooms.reduce((s, r) => s + r.peers.length, 0) };
  }
}

function parseOctet(ip: string | undefined): number {
  if (!ip) return -1;
  const parts = ip.split(".");
  if (parts.length !== 4) return -1;
  const n = Number(parts[3]);
  return Number.isInteger(n) && n >= 0 && n <= 255 ? n : -1;
}
