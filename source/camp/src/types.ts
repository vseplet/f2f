// Wire protocol. There is no WebSocket — everything is either UDP
// (announce protocol on the STUN port) or plain HTTP (`/api/id/:id`).
// Keep this in sync with source/mac/internal/rendezvous.

export type PeerInfo = {
  name: string;
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
};

// --- UDP wire types ---

// Announce: client → server, on the STUN UDP port. Server observes the
// public endpoint via the packet's source address — no need to put it
// in the body.
export type AnnounceReq = {
  t: "announce";
  name: string;
  camp_id: string;
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
