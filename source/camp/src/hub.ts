import type { PeerInfo } from "./types";

export type Peer = {
  campID: string;
  info: PeerInfo;
  lastSeen: number;
};

type Camp = {
  peers: Map<string, Peer>; // keyed by pub
};

export class Hub {
  private camps = new Map<string, Camp>();

  private ensure(campID: string): Camp {
    let c = this.camps.get(campID);
    if (!c) {
      c = { peers: new Map() };
      this.camps.set(campID, c);
    }
    return c;
  }

  async upsert(campID: string, pub: string, name: string, publicIP: string, udpPort: number): Promise<PeerInfo> {
    const c = this.ensure(campID);
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
      return existing.info;
    }
    const info: PeerInfo = {
      name,
      pub,
      public_ip: publicIP,
      udp_port: udpPort,
      udp_endpoint: `${publicIP}:${udpPort}`,
      joined_at: now,
      online: true,
      last_seen_at: now,
    };
    c.peers.set(pub, { campID, info, lastSeen: now });
    return info;
  }

  get(campID: string, pub: string): Peer | undefined {
    return this.camps.get(campID)?.peers.get(pub);
  }

  has(campID: string, pub: string): boolean {
    return this.camps.get(campID)?.peers.has(pub) ?? false;
  }

  list(campID: string): PeerInfo[] {
    const c = this.camps.get(campID);
    if (!c) return [];
    return Array.from(c.peers.values(), (p) => p.info);
  }

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
      if (c.peers.size === 0) {
        this.camps.delete(campID);
      }
    }
    return removed;
  }

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
