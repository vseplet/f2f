import './style.css';

import { Start, Stop, Status, SendSignal } from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

// ---- tab switching ----
$$('.ax-tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    const target = btn.dataset.tab;
    $$('.ax-tab').forEach((b) => b.classList.toggle('ax-tab-active', b === btn));
    $$('.tab-panel').forEach((p) => p.classList.toggle('hidden', p.id !== 'tab-' + target));
  });
});

// ---- engine button ----
const $engineBtn = $('#btn-engine');
const $engineState = $engineBtn.querySelector('.ax-engine-state');
const $engineLabel = $engineBtn.querySelector('.ax-engine-label');
const $engineMeta = $('#engine-meta');

function setEngineBtn(state, label, meta) {
  $engineBtn.className = 'ax-engine-btn state-' + state;
  $engineBtn.title = label;
  const icons = { running: '■', stopped: '▶', loading: '⋯', error: '!' };
  $engineState.textContent = icons[state] || '·';
  $engineLabel.textContent = label;
  $engineMeta.textContent = meta || '';
}

let pendingOp = false;
$engineBtn.addEventListener('click', async () => {
  if (pendingOp) return;
  const s = await Status().catch(() => null);
  if (s && s.running) {
    pendingOp = true;
    setEngineBtn('loading', 'stopping…', '');
    try {
      await Stop();
    } catch (e) {
      alert('stop failed: ' + (e?.message || e));
    } finally {
      pendingOp = false;
      refresh();
    }
    return;
  }
  const name = $('#camp-name').value.trim();
  const id = $('#camp-id').value.trim();
  if (!name || !id) { alert('name and camp id required'); return; }
  pendingOp = true;
  setEngineBtn('loading', 'starting…', '');
  try {
    await Start({ name, id });
  } catch (e) {
    alert('start failed: ' + (e?.message || e));
  } finally {
    pendingOp = false;
    refresh();
  }
});

// ---- peer rendering ----
function fmtAgo(ms) {
  if (!ms) return '—';
  const dt = Date.now() - ms;
  if (dt < 1000) return 'now';
  if (dt < 60000) return Math.floor(dt / 1000) + 's';
  if (dt < 3600000) return Math.floor(dt / 60000) + 'm';
  return Math.floor(dt / 3600000) + 'h';
}

function dotClass(p) {
  if (p.self) return 'self';
  if (!p.online) return 'offline';
  if (!p.reachable) return 'unreachable';
  return 'reachable';
}

async function refresh() {
  let s;
  try { s = await Status(); } catch { return; }
  if (s.running) {
    setEngineBtn('running', 'running', `· ${s.tunnel_ip || '?'}`);
    $('#camp-name').disabled = true;
    $('#camp-id').disabled = true;
    $('#camp-name').value = s.name || $('#camp-name').value;
    $('#camp-id').value = s.camp_id || $('#camp-id').value;
  } else {
    setEngineBtn('stopped', 'start', '');
    $('#camp-name').disabled = false;
    $('#camp-id').disabled = false;
  }

  const peers = Array.isArray(s.peers) ? s.peers : [];
  $('#peers-meta').textContent = peers.length ? peers.length + ' peer(s)' : '';
  const $status = $('#peers-status');
  const $table = $('#peers-table');
  if (!s.running) {
    $status.textContent = 'stopped';
    $status.classList.remove('hidden');
    $table.classList.add('hidden');
  } else if (peers.length <= 1) {
    $status.textContent = 'announced; waiting for other peers…';
    $status.classList.remove('hidden');
    $table.classList.add('hidden');
  } else {
    $status.classList.add('hidden');
    $table.classList.remove('hidden');
  }

  const $body = $('#peers-tbody');
  $body.innerHTML = '';
  for (const p of peers) {
    const tr = document.createElement('tr');
    if (p.self) tr.classList.add('self');
    tr.innerHTML = `
      <td class="ax-peers-dot"><span class="ax-dot ${dotClass(p)}"></span></td>
      <td>${escapeHtml(p.name || '?')}</td>
      <td>${escapeHtml(p.tunnel_ip || '')}</td>
      <td>${escapeHtml(p.udp_endpoint || p.public_ip || '')}</td>
      <td>${p.self ? 'self' : fmtAgo(p.last_seen_ms)}</td>
    `;
    $body.appendChild(tr);
  }

  // populate meet's peer select with non-self online peers
  const $sel = $('#ax-call-peer');
  const prev = $sel.value;
  $sel.innerHTML = '<option value="">— peer —</option>';
  for (const p of peers) {
    if (p.self) continue;
    const opt = document.createElement('option');
    opt.value = p.tunnel_ip;
    opt.textContent = p.name;
    $sel.appendChild(opt);
  }
  $sel.value = prev;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

refresh();
setInterval(refresh, 2000);

// ---- meet tab: signal transport ----
const $logBody = $('#ax-log-body');
const $logCount = $('#ax-log-count');
const $callBtn = $('#ax-call-btn');
const $callMeta = $('#ax-call-meta');
const $callSel = $('#ax-call-peer');
let logLines = 0;

function meetLog(line) {
  logLines++;
  $logCount.textContent = logLines;
  const div = document.createElement('div');
  div.textContent = line;
  $logBody.appendChild(div);
  $logBody.scrollTop = $logBody.scrollHeight;
}

$('#ax-log-clear').addEventListener('click', () => {
  $logBody.innerHTML = '';
  logLines = 0;
  $logCount.textContent = '0';
});

// Subscribe to signal-frames from peers. Body is currently a plain
// string; for WebRTC we'll JSON-parse it (Step 2) — for now we just
// log so the transport can be verified end-to-end.
EventsOn('signal', (msg) => {
  meetLog(`← from ${msg.from}: ${msg.body}`);
});

$callBtn.addEventListener('click', async () => {
  const to = $callSel.value;
  if (!to) {
    $callMeta.textContent = 'no peer selected';
    return;
  }
  const body = `ping from desktop ${Date.now()}`;
  try {
    await SendSignal(to, body);
    meetLog(`→ to ${to}: ${body}`);
    $callMeta.textContent = '';
  } catch (e) {
    $callMeta.textContent = 'send failed';
    meetLog(`! send to ${to} failed: ${e?.message || e}`);
  }
});
