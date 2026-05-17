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

export class Hub {
  // room name → peer name → peer
  private rooms = new Map<string, Map<string, Peer>>();

  // Add peer to its room. Returns the snapshot of peers already present
  // (excluding the new one) so the caller can build the Welcome message
  // before broadcasting peer-joined.
  join(room: string, peer: Peer): { existing: PeerInfo[] } {
    let r = this.rooms.get(room);
    if (!r) {
      r = new Map();
      this.rooms.set(room, r);
    }
    const existing = Array.from(r.values()).map((p) => p.info);
    r.set(peer.info.name, peer);
    return { existing };
  }

  // Remove peer from its room. Returns true if anything was removed.
  // Empty rooms are dropped from the map so memory doesn't leak on
  // long-lived servers.
  leave(room: string, name: string): boolean {
    const r = this.rooms.get(room);
    if (!r) return false;
    const ok = r.delete(name);
    if (r.size === 0) this.rooms.delete(room);
    return ok;
  }

  // Find a specific peer in a room (for direct signal forwarding).
  get(room: string, name: string): Peer | undefined {
    return this.rooms.get(room)?.get(name);
  }

  // True if a peer with this name already lives in the room.
  has(room: string, name: string): boolean {
    return this.rooms.get(room)?.has(name) ?? false;
  }

  // Send a message to every peer in the room *except* the excluded one.
  broadcast(room: string, msg: ServerMsg, excludeName?: string): void {
    const r = this.rooms.get(room);
    if (!r) return;
    const data = JSON.stringify(msg);
    for (const p of r.values()) {
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
    for (const [name, members] of this.rooms.entries()) {
      rooms.push({ name, peers: Array.from(members.keys()) });
    }
    return { rooms, total_peers: rooms.reduce((s, r) => s + r.peers.length, 0) };
  }
}
