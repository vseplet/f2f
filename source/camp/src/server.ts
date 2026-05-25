// f2f-camp — rendezvous server for f2f peers.
//
// Peers register via UDP announce on STUN_PORT (see ./stun.ts).
// HTTP is read-only: /api/id/:id for peer list, /id/:id for humans.
// State is in-memory, single-instance.

import { Hub } from "./hub";
import { startUDP } from "./stun";
import type { PeerInfo } from "./types";

const PORT = Number(Bun.env.PORT ?? 8080);
const STUN_PORT = Number(Bun.env.STUN_PORT ?? 3478);
const EVICT_AFTER_MS = 60_000;
const EVICT_INTERVAL_MS = 10_000;
const MAX_CAMP_ID_LEN = 128;
const NAME_RE = /^[A-Za-z0-9_.-]+$/;

const hub = new Hub();

const server = Bun.serve({
  port: PORT,
  fetch(req, _srv) {
    const url = new URL(req.url);
    if (url.pathname === "/api/stats") {
      return Response.json(hub.stats());
    }
    if (url.pathname.startsWith("/api/id/")) {
      const id = decodeURIComponent(url.pathname.slice("/api/id/".length));
      if (!isValidCampID(id)) {
        return Response.json({ error: "invalid camp id" }, { status: 400 });
      }
      return Response.json({ camp_id: id, peers: hub.list(id), now: Date.now() });
    }
    if (url.pathname.startsWith("/id/")) {
      const id = decodeURIComponent(url.pathname.slice("/id/".length));
      if (!isValidCampID(id)) {
        return new Response("invalid camp id", { status: 400 });
      }
      return renderCampPage(id, hub.list(id));
    }
    if (url.pathname === "/healthz") {
      return new Response("ok");
    }
    if (url.pathname === "/") {
      return new Response(
        `f2f-camp — rendezvous for f2f peers\n` +
          `Announce:  udp ${url.host.replace(/:\d+$/, "")}:${STUN_PORT}\n` +
          `Stats:     ${url.origin}/api/stats\n` +
          `Camp view: ${url.origin}/id/<camp-id>\n`,
        { headers: { "content-type": "text/plain" } },
      );
    }
    return new Response("not found", { status: 404 });
  },
});

console.log(`f2f-camp http listening on http://localhost:${server.port}`);

try {
  await startUDP(STUN_PORT, hub);
} catch (err) {
  console.error(`udp: failed to bind ${STUN_PORT}: ${(err as Error).message}`);
}

setInterval(() => {
  hub.evictStale(Date.now() - EVICT_AFTER_MS);
}, EVICT_INTERVAL_MS);

function isValidCampID(name: string): boolean {
  return name.length > 0 && name.length <= MAX_CAMP_ID_LEN && NAME_RE.test(name);
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

function renderCampPage(campID: string, peers: PeerInfo[]): Response {
  const rows = peers
    .map((p) => {
      const endpoint = p.udp_endpoint ?? `${p.public_ip}${p.udp_port ? ":" + p.udp_port : ""}`;
      const fp = p.pub ? p.pub.slice(0, 16) : "—";
      return `      <tr>
        <td>${escapeHTML(p.name)}</td>
        <td class="muted">${escapeHTML(fp)}</td>
        <td>${escapeHTML(endpoint)}</td>
        <td class="muted">${ago(p.joined_at)}</td>
      </tr>`;
    })
    .join("\n");
  const body =
    peers.length === 0
      ? `<p class="muted">no peers in this camp</p>`
      : `<table>
      <thead><tr><th>name</th><th>fingerprint</th><th>udp endpoint</th><th>joined</th></tr></thead>
      <tbody>
${rows}
      </tbody>
    </table>`;
  const renderedAt = new Date().toISOString().replace("T", " ").slice(0, 19) + " UTC";
  const html = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="5">
  <title>f2f-camp · ${escapeHTML(campID)}</title>
  <style>
    body { background: #111; color: #ddd; font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; padding: 24px; margin: 0; }
    h1 { font-size: 14px; color: #888; font-weight: normal; margin: 0 0 16px; }
    h1 strong { color: #ddd; font-weight: normal; }
    table { border-collapse: collapse; }
    th, td { padding: 4px 24px 4px 0; text-align: left; vertical-align: top; }
    th { color: #666; font-weight: normal; border-bottom: 1px solid #333; padding-bottom: 6px; }
    .muted { color: #666; }
    footer { color: #555; font-size: 12px; margin-top: 24px; }
  </style>
</head>
<body>
  <h1>camp <strong>${escapeHTML(campID)}</strong> · ${peers.length} peer${peers.length === 1 ? "" : "s"}</h1>
  ${body}
  <footer>data as of ${renderedAt} · refreshes every 5s</footer>
</body>
</html>
`;
  return new Response(html, {
    headers: { "content-type": "text/html; charset=utf-8" },
  });
}

function shutdown(signal: string) {
  console.log(`received ${signal}; closing`);
  server.stop(true);
  process.exit(0);
}
process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));
