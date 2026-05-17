// Minimal STUN-like UDP probe responder. A peer sends a small JSON probe
// to our UDP port; we reply with what we see as its source IP and port.
// This is what lets peers behind symmetric NAT learn their *actual*
// external UDP endpoint — which often differs from the local socket
// port — and report it back via WebSocket for rendezvous.
//
// Why not real STUN (RFC 5389)? It's overkill: we only need the
// "mapped-address" attribute, and a custom JSON ping is easier to
// debug, requires no codec, and stays inside our own tooling.

type ProbeMsg = { t: "probe"; id: string };
type ReflexMsg = { t: "reflex"; id: string; ip: string; port: number };

function isValidProbe(x: unknown): x is ProbeMsg {
  if (typeof x !== "object" || x === null) return false;
  const m = x as Record<string, unknown>;
  return m.t === "probe" && typeof m.id === "string" && m.id.length > 0 && m.id.length <= 64;
}

export async function startStun(port: number) {
  const socket = await Bun.udpSocket({
    port,
    socket: {
      data(sock, buf, srcPort, srcAddr) {
        // Cap payload aggressively so reflection amplification can't
        // happen — the reply is ~80 bytes and we won't parse anything
        // larger than that anyway.
        if (buf.length > 256) return;

        let msg: unknown;
        try {
          msg = JSON.parse(buf.toString("utf8"));
        } catch {
          return; // not our protocol — silently drop
        }
        if (!isValidProbe(msg)) return;

        const reply: ReflexMsg = {
          t: "reflex",
          id: msg.id,
          ip: srcAddr,
          port: srcPort,
        };
        sock.send(JSON.stringify(reply), srcPort, srcAddr);
      },
    },
  });

  console.log(`stun: udp ${socket.hostname}:${socket.port}`);
  return socket;
}
