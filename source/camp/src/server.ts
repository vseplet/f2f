// f2f-camp — rendezvous server for f2f peers.
//
// Each peer opens one WebSocket to /ws, sends a "hello" with their name +
// room + (optionally) UDP port, and then receives the current peer list
// plus push notifications when peers join / leave / update. Signal messages
// are forwarded peer-to-peer through the server for hole-punching setup.
//
// State is in-memory and per-process — fly.io single-instance is fine for
// MVP. Two instances would require pinning peers in the same room to the
// same node (sticky sessions) or moving to a shared store.

import type { ServerWebSocket } from "bun";
import { Hub, type Peer, type SocketData } from "./hub";
import { startStun } from "./stun";
import type { ClientMsg, PeerInfo, ServerMsg } from "./types";

const PORT = Number(Bun.env.PORT ?? 8080);
const STUN_PORT = Number(Bun.env.STUN_PORT ?? 3478);
const MAX_NAME_LEN = 64;
const MAX_ROOM_LEN = 128;
const NAME_RE = /^[A-Za-z0-9_.-]+$/;

const hub = new Hub();

function send(ws: ServerWebSocket<SocketData>, msg: ServerMsg) {
  try {
    ws.send(JSON.stringify(msg));
  } catch {
    /* dead socket; close handler will tidy */
  }
}

function fail(ws: ServerWebSocket<SocketData>, code: string, message: string) {
  send(ws, { type: "error", code, message });
}

function makeEndpoint(p: PeerInfo): PeerInfo {
  if (p.udp_port == null) return { ...p, udp_endpoint: undefined };
  return { ...p, udp_endpoint: `${p.public_ip}:${p.udp_port}` };
}

// clientIP returns the peer's public IP as seen by us. Behind fly.io the
// real client IP is in Fly-Client-IP; without it we fall back to the
// socket's remote address (useful for local dev).
function clientIP(req: Request, fallback: string): string {
  const h = req.headers.get("fly-client-ip");
  if (h) return h.trim();
  const xff = req.headers.get("x-forwarded-for");
  if (xff) return xff.split(",")[0]!.trim();
  return fallback;
}

const server = Bun.serve<SocketData>({
  port: PORT,
  fetch(req, srv) {
    const url = new URL(req.url);
    if (url.pathname === "/ws") {
      const fallback = srv.requestIP(req)?.address ?? "0.0.0.0";
      const ip = clientIP(req, fallback);
      const upgraded = srv.upgrade(req, {
        data: { peer: null, publicIP: ip } as SocketData & { publicIP: string },
      });
      if (upgraded) return undefined;
      return new Response("upgrade failed", { status: 400 });
    }
    if (url.pathname === "/api/stats") {
      return Response.json(hub.stats());
    }
    if (url.pathname.startsWith("/api/rooms/")) {
      const name = decodeURIComponent(url.pathname.slice("/api/rooms/".length));
      if (!isValidRoomName(name)) {
        return Response.json({ error: "invalid room name" }, { status: 400 });
      }
      return Response.json({ room: name, peers: hub.list(name) });
    }
    if (url.pathname.startsWith("/room/")) {
      const name = decodeURIComponent(url.pathname.slice("/room/".length));
      if (!isValidRoomName(name)) {
        return new Response("invalid room name", { status: 400 });
      }
      return renderRoomPage(name, hub.list(name));
    }
    if (url.pathname === "/healthz") {
      return new Response("ok");
    }
    if (url.pathname === "/") {
      return new Response(
        `f2f-camp — rendezvous for f2f peers\n` +
          `WebSocket:  ${url.origin.replace(/^http/, "ws")}/ws\n` +
          `Stats:      ${url.origin}/api/stats\n` +
          `Room view:  ${url.origin}/room/<name>\n`,
        { headers: { "content-type": "text/plain" } },
      );
    }
    return new Response("not found", { status: 404 });
  },
  websocket: {
    open(_ws) {
      // Nothing to do until we get the hello. We rely on the client
      // sending hello within a few seconds — otherwise the socket idles
      // and Bun's default idleTimeout will close it.
    },
    message(ws, raw) {
      let msg: ClientMsg;
      try {
        msg = JSON.parse(typeof raw === "string" ? raw : raw.toString()) as ClientMsg;
      } catch {
        fail(ws, "bad_json", "message is not valid JSON");
        return;
      }
      if (!msg || typeof (msg as { type?: unknown }).type !== "string") {
        fail(ws, "bad_message", "missing type field");
        return;
      }

      // Hello is the only message allowed before identification.
      if (ws.data.peer == null) {
        if (msg.type !== "hello") {
          fail(ws, "no_hello", "send hello before anything else");
          return;
        }
        handleHello(ws, msg);
        return;
      }

      switch (msg.type) {
        case "hello":
          fail(ws, "already_hello", "already identified");
          return;
        case "announce":
          handleAnnounce(ws, msg);
          return;
        case "signal":
          handleSignal(ws, msg);
          return;
        case "list":
          handleList(ws);
          return;
        case "ping":
          send(ws, { type: "pong" });
          return;
        default: {
          const _exh: never = msg;
          fail(ws, "unknown_type", `unknown type: ${JSON.stringify(_exh)}`);
        }
      }
    },
    close(ws) {
      const peer = ws.data.peer;
      if (!peer) return;
      hub.leave(peer.room, peer.info.name);
      hub.broadcast(peer.room, { type: "peer-left", name: peer.info.name });
      console.log(`leave: ${peer.info.name}@${peer.room}`);
    },
    drain(_ws) {
      // Backpressure relief; nothing to do here, we don't queue large
      // payloads and send() drops failed writes silently.
    },
    // Reasonable defaults for a long-lived control socket.
    idleTimeout: 120, // seconds; clients should ping ~ every 60s
    maxPayloadLength: 1 << 20, // 1 MiB hard cap on a single message
  },
});

console.log(`f2f-camp listening on http://localhost:${server.port}`);

// STUN-like UDP responder for external endpoint discovery. Runs alongside
// the HTTP server; failure to bind shouldn't crash the WebSocket service.
let stunSocket: Awaited<ReturnType<typeof startStun>> | null = null;
try {
  stunSocket = await startStun(STUN_PORT);
} catch (err) {
  console.error(`stun: failed to bind UDP ${STUN_PORT}: ${(err as Error).message}`);
}

// ---- handlers ----

function handleHello(
  ws: ServerWebSocket<SocketData>,
  msg: Extract<ClientMsg, { type: "hello" }>,
) {
  const name = String(msg.name ?? "");
  const room = String(msg.room ?? "");
  if (!name || name.length > MAX_NAME_LEN || !NAME_RE.test(name)) {
    fail(ws, "bad_name", "name must match [A-Za-z0-9_.-]+ and be ≤64 chars");
    return;
  }
  if (!room || room.length > MAX_ROOM_LEN || !NAME_RE.test(room)) {
    fail(ws, "bad_room", "room must match [A-Za-z0-9_.-]+ and be ≤128 chars");
    return;
  }
  if (hub.has(room, name)) {
    fail(ws, "name_taken", `peer "${name}" already in room "${room}"`);
    return;
  }

  const publicIP = (ws.data as { publicIP?: string }).publicIP ?? "0.0.0.0";
  const info: PeerInfo = makeEndpoint({
    name,
    public_ip: publicIP,
    udp_port: msg.udp_port,
    tunnel_ip: "", // filled by hub.join
    joined_at: Date.now(),
  });
  const peer: Peer = { ws, room, info };
  let joined;
  try {
    joined = hub.join(room, peer);
  } catch (err) {
    fail(ws, "room_full", (err as Error).message);
    return;
  }
  ws.data.peer = peer;

  send(ws, { type: "welcome", you: info, room, peers: joined.existing });
  hub.broadcast(room, { type: "peer-joined", peer: info }, name);
  console.log(
    `join: ${name}@${room} ${joined.tunnelIP} from ${publicIP}${info.udp_port ? `:${info.udp_port}` : ""}`,
  );
}

function handleAnnounce(
  ws: ServerWebSocket<SocketData>,
  msg: Extract<ClientMsg, { type: "announce" }>,
) {
  const peer = ws.data.peer!;
  const port = Number(msg.udp_port);
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    fail(ws, "bad_port", "udp_port must be an integer in 1..65535");
    return;
  }
  peer.info = makeEndpoint({ ...peer.info, udp_port: port });
  hub.broadcast(peer.room, { type: "peer-updated", peer: peer.info });
}

function handleSignal(
  ws: ServerWebSocket<SocketData>,
  msg: Extract<ClientMsg, { type: "signal" }>,
) {
  const peer = ws.data.peer!;
  const room = (peer as Peer & { room: string }).room;
  const target = hub.get(room, String(msg.to));
  if (!target) {
    fail(ws, "no_peer", `no peer "${msg.to}" in room`);
    return;
  }
  send(target.ws, { type: "signal", from: peer.info.name, payload: msg.payload });
}

function handleList(ws: ServerWebSocket<SocketData>) {
  const peer = ws.data.peer!;
  const room = (peer as Peer & { room: string }).room;
  const peers = hub.list(room).filter((p) => p.name !== peer.info.name);
  send(ws, { type: "welcome", you: peer.info, room, peers });
}

function isValidRoomName(name: string): boolean {
  return name.length > 0 && name.length <= MAX_ROOM_LEN && NAME_RE.test(name);
}

function escapeHTML(s: string): string {
  return s.replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]!);
}
const HTML_ESCAPES: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

function ago(ts: number): string {
  const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

function renderRoomPage(roomName: string, peers: PeerInfo[]): Response {
  const rows = peers
    .map((p) => {
      const endpoint = p.udp_endpoint ?? `${p.public_ip}${p.udp_port ? ":" + p.udp_port : ""}`;
      return `      <tr>
        <td>${escapeHTML(p.name)}</td>
        <td>${escapeHTML(p.tunnel_ip || "—")}</td>
        <td>${escapeHTML(endpoint)}</td>
        <td class="muted">${ago(p.joined_at)}</td>
      </tr>`;
    })
    .join("\n");
  const body =
    peers.length === 0
      ? `<p class="muted">no peers in this room</p>`
      : `<table>
      <thead><tr><th>name</th><th>tunnel ip</th><th>udp endpoint</th><th>joined</th></tr></thead>
      <tbody>
${rows}
      </tbody>
    </table>`;
  const html = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="5">
  <title>f2f-camp · ${escapeHTML(roomName)}</title>
  <style>
    body { background: #111; color: #ddd; font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; padding: 24px; margin: 0; }
    h1 { font-size: 14px; color: #888; font-weight: normal; margin: 0 0 16px; }
    h1 strong { color: #ddd; font-weight: normal; }
    table { border-collapse: collapse; }
    th, td { padding: 4px 24px 4px 0; text-align: left; vertical-align: top; }
    th { color: #666; font-weight: normal; border-bottom: 1px solid #333; padding-bottom: 6px; }
    .muted { color: #666; }
  </style>
</head>
<body>
  <h1>room <strong>${escapeHTML(roomName)}</strong> · ${peers.length} peer${peers.length === 1 ? "" : "s"}</h1>
  ${body}
</body>
</html>
`;
  return new Response(html, {
    headers: { "content-type": "text/html; charset=utf-8" },
  });
}

// Graceful shutdown so fly.io rolling deploys don't leave dangling clients.
function shutdown(signal: string) {
  console.log(`received ${signal}; closing`);
  server.stop(true);
  if (stunSocket) stunSocket.close();
  process.exit(0);
}
process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));
