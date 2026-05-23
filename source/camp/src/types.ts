// Wire protocol. There is no WebSocket — everything is either UDP
// (announce protocol on the STUN port) or plain HTTP (`/api/id/:id`).
// Keep this in sync with source/mac/internal/rendezvous.

export type PeerInfo = {
  name: string;
  // Ed25519 public key in hex (64 chars) — the stable, cryptographically
  // unique identity of the peer. `name` is just a display alias. Empty
  // for legacy peers that haven't announced a pub yet (transitional).
  pub?: string;
  public_ip: string;
  // Advertised UDP port the peer is reachable on, derived from the
  // source address of its announce packet. Combined with public_ip this
  // forms an `udp_endpoint` that the *other* peer can target for hole
  // punching.
  udp_port?: number;
  udp_endpoint?: string;
  // Camp-assigned IP inside the camp's virtual subnet (e.g. 10.99.0.3).
  // The peer puts this on its local utun; other peers' tunnel_ip values
  // are how it reaches them through the overlay.
  tunnel_ip: string;
  joined_at: number;
  // online = peer has announced within EVICT_AFTER_MS. Offline peers
  // come from the sticky-binding catalog only, so they may lack
  // public_ip/udp_endpoint (set to "" / undefined in that case).
  online: boolean;
  last_seen_at: number;
};

// --- UDP wire types ---

// Announce: client → server, on the STUN UDP port. Server observes the
// public endpoint via the packet's source address — no need to put it
// in the body.
export type AnnounceReq = {
  t: "announce";
  name: string;
  camp_id: string;
  // Ed25519 public key in hex (64 chars). Optional during the transition
  // — legacy clients without one are accepted as-is. Once a peer
  // announces with a pub, the server records it alongside name.
  pub?: string;
};

// Server's reply on success.
export type AnnouncedResp = {
  t: "announced";
  you: PeerInfo;
};

// Server's reply on error.
export type AnnounceErr = {
  t: "error";
  code: string; // bad_name | bad_camp_id | camp_full | name_conflict | …
  message: string;
};
