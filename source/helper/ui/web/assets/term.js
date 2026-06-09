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
      // 'Symbols Nerd Font' last: a per-glyph fallback so powerline/devicon
      // glyphs in TUIs render, while normal text stays in the system mono.
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, "Symbols Nerd Font", monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: { background: '#0b0e14', foreground: '#cbd3e1' },
    });
    if (window.FitAddon && window.FitAddon.FitAddon) {
      fit = new window.FitAddon.FitAddon();
      term.loadAddon(fit);
    }
    term.open(elBody());
    window.__term = term; // DEBUG: inspect term.modes.mouseTrackingMode in console
    term.onData((d) => {
      // DEBUG: surface mouse reports xterm emits (ESC[< = SGR, ESC[M = X10).
      if (d.indexOf('\x1b[<') === 0 || d.indexOf('\x1b[M') === 0) {
        console.log('[term] mouse-out', JSON.stringify(d), 'mode=', term.modes && term.modes.mouseTrackingMode);
      }
      if (ws && ws.readyState === 1) ws.send(new TextEncoder().encode(d));
    });
    term.onResize(({ cols, rows }) => {
      console.log('[term] resize', cols, rows); // DEBUG
      if (ws && ws.readyState === 1) ws.send(JSON.stringify({ t: 'resize', cols, rows }));
    });
    // DEBUG: on every mousedown over the terminal, report what xterm thinks the
    // mouse mode is — tells us if the mode was lost vs reports just not flowing.
    elBody().addEventListener('mousedown', () => {
      console.log('[term] mousedown · mouseTrackingMode=', term.modes && term.modes.mouseTrackingMode);
    }, true);
    window.addEventListener('resize', () => {
      const t = document.getElementById('tab-term');
      if (t && !t.classList.contains('hidden')) doFit();
    });
    // Refit on ANY container size change, not just window resize — dragging the
    // sidebar resizes the main column without firing a window 'resize' event, so
    // without this the grid keeps its old cols/rows and text spills off the edge.
    if (window.ResizeObserver) {
      let raf = 0;
      const ro = new ResizeObserver(() => {
        if (raf) return; // coalesce the burst of events during a drag
        raf = requestAnimationFrame(() => { raf = 0; doFit(); });
      });
      ro.observe(elBody());
    }
  }

  function doFit() {
    if (!fit) return;
    // Never fit a hidden/zero-size container: FitAddon then computes NaN cols/
    // rows and xterm.resize(NaN, NaN) corrupts the grid geometry, which breaks
    // mouse-coordinate mapping (mouse "stops working" after a tab switch). The
    // term panel goes display:none when another tab is shown, and the
    // ResizeObserver fires on that 0×0 transition.
    const el = elBody();
    if (!el || el.offsetWidth === 0 || el.offsetHeight === 0) return;
    try { fit.fit(); } catch (_) {}
  }

  // sendResize pushes the current grid to the host unconditionally. onResize
  // only fires when cols/rows CHANGE, so after a page reload (fresh xterm that
  // happens to fit the same size the host already had) the host would never get
  // told — push it explicitly on connect.
  function sendResize() {
    if (term && ws && ws.readyState === 1) {
      ws.send(JSON.stringify({ t: 'resize', cols: term.cols, rows: term.rows }));
    }
  }

  // redraw forces xterm to repaint every row from its own buffer. While a
  // browser tab is hidden the renderer is throttled, so writes update the
  // buffer but the DOM lags; on return the screen can look garbled. A refresh
  // fixes it client-side — no reattach, so no mouse-mode loss.
  function redraw() {
    if (!term) return;
    const t = document.getElementById('tab-term');
    if (t && t.classList.contains('hidden')) return; // not visible; refresh on show
    try { term.refresh(0, term.rows - 1); } catch (_) {}
  }

  // DEBUG: scan host→client bytes for sequences that would turn mouse tracking
  // OFF — mouse DECRST (ESC[?1000l / 1002l / 1003l), full reset (ESC c) or soft
  // reset (ESC[!p). Logs them so we can see exactly what kills the mouse.
  function dbgScanIn(data) {
    let s = data;
    if (typeof s !== 'string') { try { s = new TextDecoder().decode(data); } catch (_) { return; } }
    const hits = s.match(/\x1b\[\?(?:1000|1002|1003|1006)[hl]|\x1bc|\x1b\[!p/g);
    if (hits) console.log('[term] IN mode-seq', hits.map((h) => JSON.stringify(h)).join(' '));
  }

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
    ws.onopen = () => { setStatus('connected'); doFit(); sendResize(); term.focus(); };
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') { dbgScanIn(e.data); term.write(e.data); }
      else { const u = new Uint8Array(e.data); dbgScanIn(u); term.write(u); }
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
    setTimeout(() => { doFit(); redraw(); }, 0); // repaint when the panel re-shows
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

  // leaveTab drops the client view and returns to the default tab. The host
  // PTY keeps running (reattach later) — this is detach, not terminate.
  function leaveTab() {
    disconnect();
    if ((location.hash || '').indexOf('#term:') === 0) location.hash = '';
    const t = document.getElementById('tab-term');
    if (t) t.classList.add('hidden');
    const first = document.querySelector('.ax-tab');
    if (first) first.click(); // back to the default tab
  }

  // killSession actually terminates the PTY on the host, then leaves the tab.
  // The persisted session id is dropped so the next open starts fresh rather
  // than reattaching to a dead session.
  function killSession() {
    if (ws && ws.readyState === 1) {
      try { ws.send(JSON.stringify({ t: 'kill' })); } catch (_) {}
    }
    if (curPub) { try { localStorage.removeItem('f2f.term.' + curPub); } catch (_) {} }
    setTimeout(leaveTab, 150); // give the kill time to reach the host before we drop the WS
  }

  function applyTermRoute() {
    const m = (location.hash || '').replace(/^#/, '').match(/^term:(.+)$/);
    if (m) open(decodeURIComponent(m[1]));
    // Switching to another tab no longer drops the WS: the connection (and the
    // live xterm state — mouse mode, alt-screen, …) is kept alive in the
    // background, the panel just gets hidden. Reattaching would replay the ring
    // WITHOUT the app's one-time mouse-enable, so the mouse would stop working
    // on return. Teardown happens only via the explicit "leave"/"kill" button
    // or when opening another peer.
  }

  // toggleFullscreen uses the native Fullscreen API on the terminal body
  // (like the video tiles in calls); xterm is refit after the mode change.
  function toggleFullscreen() {
    const el = elBody();
    if (document.fullscreenElement) {
      if (document.exitFullscreen) document.exitFullscreen();
    } else if (el && el.requestFullscreen) {
      el.requestFullscreen().catch(() => {});
    }
  }

  function init() {
    const kill = document.getElementById('term-kill');
    if (kill) kill.addEventListener('click', killSession);
    const fs = document.getElementById('term-fs');
    if (fs) fs.addEventListener('click', toggleFullscreen);
    document.addEventListener('fullscreenchange', () => setTimeout(doFit, 0));
    // When the browser tab becomes visible again, repaint: the renderer was
    // throttled while hidden so the screen may be stale.
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) setTimeout(redraw, 0);
    });
    window.addEventListener('hashchange', applyTermRoute);
    applyTermRoute();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
