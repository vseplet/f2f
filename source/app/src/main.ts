// Standalone f2f desktop UI. Polls the local helper's loopback API via the Tauri
// http plugin (Rust-side fetch → no CORS problems) and offers start/stop of the
// privileged helper through Rust commands (see src-tauri/src/lib.rs).
import { fetch } from "@tauri-apps/plugin-http";
import { invoke } from "@tauri-apps/api/core";

const BASE = "http://127.0.0.1:2202";

type Peer = {
  name: string;
  fp?: string;
  role?: string;
  overlay_v4?: string;
  udp_endpoint?: string;
  rtt_ms?: number;
  self?: boolean;
  paired?: boolean;
  half_paired?: boolean;
  in_camp?: boolean;
};
type Status = {
  running: boolean;
  camp_label?: string;
  local_ip?: string;
  identity_fp?: string;
  camp_reflex?: string;
  peers?: Peer[];
};

const $ = (id: string) => document.getElementById(id)!;

function marker(p: Peer): string {
  if (p.self) return "you";
  if (p.paired) return "🟢";
  if (p.half_paired) return "🟡";
  if (p.in_camp) return "🔴";
  return "⚪";
}

function escapeHtml(s: string): string {
  return s.replace(
    /[&<>"']/g,
    (c) =>
      ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
      })[c]!,
  );
}

function render(s: Status) {
  $("err").textContent = "";
  $("dot").className = "dot " + (s.running ? "on" : "off");
  $("state-text").textContent = s.running
    ? `running · ${s.camp_label ?? "?"}`
    : "stopped";
  $("summary").innerHTML = s.running
    ? `<span>overlay <b>${escapeHtml(s.local_ip ?? "—")}</b></span>
       <span>fp <b>${escapeHtml(s.identity_fp ?? "—")}</b></span>
       <span>reflex <b>${escapeHtml(s.camp_reflex ?? "—")}</b></span>`
    : `<span class="muted">helper is not running</span>`;

  const peers = s.peers ?? [];
  $("peers-body").innerHTML =
    peers
      .map(
        (p) => `<tr>
      <td>${marker(p)}</td>
      <td>${escapeHtml(p.name)}</td>
      <td>${escapeHtml(p.role ?? "-")}</td>
      <td>${escapeHtml(p.overlay_v4 ?? "—")}</td>
      <td>${escapeHtml(p.udp_endpoint ?? "—")}</td>
      <td>${p.rtt_ms ? p.rtt_ms + "ms" : ""}</td>
    </tr>`,
      )
      .join("") || `<tr><td colspan="6" class="muted">no peers</td></tr>`;
}

function renderOffline(reason: string) {
  $("dot").className = "dot off";
  $("state-text").textContent = "helper unreachable";
  $("summary").innerHTML =
    `<span class="muted">can't reach the helper on ${BASE}</span>`;
  $("peers-body").innerHTML = "";
  $("err").textContent = reason;
}

async function refresh() {
  try {
    const r = await fetch(`${BASE}/api/status`, { method: "GET" });
    if (!r.ok) throw new Error(`status ${r.status}`);
    render((await r.json()) as Status);
  } catch (e) {
    renderOffline(String(e));
  }
  // Offer install only when the binary isn't on PATH.
  try {
    const bin = (await invoke("helper_bin")) as string | null;
    ($("btn-install") as HTMLButtonElement).hidden = !!bin;
    ($("btn-start") as HTMLButtonElement).disabled = !bin;
  } catch {
    /* helper_bin unavailable — leave buttons as-is */
  }
}

window.addEventListener("DOMContentLoaded", () => {
  $("btn-install").addEventListener("click", async () => {
    try {
      await invoke("helper_install");
    } catch (e) {
      $("err").textContent = String(e);
    }
  });
  $("btn-start").addEventListener("click", async () => {
    try {
      await invoke("helper_start");
    } catch (e) {
      $("err").textContent = String(e);
    }
  });
  $("btn-stop").addEventListener("click", async () => {
    try {
      await invoke("helper_stop");
    } catch (e) {
      $("err").textContent = String(e);
    }
  });
  refresh();
  setInterval(refresh, 3000);
});
