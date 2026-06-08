// vnc.js — remote desktop (services/vnc over the bus), rendered with noVNC
// in the main column as the #tab-vnc panel. Opened from the sidebar
// "desktops" rows, which route to #vnc:<pub>.
//
// noVNC pulled from CDN as an ES module (debug-friendly; vendor later for
// offline like the other libs). RFB opens its own WebSocket to our local
// bridge, which pipes the raw RFB stream over the bus to the peer's VNC
// server — the RFB handshake (incl. password auth) is end-to-end.
import RFB from 'https://cdn.jsdelivr.net/npm/@novnc/novnc@1.5.0/lib/rfb.js';

(function () {
  let rfb = null, curPub = '';

  const elBody = () => document.getElementById('vnc-body');
  const elTitle = () => document.getElementById('vnc-title');
  const elStatus = () => document.getElementById('vnc-status');
  function setStatus(t) { const e = elStatus(); if (e) e.textContent = t || ''; }

  function showVncTab() {
    document.querySelectorAll('.tab-panel').forEach((p) => p.classList.add('hidden'));
    document.querySelectorAll('.ax-tab').forEach((t) => t.classList.remove('ax-tab-active'));
    const t = document.getElementById('tab-vnc');
    if (t) t.classList.remove('hidden');
  }

  function disconnect() {
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
    rfb.viewOnly = false;     // full control (mouse + keyboard)
    rfb.scaleViewport = true; // fit the remote framebuffer to the panel
    rfb.background = '#0b0e14';
    rfb.addEventListener('connect', () => setStatus('connected'));
    rfb.addEventListener('disconnect', (e) => {
      setStatus(e.detail && e.detail.clean ? 'disconnected' : 'disconnected (error)');
    });
    rfb.addEventListener('credentialsrequired', () => {
      const pw = window.prompt('VNC password:');
      try { rfb.sendCredentials({ password: pw || '' }); } catch (_) {}
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
    else if (curPub) disconnect(); // navigated away
  }

  function init() {
    const d = document.getElementById('vnc-disconnect');
    if (d) d.addEventListener('click', disconnectButton);
    const fs = document.getElementById('vnc-fs');
    if (fs) fs.addEventListener('click', toggleFullscreen);
    window.addEventListener('hashchange', applyVncRoute);
    applyVncRoute();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
