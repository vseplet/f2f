// call.js — unified call UI for f2f. Owns the active-call indicator bar
// (above the tabs) and the #tab-call window. Implements 1:1 (p2p) calls
// here; group (SFU) is a placeholder for now. audio.js / meet2.js are
// kept on disk as working references but are NOT loaded.
//
// Signalling rides the local HTTP server, addressed by peer pub:
//   POST /api/signal/outbox  body { to:<pub>, kind, sdp|candidate }
//   SSE  /api/signal/stream  delivers inbound { from:<pub>, kind, ... }
// Server forwards to the target peer's overlay IP and tags our pub as
// "from", so neither side depends on the engine's "active peer".
//
// Routes mirror chats:  #call:dm:<pub>  /  #call:group:<id>
$(function () {
  const PUB_RE = /^[0-9a-f]{64}$/;

  // ---- DOM ----
  const $bar = $('#ax-callbar');
  const videoPeer = document.getElementById('call-video-peer');
  const videoSelf = document.getElementById('call-video-self');
  const tilePeer = document.getElementById('call-tile-peer');
  const tileSelf = document.getElementById('call-tile-self');

  // ---- WebRTC session state ----
  let pc = null;
  let localStream = null;
  let isOfferer = false;
  let currentPeerPub = '';     // who we're signalling with
  let pendingCandidates = [];  // ICE that arrived before remoteDescription

  // ---- signalling ----
  let signalES = null;
  function startSignaling() {
    if (signalES) return;
    signalES = new EventSource('/api/signal/stream');
    signalES.onmessage = async (e) => {
      try { await handleSignal(JSON.parse(e.data)); } catch (_) {}
    };
  }
  function sendSignal(obj) {
    obj.to = currentPeerPub;
    return fetch('/api/signal/outbox', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(obj),
    }).catch(() => {});
  }

  async function statusPeers() {
    try { return ((await (await fetch('/api/status')).json()).peers) || []; }
    catch (_) { return []; }
  }
  async function pubForName(name) {
    const p = (await statusPeers()).find(p => !p.self && p.name === name);
    return p ? p.pub : null;
  }
  async function nameForPub(pub) {
    const p = (await statusPeers()).find(p => p.pub === pub);
    return p ? p.name : (pub || '').slice(0, 12);
  }

  // ---- media ----
  async function ensureLocalStream() {
    if (localStream) return localStream;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) return null;
    const gum = async (c) => { try { return await navigator.mediaDevices.getUserMedia(c); } catch (_) { return null; } };
    localStream =
      (await gum({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true }, video: { width: { ideal: 640 }, height: { ideal: 480 } } })) ||
      (await gum({ audio: true })) ||
      (await gum({ video: true }));
    if (localStream) {
      videoSelf.srcObject = localStream;
      if (localStream.getVideoTracks().length) tileSelf.classList.add('has-video');
      videoSelf.play().catch(() => {});
    }
    return localStream;
  }

  // ---- peer connection ----
  function newPC() {
    const conn = new RTCPeerConnection({ iceServers: [] }); // host candidates only
    conn.ontrack = (e) => {
      videoPeer.srcObject = e.streams[0];
      if (e.track.kind === 'video') tilePeer.classList.add('has-video');
      videoPeer.play().catch(() => {});
    };
    conn.onicecandidate = (e) => {
      if (e.candidate) sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
    };
    conn.onnegotiationneeded = async () => {
      if (!isOfferer || conn.signalingState !== 'stable') return;
      try {
        const offer = await conn.createOffer();
        await conn.setLocalDescription(offer);
        sendSignal({ kind: 'offer', sdp: offer.sdp });
      } catch (_) {}
    };
    conn.oniceconnectionstatechange = () => {
      const st = conn.iceConnectionState;
      if (st === 'failed' || st === 'disconnected') Call.setState('weak');
      else if (st === 'connected' || st === 'completed') Call.setState('active');
    };
    return conn;
  }

  async function flushCandidates() {
    for (const c of pendingCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
    pendingCandidates = [];
  }

  // Caller side: build the offer to currentPeerPub.
  async function offerTo() {
    await ensureLocalStream();
    pc = newPC();
    isOfferer = true;
    if (localStream) localStream.getTracks().forEach(t => pc.addTrack(t, localStream));
    const offer = await pc.createOffer({ offerToReceiveAudio: true, offerToReceiveVideo: true });
    await pc.setLocalDescription(offer);
    sendSignal({ kind: 'offer', sdp: offer.sdp });
  }

  async function handleSignal(msg) {
    if (msg.from) currentPeerPub = msg.from; // reply path
    if (msg.kind === 'offer') {
      if (pc && pc.signalingState !== 'closed') {
        // renegotiation within an active call
        try {
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
          const ans = await pc.createAnswer();
          await pc.setLocalDescription(ans);
          sendSignal({ kind: 'answer', sdp: ans.sdp });
        } catch (_) {}
        return;
      }
      // incoming call → adopt + answer
      const title = await nameForPub(msg.from);
      Call.adopt('dm', msg.from, title);
      location.hash = 'call:dm:' + msg.from;
      await ensureLocalStream();
      pc = newPC();
      isOfferer = false;
      if (localStream) localStream.getTracks().forEach(t => pc.addTrack(t, localStream));
      await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
      await flushCandidates();
      const ans = await pc.createAnswer();
      await pc.setLocalDescription(ans);
      sendSignal({ kind: 'answer', sdp: ans.sdp });
    } else if (msg.kind === 'answer') {
      if (!pc) return;
      await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
      await flushCandidates();
    } else if (msg.kind === 'candidate') {
      if (!pc || !pc.remoteDescription) { pendingCandidates.push(msg.candidate); return; }
      try { await pc.addIceCandidate(msg.candidate); } catch (_) {}
    } else if (msg.kind === 'hangup') {
      teardown();
    }
  }

  function teardown() {
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    if (localStream) { localStream.getTracks().forEach(t => t.stop()); localStream = null; }
    isOfferer = false;
    pendingCandidates = [];
    currentPeerPub = '';
    videoPeer.srcObject = null;
    videoSelf.srcObject = null;
    tilePeer.classList.remove('has-video');
    tileSelf.classList.remove('has-video');
  }

  // ---- CallManager (state + indicator) ----
  const Call = {
    active: null, // { kind, id, title, state, startedAt }
    timer: null,

    // adopt sets the active-call state + indicator without touching media.
    adopt(kind, id, title) {
      this.active = { kind, id, title: title || id, state: 'connecting', startedAt: Date.now() };
      this.renderBar();
    },
    setState(state) {
      if (this.active) { this.active.state = state; }
    },

    // start a NEW outgoing call.
    async start(kind, idOrName, title) {
      if (this.active) this.hangup(true); // one call at a time
      if (kind === 'dm') {
        let pub = idOrName;
        if (!PUB_RE.test(idOrName)) {
          pub = (await pubForName(idOrName)) || '';
          title = title || idOrName;
        }
        if (!pub) { return; } // unknown / not a real peer
        this.adopt('dm', pub, title || (await nameForPub(pub)));
        currentPeerPub = pub;
        location.hash = 'call:dm:' + pub;
        try { await offerTo(); } catch (_) { this.hangup(); }
      } else {
        // group (SFU) — placeholder until wired.
        this.adopt('group', idOrName, title || idOrName);
        location.hash = 'call:group:' + idOrName;
      }
    },

    hangup(silent) {
      if (this.active && this.active.kind === 'dm') {
        try { sendSignal({ kind: 'hangup' }); } catch (_) {}
      }
      teardown();
      this.active = null;
      this.renderBar();
      if (!silent && (location.hash || '').indexOf('#call:') === 0) location.hash = '';
    },

    renderBar() {
      const a = this.active;
      if (!a) {
        $bar.hide();
        if (this.timer) { clearInterval(this.timer); this.timer = null; }
        return;
      }
      $bar.css('display', 'flex');
      $('#ax-callbar-icon').attr('class', 'bi ' + (a.kind === 'group' ? 'bi-people-fill' : 'bi-telephone-fill'));
      $('#ax-callbar-title').text((a.kind === 'group' ? '# ' : '') + a.title);
      if (!this.timer) this.timer = setInterval(() => this.tick(), 1000);
      this.tick();
    },
    tick() {
      if (!this.active) return;
      const s = Math.max(0, Math.floor((Date.now() - this.active.startedAt) / 1000));
      const mm = String(Math.floor(s / 60)).padStart(2, '0');
      const ss = String(s % 60).padStart(2, '0');
      $('#ax-callbar-time').text(mm + ':' + ss);
    },
  };
  window.f2fCall = Call;

  // ---- controls ----
  function toggleTrack(kind) {
    if (!localStream) return false;
    const tracks = kind === 'audio' ? localStream.getAudioTracks() : localStream.getVideoTracks();
    let on = false;
    tracks.forEach(t => { t.enabled = !t.enabled; on = t.enabled; });
    if (kind === 'video') tileSelf.classList.toggle('has-video', on);
    return on;
  }
  $('#ax-callbar-open').on('click', function () {
    if (Call.active) location.hash = 'call:' + Call.active.kind + ':' + Call.active.id;
  });
  $('#ax-callbar-mute').on('click', function (e) {
    e.stopPropagation();
    const on = toggleTrack('audio');
    $(this).find('i').attr('class', 'bi ' + (on ? 'bi-mic-fill' : 'bi-mic-mute-fill'));
  });
  $('#ax-callbar-hangup').on('click', function (e) { e.stopPropagation(); Call.hangup(); });
  $('#call-hangup').on('click', function () { Call.hangup(); });
  $('.ax-call-ctrl[title="mute"]').on('click', function () {
    const on = toggleTrack('audio');
    $(this).find('i').attr('class', 'bi ' + (on ? 'bi-mic-fill' : 'bi-mic-mute-fill'));
  });
  $('.ax-call-ctrl[title="camera"]').on('click', function () {
    const on = toggleTrack('video');
    $(this).find('i').attr('class', 'bi ' + (on ? 'bi-camera-video-fill' : 'bi-camera-video-off-fill'));
  });

  // The chat header call button starts a call for the chat we're in.
  $('#chat-call').on('click', function () {
    const h = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    const m = h.match(/^chat:(dm|channel):(.+)$/);
    if (!m) return;
    if (m[1] === 'channel') Call.start('group', m[2], m[2]);
    else Call.start('dm', m[2]);
  });

  // ---- router: #call:<kind>:<id> shows the call window ----
  function applyCallRoute() {
    const h = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    const m = h.match(/^call:(dm|group):(.+)$/);
    if (!m) return;
    const [, kind, id] = m;
    const title = (Call.active && Call.active.id === id) ? Call.active.title : id;
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-call').removeClass('hidden');
    $('#call-title').text((kind === 'group' ? '# ' : '') + title);
    $('#call-peer-name').text((kind === 'group' ? '# ' : '') + title);
  }
  window.addEventListener('hashchange', applyCallRoute);
  applyCallRoute();

  startSignaling();
});
