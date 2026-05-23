// UDP announce listener — replaces the old STUN-only probe responder.
// A peer sends `{t:"announce", name, camp_id}` on this socket; we read
// the source address off the packet itself (no need for a separate STUN
// step), upsert the peer into the hub, and reply with `{t:"announced",
// you:PeerInfo}` so the client learns its tunnel_ip and reflex.
//
// One UDP packet does three jobs at once:
//   1. registers / refreshes the peer entry (driven by the periodic
//      cadence on the client side);
//   2. lets us observe the public endpoint via srcAddr / srcPort,
//      replacing a dedicated STUN exchange;
//   3. keeps the camp-facing NAT mapping alive on the client's tunnel
//      port — relevant under endpoint-dependent NATs where the
//      peer-facing mapping doesn't help us.

import type { Hub } from "./hub";
import type { AnnouncedResp, AnnounceErr, AnnounceReq } from "./types";

const NAME_RE = /^[A-Za-z0-9_.-]+$/;
const PUB_RE = /^[a-f0-9]{64}$/;
const MAX_NAME_LEN = 64;
const MAX_CAMP_ID_LEN = 128;

function isValidAnnounce(x: unknown): x is AnnounceReq {
  if (typeof x !== "object" || x === null) return false;
  const m = x as Record<string, unknown>;
  if (m.t !== "announce") return false;
  if (typeof m.name !== "string") return false;
  if (typeof m.camp_id !== "string") return false;
  // pub is optional; if present must be a string. Format check happens
  // in the handler so we can return a structured error.
  if (m.pub !== undefined && typeof m.pub !== "string") return false;
  return true;
}

// On fly.io UDP packets only reach a Machine if you bind to the special
// `fly-global-services` address — 0.0.0.0 silently drops them. Anywhere
// else (local dev, plain Docker) we bind to 0.0.0.0. Auto-detect via
// FLY_APP_NAME, with STUN_BIND escape hatch.
function pickBindAddress(): string {
  const explicit = Bun.env.STUN_BIND?.trim();
  if (explicit) return explicit;
  return Bun.env.FLY_APP_NAME ? "fly-global-services" : "0.0.0.0";
}

export async function startUDP(port: number, hub: Hub) {
  const hostname = pickBindAddress();
  const socket = await Bun.udpSocket({
    port,
    hostname,
    socket: {
      async data(sock, buf, srcPort, srcAddr) {
        // Hard cap on payload size — both as a sanity check and to keep
        // reflection amplification cheap.
        if (buf.length > 1024) {
          console.log(`udp: drop oversize ${buf.length}B from ${srcAddr}:${srcPort}`);
          return;
        }

        let msg: unknown;
        try {
          msg = JSON.parse(buf.toString("utf8"));
        } catch {
          return; // silent — random scanners send junk
        }
        if (!isValidAnnounce(msg)) {
          return;
        }

        const name = msg.name;
        const campID = msg.camp_id;
        const pub = msg.pub ?? "";
        if (!name || name.length > MAX_NAME_LEN || !NAME_RE.test(name)) {
          sendErr(sock, srcAddr, srcPort, "bad_name", `invalid name`);
          return;
        }
        if (!campID || campID.length > MAX_CAMP_ID_LEN || !NAME_RE.test(campID)) {
          sendErr(sock, srcAddr, srcPort, "bad_camp_id", `invalid camp_id`);
          return;
        }
        // pub became the primary identity. Reject clients that don't
        // send one — at this point in the rollout every supported
        // client (mac, eventually win) generates an ed25519 keypair.
        if (!pub) {
          sendErr(sock, srcAddr, srcPort, "pub_required", "client must announce ed25519 pub");
          return;
        }
        if (!PUB_RE.test(pub)) {
          sendErr(sock, srcAddr, srcPort, "bad_pub", `invalid pub (expect 64 hex)`);
          return;
        }

        const wasNew = !hub.has(campID, pub);
        let info;
        try {
          info = await hub.upsert(campID, pub, name, srcAddr, srcPort);
        } catch (err) {
          sendErr(sock, srcAddr, srcPort, "camp_full", (err as Error).message);
          return;
        }
        if (wasNew) {
          console.log(
            `join: ${name}@${campID} ${info.tunnel_ip} pub=${pub.slice(0, 16)} from ${srcAddr}:${srcPort}`,
          );
        }

        const reply: AnnouncedResp = { t: "announced", you: info };
        sock.send(JSON.stringify(reply), srcPort, srcAddr);
      },
    },
  });

  console.log(`udp: ${hostname}:${socket.port} (announce)`);
  return socket;
}

function sendErr(
  sock: { send: (data: string, port: number, addr: string) => void },
  addr: string,
  port: number,
  code: string,
  message: string,
) {
  const reply: AnnounceErr = { t: "error", code, message };
  try {
    sock.send(JSON.stringify(reply), port, addr);
  } catch {
    /* socket closed; nothing to do */
  }
  console.log(`udp: ${addr}:${port} ← err ${code}`);
}
