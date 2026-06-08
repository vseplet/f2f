// term.js — remote terminal (services/shell over the bus), rendered with
// xterm.js in the main column as the #tab-term panel (like the other tabs).
// Opened from the sidebar "machines" rows, which route to #term:<pub>.
//
// The session id is persistent per peer (localStorage), so a reload or a
// sleep/wake reattaches to the SAME PTY on the host — the server replays the
// recent screen, no garbage. The WebSocket auto-reconnects while the tab is
// open, which is what makes it survive the gap.
(function () {
  let term = null, fit = null, ws = null;
  let curPub = '', curSession = '';
  let reconnectTimer = null, closing = false;

  const elBody = () => document.getElementById('term-body');
  const elTitle = () => document.getElementById('term-title');
  const elStatus = () => document.getElementById('term-status');

  function setStatus(t) { const e = elStatus(); if (e) e.textContent = t || ''; }

  function sessionFor(pub) {
    const k = 'f2f.term.' + pub;
    let s = '';
    try { s = localStorage.getItem(k) || ''; } catch (_) {}
    if (!s) {
      s = pub.slice(0, 8) + '-' + Math.random().toString(36).slice(2, 10);
      try { localStorage.setItem(k, s); } catch (_) {}
    }
    return s;
  }

  // showTermTab activates #tab-term in the main column (same mechanism as the
  // other tabs: hide all panels, clear active tab chips, show ours).
  function showTermTab() {
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.add('hidden'));
    document.querySelectorAll('.ax-tab').forEach((t) => t.classList.remove('ax-tab-active'));
    const t = document.getElementById('tab-term');
    if (t) t.classList.remove('hidden');
  }

  function ensureTerm() {
    if (term || !window.Terminal) return;
    term = new window.Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: { background: '#0b0e14', foreground: '#cbd3e1' },
    });
    if (window.FitAddon && window.FitAddon.FitAddon) {
      fit = new window.FitAddon.FitAddon();
      term.loadAddon(fit);
    }
    term.open(elBody());
    term.onData((d) => {
      if (ws && ws.readyState === 1) ws.send(new TextEncoder().encode(d));
    });
    term.onResize(({ cols, rows }) => {
      if (ws && ws.readyState === 1) ws.send(JSON.stringify({ t: 'resize', cols, rows }));
    });
    window.addEventListener('resize', () => {
      const t = document.getElementById('tab-term');
      if (t && !t.classList.contains('hidden')) doFit();
    });
  }

  function doFit() { if (fit) { try { fit.fit(); } catch (_) {} } }

  function connect() {
    if (closing || !term) return;
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    const url = proto + location.host + '/api/shell/ws'
      + '?peer=' + encodeURIComponent(curPub)
      + '&session=' + encodeURIComponent(curSession)
      + '&cols=' + (term.cols || 80) + '&rows=' + (term.rows || 24);
    setStatus('connecting…');
    ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => { setStatus('connected'); doFit(); term.focus(); };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') term.write(e.data);
      else term.write(new Uint8Array(e.data));
    };
    ws.onerror = () => { try { ws.close(); } catch (_) {} };
    ws.onclose = () => {
      ws = null;
      if (closing) return;
      // Reattach (sleep/gap survival): the host kept the PTY; on reconnect it
      // replays the screen from its ring buffer.
      setStatus('disconnected — reattaching…');
      reconnectTimer = setTimeout(() => { if (!closing) connect(); }, 1000);
    };
  }

  function open(pub) {
    showTermTab();
    ensureTerm();
    if (!term) { setStatus('xterm not loaded'); return; }
    closing = false;
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    if (pub !== curPub) {
      if (ws) { try { ws.close(); } catch (_) {} ws = null; }
      term.reset();
    }
    curPub = pub;
    curSession = sessionFor(pub);
    elTitle().textContent = '— terminal · ' + pub.slice(0, 12);
    setTimeout(doFit, 0);
    if (!ws) connect();
  }

  // disconnect drops the client WS but leaves the host PTY running (so it can
  // be reattached later). Used both on close and on navigating away.
  function disconnect() {
    closing = true;
    if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
    if (ws) { try { ws.close(); } catch (_) {} ws = null; }
    curPub = '';
    setStatus('');
  }

  // closeButton: leave the terminal tab and go back to the default tab.
  function closeButton() {
    disconnect();
    if ((location.hash || '').indexOf('#term:') === 0) location.hash = '';
    const t = document.getElementById('tab-term');
    if (t) t.classList.add('hidden');
    const first = document.querySelector('.ax-tab');
    if (first) first.click(); // back to the default tab
  }

  function applyTermRoute() {
    const m = (location.hash || '').replace(/^#/, '').match(/^term:(.+)$/);
    if (m) open(decodeURIComponent(m[1]));
    else if (curPub) disconnect(); // navigated away → drop client, host keeps the PTY
  }

  function init() {
    const btn = document.getElementById('term-close');
    if (btn) btn.addEventListener('click', closeButton);
    window.addEventListener('hashchange', applyTermRoute);
    applyTermRoute();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
