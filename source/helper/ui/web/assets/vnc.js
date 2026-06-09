// vnc.js — remote desktop (services/vnc over the bus), rendered with noVNC
// in the main column as the #tab-vnc panel. Opened from the sidebar
// "desktops" rows, which route to #vnc:<pub>.
//
// noVNC is vendored (ESM core/ under /vendor/novnc) — offline, no CDN. RFB
// opens its own WebSocket to our local bridge, which pipes the raw RFB stream
// over the bus to the peer's VNC server — the RFB handshake (incl. password
// auth) is end-to-end.
// noVNC ESM source (core/), vendored under /vendor/novnc — offline, no CDN.
// (The npm `lib/` build is CommonJS; the git `core/` tree is clean ES modules
// with `export default class RFB`.)
import RFB from '/vendor/novnc/core/rfb.js';

(function () {
  let rfb = null, curPub = '';

  const elBody = () => document.getElementById('vnc-body');
  const elTitle = () => document.getElementById('vnc-title');
  const elStatus = () => document.getElementById('vnc-status');
  function setStatus(t) { const e = elStatus(); if (e) e.textContent = t || ''; }

  // promptCreds shows a small modal with the requested fields (password is
  // masked, unlike window.prompt). Resolves to {username?, password?} or null
  // on cancel.
  function closeCredModal() {
    const m = document.getElementById('vnc-cred-modal');
    if (m) m.remove();
  }

  function promptCreds(types) {
    return new Promise((resolve) => {
      closeCredModal(); // never stack two
      const wrap = document.createElement('div');
      wrap.id = 'vnc-cred-modal';
      wrap.style.cssText = 'position:fixed;inset:0;z-index:10000;display:flex;align-items:center;justify-content:center;background:rgba(0,0,0,.55)';
      const box = document.createElement('div');
      box.style.cssText = 'background:#11151c;border:1px solid #2a3140;border-radius:8px;padding:18px;min-width:280px;color:#cbd3e1;font:inherit;display:flex;flex-direction:column;gap:10px';
      box.innerHTML = '<div style="font-weight:bold">VNC authentication</div>';
      const inputs = {};
      const mk = (type, name) => {
        const lab = document.createElement('label');
        lab.style.cssText = 'display:flex;flex-direction:column;gap:4px;font-size:12px;color:#9aa6bb';
        lab.textContent = name;
        const inp = document.createElement('input');
        inp.type = type; inp.autocomplete = type === 'password' ? 'current-password' : 'username';
        inp.style.cssText = 'background:#0b0e14;border:1px solid #2a3140;border-radius:5px;padding:6px 8px;color:#cbd3e1;font:inherit';
        lab.appendChild(inp); box.appendChild(lab); inputs[name] = inp;
      };
      if (types.includes('username')) mk('text', 'username');
      if (types.includes('password')) mk('password', 'password');
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;gap:8px;justify-content:flex-end;margin-top:4px';
      const mkBtn = (txt) => { const b = document.createElement('button'); b.type = 'button'; b.textContent = txt; b.style.cssText = 'border:1px solid #2a3140;border-radius:5px;padding:4px 12px;background:#1a1f29;color:#cbd3e1;font:inherit;cursor:pointer'; return b; };
      const cancel = mkBtn('cancel'); const ok = mkBtn('connect'); ok.style.borderColor = '#356'; ok.style.color = '#9cf';
      row.appendChild(cancel); row.appendChild(ok); box.appendChild(row);
      wrap.appendChild(box); document.body.appendChild(wrap);
      const first = inputs.username || inputs.password; if (first) first.focus();
      const done = (v) => { closeCredModal(); resolve(v); };
      cancel.onclick = () => done(null);
      ok.onclick = () => {
        const c = {};
        if (inputs.username) c.username = inputs.username.value;
        if (inputs.password) c.password = inputs.password.value;
        done(c);
      };
      wrap.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); ok.click(); }
        else if (e.key === 'Escape') { e.preventDefault(); cancel.click(); }
      });
    });
  }

  function showVncTab() {
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.add('hidden'));
    document.querySelectorAll('.ax-tab').forEach((t) => t.classList.remove('ax-tab-active'));
    const t = document.getElementById('tab-vnc');
    if (t) t.classList.remove('hidden');
  }

  function disconnect() {
    closeCredModal();
    if (rfb) { try { rfb.disconnect(); } catch (_) {} rfb = null; }
    curPub = '';
    setStatus('');
    const b = elBody();
    if (b) b.innerHTML = ''; // RFB owns the container; clear it for the next open
  }

  function connect(pub) {
    disconnect();
    curPub = pub;
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    const url = proto + location.host + '/api/vnc/ws?peer=' + encodeURIComponent(pub);
    setStatus('connecting…');
    rfb = new RFB(elBody(), url);
    rfb.viewOnly = false;      // full control (mouse + keyboard)
    rfb.scaleViewport = true;  // fit the remote framebuffer to the panel
    rfb.background = '#0b0e14';
    rfb.showDotCursor = true;  // always show a cursor (dot fallback) — noVNC hides
                               // the local one and draws the remote cursor; without
                               // this you get no cursor when the remote shape is absent
    applyQuality();            // picture quality / bandwidth (Tight/JPEG)
    rfb.addEventListener('connect', () => setStatus('connected'));
    rfb.addEventListener('disconnect', (e) => {
      setStatus(e.detail && e.detail.clean ? 'disconnected' : 'disconnected (error)');
    });
    rfb.addEventListener('credentialsrequired', (e) => {
      // macOS Screen Sharing (Apple ARD, security type 30) needs username +
      // password; plain VNC auth needs only a password. Ask exactly what the
      // server requested via a masked modal; Cancel gives up (sending
      // incomplete creds would re-trigger this event forever).
      const types = (e.detail && e.detail.types) || ['password'];
      promptCreds(types).then((c) => {
        if (c === null) { setStatus('auth cancelled'); try { rfb.disconnect(); } catch (_) {} return; }
        try { rfb.sendCredentials(c); } catch (_) {}
      });
    });
    rfb.addEventListener('securityfailure', (e) => {
      setStatus('auth failed' + (e.detail && e.detail.reason ? ': ' + e.detail.reason : ''));
    });
  }

  function open(pub) {
    showVncTab();
    elTitle().textContent = '— desktop · ' + pub.slice(0, 12);
    if (pub !== curPub || !rfb) connect(pub);
  }

  function disconnectButton() {
    disconnect();
    if ((location.hash || '').indexOf('#vnc:') === 0) location.hash = '';
    const t = document.getElementById('tab-vnc');
    if (t) t.classList.add('hidden');
    const first = document.querySelector('.ax-tab');
    if (first) first.click();
  }

  // applyQuality maps the quality dropdown to noVNC's JPEG quality (0-9) and
  // zlib compression (0-9). Lower quality + higher compression = less
  // bandwidth, softer picture. Applies live; only effective when the server
  // uses Tight/JPEG encoding.
  function applyQuality() {
    if (!rfb) return;
    const sel = document.getElementById('vnc-quality');
    const v = (sel && sel.value) || 'medium';
    const map = {
      high: { q: 8, c: 2 },
      medium: { q: 5, c: 4 },
      low: { q: 2, c: 7 },
      ultra: { q: 0, c: 9 }, // floor for Tight/JPEG: lowest quality, max compression
    };
    const m = map[v] || map.medium;
    try { rfb.qualityLevel = m.q; rfb.compressionLevel = m.c; } catch (_) {}
  }

  function toggleFullscreen() {
    const el = elBody();
    if (document.fullscreenElement) {
      if (document.exitFullscreen) document.exitFullscreen();
    } else if (el && el.requestFullscreen) {
      el.requestFullscreen().catch(() => {});
    }
  }

  function applyVncRoute() {
    const m = (location.hash || '').replace(/^#/, '').match(/^vnc:(.+)$/);
    if (m) open(decodeURIComponent(m[1]));
    // Switching to another tab no longer disconnects: the RFB session (and its
    // authenticated VNC connection) is kept alive in the background — the panel
    // just gets hidden — so coming back doesn't force a re-login. It's torn down
    // only by the explicit "disconnect" button or when opening another peer.
  }

  function init() {
    const d = document.getElementById('vnc-disconnect');
    if (d) d.addEventListener('click', disconnectButton);
    const fs = document.getElementById('vnc-fs');
    if (fs) fs.addEventListener('click', toggleFullscreen);
    const q = document.getElementById('vnc-quality');
    if (q) q.addEventListener('change', applyQuality);
    window.addEventListener('hashchange', applyVncRoute);
    applyVncRoute();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
