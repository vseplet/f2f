// Wire protocol. All messages are JSON strings over a single WebSocket
// connection per peer. Server enforces "one hello first; everything else
// only after we know who you are".

export type PeerInfo = {
  name: string;
  public_ip: string;
  // Advertised UDP port the peer listens on. Combined with public_ip this
  // forms an `udp_endpoint` that the *other* peer can target for hole
  // punching. May be missing during the brief window between WebSocket
  // open and the peer's first `announce`.
  udp_port?: number;
  udp_endpoint?: string;
  joined_at: number;
};

// ---- client → server ----

export type Hello = {
  type: "hello";
  name: string; // unique within the room
  room: string;
  udp_port?: number; // optional; can also be sent later via announce
};

export type Announce = {
  type: "announce";
  udp_port: number;
};

export type Signal = {
  type: "signal";
  to: string; // peer name within the same room
  payload: unknown;
};

export type List = { type: "list" };
export type Ping = { type: "ping" };

export type ClientMsg = Hello | Announce | Signal | List | Ping;

// ---- server → client ----

export type Welcome = {
  type: "welcome";
  you: PeerInfo;
  room: string;
  peers: PeerInfo[];
};

export type ErrorMsg = {
  type: "error";
  code: string;
  message: string;
};

export type PeerJoined = {
  type: "peer-joined";
  peer: PeerInfo;
};

export type PeerUpdated = {
  type: "peer-updated";
  peer: PeerInfo;
};

export type PeerLeft = {
  type: "peer-left";
  name: string;
};

export type SignalDelivery = {
  type: "signal";
  from: string;
  payload: unknown;
};

export type Pong = { type: "pong" };

export type ServerMsg =
  | Welcome
  | ErrorMsg
  | PeerJoined
  | PeerUpdated
  | PeerLeft
  | SignalDelivery
  | Pong;
