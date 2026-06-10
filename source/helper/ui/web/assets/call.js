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
  console.log('%c[grp] call.js INSTRUMENTED build loaded', 'color:#7fc474;font-weight:bold');
  const PUB_RE = /^[0-9a-f]{64}$/;

  // encHash escapes a value for the URL hash but keeps '/' readable —
  // channel ids are "<ownerPub>/<name>" and a %2F-littered address bar is
  // noise; the route regexes capture slashes fine.
  function encHash(v) { return encodeURIComponent(v).replace(/%2F/gi, '/'); }

  // chatEvent drops a call lifecycle line (started/ended) into the chat the
  // call belongs to — events are ordinary messages, so they reach every
  // member's history through the messenger's delivery/outbox machinery.
  function chatEvent(kind, key, type) {
    if (!key) return;
    fetch('/api/chat/send', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ kind, key, type }),
    }).catch(() => {});
  }

  // ---- DOM ----
  const $bar = $('#ax-callbar');
  const videoPeer = document.getElementById('call-video-peer');
  const videoSelf = document.getElementById('call-video-self');
  const tilePeer = document.getElementById('call-tile-peer');
  const tileSelf = document.getElementById('call-tile-self');

  // ---- WebRTC session state ----
  let pc = null;
  let localStream = null;
  let isOfferer = false;       // also = the "impolite" peer for glare handling
  let makingOffer = false;     // perfect-negotiation: a local offer is in flight
  let currentPeerPub = '';     // who we're signalling with

  // screen share + volume
  let screenStream = null;
  let screenSenders = [];
  let peerCamStreamId = '';    // id of the peer's camera/mic stream (vs screen)
  let peerScreenStreamId = ''; // id of the peer's screen stream (from its signal)
  let volume = 80;

  // local media toggles
  let micEnabled = false;
  let camEnabled = false;

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
    const acquire = async () =>
      (await gum({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true }, video: { width: { ideal: 640 }, height: { ideal: 480 } } })) ||
      (await gum({ audio: true })) ||
      (await gum({ video: true }));
    // After a reload the previous page may still hold the camera/mic for a
    // moment (NotReadableError). Retry a few times before giving up — a
    // media-less offer would make our streams vanish on the peer's side.
    for (let i = 0; i < 5 && !localStream; i++) {
      localStream = await acquire();
      if (!localStream) await new Promise(r => setTimeout(r, 300));
    }
    if (localStream) {
      videoSelf.srcObject = localStream;
      micEnabled = localStream.getAudioTracks().length > 0;
      camEnabled = localStream.getVideoTracks().length > 0;
      if (camEnabled) tileSelf.classList.add('has-video');
      videoSelf.play().catch(() => {});
      updateMediaButtons();
    } else {
      console.warn('[f2f call] no local media (mic/cam busy or denied) — peer will not see/hear us');
    }
    return localStream;
  }

  // Wait until ICE gathering finishes so the local SDP carries every host
  // candidate; we then send that full SDP instead of trickling candidates.
  // Capped so a stuck gatherer can't hang the handshake forever. 5s: on a
  // mac with many interfaces (en0, awdl0, foreign utuns) 2s sometimes cut
  // the gather short and the overlay (100.64.x) candidate missed the SDP —
  // then media had no path over the tunnel at all.
  function waitIceComplete(conn, ms = 5000) {
    if (conn.iceGatheringState === 'complete') return Promise.resolve();
    return new Promise((resolve) => {
      let done = false;
      const finish = () => {
        if (done) return; done = true;
        conn.removeEventListener('icegatheringstatechange', check);
        resolve();
      };
      const check = () => { if (conn.iceGatheringState === 'complete') finish(); };
      conn.addEventListener('icegatheringstatechange', check);
      setTimeout(finish, ms);
    });
  }

  // ---- peer connection ----
  function newPC() {
    const conn = new RTCPeerConnection({ iceServers: [] }); // host candidates only
    conn.ontrack = (e) => {
      const stream = e.streams[0];
      if (!stream) return;
      // Screen is identified by the peer's screen-on signal (sid). Until that
      // arrives, fall back to "first stream = camera" — and the signal handler
      // re-routes if a camera-less peer's screen was mistaken for the camera.
      const isScreen = peerScreenStreamId
        ? stream.id === peerScreenStreamId
        : (peerCamStreamId && stream.id !== peerCamStreamId);
      if (isScreen) {
        showRemoteScreen(stream);          // peer started screen share
        stream.getVideoTracks().forEach(t => t.addEventListener('ended', clearRemoteScreen));
        return;
      }
      if (!peerCamStreamId) peerCamStreamId = stream.id;
      if (stream.id === peerCamStreamId) {
        videoPeer.srcObject = stream;
        if (e.track.kind === 'video') { tilePeer.classList.add('has-video'); reflowStage(); }
        videoPeer.volume = volume / 100;
        videoPeer.play().catch(() => {});
      }
    };
    // Non-trickle: candidates are embedded in the SDP we send after gathering
    // completes (see waitIceComplete), so we don't trickle them separately —
    // that avoids lost/out-of-order candidate messages over the SSE relay.
    // We still log them so we can see whether an overlay (100.64.x) host
    // candidate is actually gathered — if not, media can't flow over the tunnel.
    conn.onicecandidate = (e) => {
      if (e.candidate) console.log('[f2f call] cand', e.candidate.candidate);
      else console.log('[f2f call] gathering complete');
    };
    // Either side may renegotiate (e.g. answerer starts a screen share). The
    // stable guard prevents firing during the initial offer; collisions are
    // resolved on the receiving side via perfect-negotiation politeness.
    conn.onnegotiationneeded = async () => {
      if (conn.signalingState !== 'stable') return;
      try {
        makingOffer = true;
        const offer = await conn.createOffer();
        await conn.setLocalDescription(offer);
        await waitIceComplete(conn);
        sendSignal({ kind: 'offer', sdp: conn.localDescription.sdp });
      } catch (_) {} finally { makingOffer = false; }
    };
    conn.oniceconnectionstatechange = () => {
      const st = conn.iceConnectionState;
      console.log('[f2f call] ice', st);
      if (st === 'failed') {
        Call.setState('weak');
        if (isOfferer) restartIce(); else requestIceRestart();
      }
      else if (st === 'disconnected') Call.setState('weak');
      else if (st === 'connected' || st === 'completed') Call.setState('active');
    };
    return conn;
  }

  // The answerer can't restart ICE itself (a restart is an offer with fresh
  // credentials, and offers stay on the caller's side to avoid glare), so it
  // asks the offerer over the signalling channel — which rides the helper
  // bus and stays up even when the media path is dead. Throttled so a
  // flapping connection doesn't spam restart offers.
  let lastRestartReq = 0;
  function requestIceRestart() {
    const now = Date.now();
    if (now - lastRestartReq < 5000) return;
    lastRestartReq = now;
    sendSignal({ kind: 'restart' });
  }

  // ICE restart: on a failed connection the offerer re-offers with fresh ICE
  // (renegotiation on the same pc, so the peer just answers — no reset).
  let iceRestarting = false;
  async function restartIce() {
    if (!pc || !isOfferer || iceRestarting) return;
    iceRestarting = true;
    try {
      const offer = await pc.createOffer({ iceRestart: true });
      await pc.setLocalDescription(offer);
      await waitIceComplete(pc);
      sendSignal({ kind: 'offer', sdp: pc.localDescription.sdp });
    } catch (_) {} finally { iceRestarting = false; }
  }

  // Caller side: build the offer to currentPeerPub.
  async function offerTo() {
    await ensureLocalStream();
    pc = newPC();
    isOfferer = true;
    if (localStream) localStream.getTracks().forEach(t => pc.addTrack(t, localStream));
    const offer = await pc.createOffer({ offerToReceiveAudio: true, offerToReceiveVideo: true });
    await pc.setLocalDescription(offer);
    await waitIceComplete(pc);
    sendSignal({ kind: 'offer', sdp: pc.localDescription.sdp, fresh: true }); // fresh = new session
  }

  async function handleSignal(msg) {
    if (msg.from) currentPeerPub = msg.from; // reply path
    if (msg.kind === 'offer') {
      const renegotiation = pc && pc.signalingState !== 'closed' && !msg.fresh;
      if (renegotiation) {
        // track change within the live session (e.g. screen share). If both
        // sides offered at once (glare), the impolite peer (the original
        // offerer) ignores the incoming offer; the polite one rolls back.
        const collision = makingOffer || pc.signalingState !== 'stable';
        if (collision && isOfferer) return;
        try {
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
          const ans = await pc.createAnswer();
          await pc.setLocalDescription(ans);
          await waitIceComplete(pc);
          sendSignal({ kind: 'answer', sdp: pc.localDescription.sdp });
        } catch (_) {}
        return;
      }
      // fresh offer: a new call, or the other side reloaded and is reconnecting.
      // Drop any stale connection and answer cleanly.
      if (pc) {
        try { pc.close(); } catch (_) {}
        pc = null; peerCamStreamId = ''; peerScreenStreamId = '';
        videoPeer.srcObject = null; tilePeer.classList.remove('has-video');
        clearRemoteScreen();
      }
      const name = await nameForPub(msg.from);
      Call.adopt('dm', name, name, msg.from);
      location.hash = 'call:dm:' + msg.from; // routes key dm calls by pub, like chats
      await ensureLocalStream();
      pc = newPC();
      isOfferer = false;
      if (localStream) localStream.getTracks().forEach(t => pc.addTrack(t, localStream));
      await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
      const ans = await pc.createAnswer();
      await pc.setLocalDescription(ans);
      await waitIceComplete(pc);
      sendSignal({ kind: 'answer', sdp: pc.localDescription.sdp });
    } else if (msg.kind === 'answer') {
      if (!pc) return;
      await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
    } else if (msg.kind === 'candidate') {
      // We're non-trickle (candidates ride in the SDP); only honour a stray
      // trickled candidate if the connection is already up enough to take it.
      if (pc && pc.remoteDescription) { try { await pc.addIceCandidate(msg.candidate); } catch (_) {} }
    } else if (msg.kind === 'restart') {
      // The answerer noticed the media path died and asks us to re-offer
      // with fresh ICE. No-op on the answerer side (restartIce guards).
      restartIce();
    } else if (msg.kind === 'screen') {
      if (msg.on) {
        peerScreenStreamId = msg.sid || '';
        // If the screen arrived before this signal and was mistaken for the
        // camera (peer has no camera), move it from the camera tile to screen.
        if (peerScreenStreamId && peerCamStreamId === peerScreenStreamId) {
          const s = videoPeer.srcObject;
          peerCamStreamId = '';
          videoPeer.srcObject = null;
          tilePeer.classList.remove('has-video');
          reflowStage();
          if (s) showRemoteScreen(s);
        }
      } else {
        clearRemoteScreen();          // peer stopped sharing → drop the tile
        peerScreenStreamId = '';
      }
    } else if (msg.kind === 'hangup') {
      Call.endLocal(); // peer left → close window + indicator on our side too
    }
  }

  function teardown() {
    stopScreenShare(true);
    clearRemoteScreen();
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    if (localStream) { localStream.getTracks().forEach(t => t.stop()); localStream = null; }
    isOfferer = false;
    currentPeerPub = '';
    peerCamStreamId = '';
    peerScreenStreamId = '';
    micEnabled = false;
    camEnabled = false;
    videoPeer.srcObject = null;
    videoSelf.srcObject = null;
    tilePeer.classList.remove('has-video');
    tileSelf.classList.remove('has-video');
    if (typeof updateMediaButtons === 'function') updateMediaButtons();
  }

  // ---- session persistence (survive a tab reload mid-call) ----
  const CALL_KEY = 'f2f.call';
  function saveCall(a) {
    try { sessionStorage.setItem(CALL_KEY, JSON.stringify({ kind: a.kind, id: a.id, title: a.title, pub: a.pub || '' })); } catch (_) {}
  }
  function clearCall() { try { sessionStorage.removeItem(CALL_KEY); } catch (_) {} }

  // ---- CallManager (state + indicator) ----
  const Call = {
    active: null, // { kind, id, title, state, startedAt }
    timer: null,

    // adopt sets the active-call state + indicator without touching media.
    // id is the route label (nickname for dm); pub is kept for signalling/reconnect.
    adopt(kind, id, title, pub) {
      this.active = { kind, id, title: title || id, pub: pub || '', state: 'connecting', startedAt: Date.now() };
      saveCall(this.active);
      this.renderBar();
    },
    setState(state) {
      if (this.active) { this.active.state = state; }
    },

    // start a NEW outgoing call.
    async start(kind, idOrName, title) {
      if (this.active) await this.hangup(true); // one call at a time — fully tear the old one down BEFORE joining the new (shared Group singleton)
      if (kind === 'dm') {
        let pub = idOrName;
        if (!PUB_RE.test(idOrName)) {
          pub = (await pubForName(idOrName)) || '';
          title = title || idOrName;
        }
        if (!pub) { return; } // unknown / not a real peer
        const name = title || (await nameForPub(pub)); // nameForPub falls back to a pub slice
        this.adopt('dm', name, name, pub);
        currentPeerPub = pub;
        location.hash = 'call:dm:' + pub; // routes key dm calls by pub, like chats
        chatEvent('dm', pub, 'call_start'); // caller drops the system line (callee doesn't — no dupes)
        try { await offerTo(); } catch (_) { this.hangup(); }
      } else {
        // group (SFU) — create/join the camp's call and render in this window.
        this.adopt('group', idOrName, title || idOrName, '');
        currentPeerPub = '';
        location.hash = 'call:group:' + encHash(idOrName);
        Group.start(idOrName, title || idOrName).catch(() => this.hangup());
      }
    },

    // hang up. For dm we notify the peer over the p2p channel; for group the
    // SFU leave happens in Group.leave (via endLocal). The side that ACTS
    // (presses hangup / hosts the dying SFU) drops the "call ended" line —
    // the passive side doesn't, so it lands exactly once.
    async hangup(silent) {
      if (this.active && this.active.kind === 'dm') {
        let pub = this.active.pub || currentPeerPub;
        if (pub) { currentPeerPub = pub; try { sendSignal({ kind: 'hangup' }); } catch (_) {} }
        if (pub) chatEvent('dm', pub, 'call_end');
      } else if (this.active && this.active.kind === 'group'
                 && Group.channel && Group.sfuHost === Group.myIP) {
        chatEvent('channel', Group.channel, 'call_end');
      }
      await this.endLocal(silent);
    },

    // tear everything down locally without signalling (used on a remote hangup).
    // silent = we're switching to another call, so skip any navigation.
    // A finished call drops the user back into the chat it belongs to (the
    // "call ended" line is already there) instead of a dead-end ended screen.
    async endLocal(silent) {
      const a = this.active;
      const chatRoute = a
        ? (a.kind === 'group' ? 'chat:channel:' + encHash(a.id) : 'chat:dm:' + (a.pub || a.id))
        : '';
      if (a && a.kind === 'group') await Group.leave(true);
      else teardown();
      clearCall();
      this.active = null;
      this.renderBar();
      if (!silent && (location.hash || '').indexOf('#call:') === 0) {
        location.hash = chatRoute;
      }
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

  // ---- Group calls (SFU) — ported from meet2, rendered in this window.
  // Host-based (no room binding yet): the channel call joins the camp's
  // existing SFU call or creates one. Uses the separate /api/call/* signalling,
  // so it never collides with the p2p path above.
  const Group = {
    pc: null, stream: null, signalES: null,
    screenStream: null, screenSenders: [],
    micEnabled: false, camEnabled: false, inCall: false,
    sfuHost: '', myIP: '', myName: '',
    remoteTiles: {},   // streamId → {tile, peerIP}
    peerTiles: {},     // tunnelIP → tile (placeholder or video)
    peersWithVideo: {},
    pending: [], hasRemoteDesc: false, negotiating: false,
    signalChain: Promise.resolve(),
    reconnectAttempts: 0, reconnectTimer: null, pollTimer: null,

    async jget(url, opts) {
      const r = await fetch(url, opts);
      if (r.status === 204) return null;
      if (!r.ok) throw new Error(r.status);
      return r.json();
    },

    channel: '', // messenger channel id this group call is bound to ('' = unbound)

    async start(target) {
      try {
        const s = await (await fetch('/api/status')).json();
        this.myIP = (s && s.local_ip) || '';
        this.myName = (s && s.camp_name) || 'you';
      } catch (_) {}
      // `target` is a messenger channel id ("<ownerPub>/<name>", started from
      // a chat), or a call_id / sfu_host IP (clicked an existing meet row).
      // A channel target binds: we join THAT channel's call or create one
      // bound to it — never capture another channel's call. A specific
      // target (digit-leading call_id or IP) is worth a few discovery
      // retries because a peer's call may not have been polled yet.
      const isChannel = (target || '').indexOf('/') > 0;
      this.channel = isChannel ? target : '';
      const specific = /^\d/.test(target || '');
      const call = await this.findCall(target, specific ? 4 : 1, isChannel);
      console.log('[grp] start target=%s myIP=%s → %s', target, this.myIP, call ? 'JOIN host ' + call.sfu_host : 'CREATE new (no existing call found)');
      if (call) { this.sfuHost = call.sfu_host; await this.join(); }
      else { await this.create(); }
    },
    // findCall locates the call to join: a channel target matches ONLY its
    // channel's call; otherwise exact call_id/sfu_host match, then any
    // active call. Retries ride out the ~3s remote-call discovery lag.
    async findCall(target, tries, channelOnly) {
      for (let i = 0; i < tries; i++) {
        let calls = [];
        try { calls = (await this.jget('/api/call/list')) || []; } catch (_) {}
        const c = channelOnly
          ? calls.find(x => x.channel === target)
          : (calls.find(x => x.call_id === target || x.sfu_host === target)
             || calls.find(x => x.sfu_host));
        if (c) return c;
        if (i < tries - 1) await new Promise(r => setTimeout(r, 1200));
      }
      return null;
    },

    async create() {
      if (this.inCall) await this.leave(true);
      await this.jget('/api/call/create', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ channel: this.channel }),
      });
      this.sfuHost = this.myIP;
      // Only the creator announces — joiners don't, so the channel gets one
      // "call started" line per call.
      chatEvent('channel', this.channel, 'call_start');
      await this.joinSFU();
    },
    async join() {
      if (!this.sfuHost) return;
      const target = this.sfuHost;
      if (this.inCall) await this.leave(true);
      this.sfuHost = target;
      try {
        await this.jget('/api/call/join', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ tunnel_ip: this.myIP, name: this.myName, sfu_host: this.sfuHost }),
        });
        console.log('[grp] join POST ok → sfu', this.sfuHost);
      } catch (e) { console.warn('[grp] join POST FAILED to', this.sfuHost, ':', e.message); throw e; }
      await this.joinSFU();
    },
    async joinSFU() {
      await this.acquireMedia();
      this.pc = this.createPC();
      if (this.stream) this.stream.getTracks().forEach(t => this.pc.addTrack(t, this.stream));
      else { this.pc.addTransceiver('audio', { direction: 'recvonly' }); this.pc.addTransceiver('video', { direction: 'recvonly' }); }
      this.startSignalStream();
      this.inCall = true;
      Call.setState('active');
      this.refreshButtons();
      this.startPolling();
    },

    async acquireMedia() {
      const gum = async (c) => { try { return await navigator.mediaDevices.getUserMedia(c); } catch (_) { return null; } };
      this.stream = (await gum({ audio: true, video: { width: { ideal: 640 }, height: { ideal: 480 } } })) || (await gum({ audio: true })) || (await gum({ video: true }));
      if (this.stream) {
        this.micEnabled = this.stream.getAudioTracks().length > 0;
        this.camEnabled = this.stream.getVideoTracks().length > 0;
        videoSelf.srcObject = this.stream;
        tileSelf.classList.toggle('has-video', this.camEnabled);
        videoSelf.play().catch(() => {});
      }
    },

    createPC() {
      const conn = new RTCPeerConnection({ iceServers: [] });
      conn.onicecandidate = (e) => {
        if (!e.candidate) return;
        // remote SFU: only ship candidates that name the host's overlay IP.
        if (this.sfuHost !== this.myIP && this.myIP && e.candidate.candidate.indexOf(this.myIP) === -1) return;
        this.sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
      };
      conn.ontrack = (e) => { console.log('[grp] ontrack', e.track.kind, e.streams[0] && e.streams[0].id); if (e.streams && e.streams[0]) this.addRemoteStream(e.streams[0]); };
      conn.onconnectionstatechange = () => {
        const st = conn.connectionState;
        console.log('[grp] pc', st);
        if (st === 'connected') this.reconnectAttempts = 0;
        else if (st === 'failed') this.scheduleReconnect();
        else if (st === 'closed') Call.hangup();
      };
      conn.onnegotiationneeded = async () => {
        if (this.negotiating) return;
        this.negotiating = true;
        try {
          const offer = await conn.createOffer();
          await conn.setLocalDescription(offer);
          const resp = await this.sendSignal({ kind: 'offer', sdp: offer.sdp });
          if (resp && resp.kind === 'answer' && conn.signalingState === 'have-local-offer') {
            await conn.setRemoteDescription({ type: 'answer', sdp: resp.sdp });
            this.hasRemoteDesc = true; await this.flush();
          }
        } catch (_) {} finally { this.negotiating = false; }
      };
      return conn;
    },

    async sendSignal(msg) {
      try {
        const r = await fetch('/api/call/signal', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(msg) });
        if (!r.ok) { console.warn('[grp] signal', msg.kind, '→ HTTP', r.status, await r.text().catch(() => '')); return null; }
        if (r.status === 204) return null;
        return r.json();
      } catch (e) { console.warn('[grp] signal send failed', e.message); return null; }
    },
    startSignalStream() {
      if (this.signalES) return;
      this.signalES = new EventSource('/api/call/signal/stream');
      this.signalES.onmessage = (e) => {
        this.signalChain = this.signalChain.then(() => this.handleSignal(JSON.parse(e.data))).catch(() => {});
      };
    },
    stopSignalStream() { if (this.signalES) { this.signalES.close(); this.signalES = null; } },
    async flush() { while (this.pending.length) { try { await this.pc.addIceCandidate(this.pending.shift()); } catch (_) {} } },

    async handleSignal(msg) {
      const pc = this.pc;
      console.log('[grp] <sfu', msg.kind, 'from=' + msg.from, 'pc=' + (pc && pc.signalingState));
      if (!pc || msg.from !== 'sfu') return;
      if (msg.kind === 'offer') {
        if (pc.signalingState === 'have-local-offer') { try { await pc.setLocalDescription({ type: 'rollback' }); } catch (_) {} }
        this.hasRemoteDesc = false;
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        this.hasRemoteDesc = true;
        const ans = await pc.createAnswer();
        await pc.setLocalDescription(ans);
        await this.sendSignal({ kind: 'answer', sdp: ans.sdp });
        await this.flush();
      } else if (msg.kind === 'answer') {
        await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
        this.hasRemoteDesc = true; await this.flush();
      } else if (msg.kind === 'renegotiate') {
        const needed = msg.tracks || [];
        this.negotiating = true;
        try {
          const need = {}; needed.forEach(t => need[t.kind] = (need[t.kind] || 0) + 1);
          const have = {};
          pc.getTransceivers().forEach(tr => {
            if (tr.direction === 'recvonly' || tr.direction === 'sendrecv') { const k = tr.receiver.track.kind; have[k] = (have[k] || 0) + 1; }
          });
          for (const kind in need) { for (let d = 0; d < need[kind] - (have[kind] || 0); d++) pc.addTransceiver(kind, { direction: 'recvonly' }); }
          const offer = await pc.createOffer();
          await pc.setLocalDescription(offer);
          const resp = await this.sendSignal({ kind: 'offer', sdp: offer.sdp });
          if (resp && resp.kind === 'answer' && pc.signalingState === 'have-local-offer') {
            await pc.setRemoteDescription({ type: 'answer', sdp: resp.sdp });
            this.hasRemoteDesc = true; await this.flush();
          }
        } catch (_) {} finally { this.negotiating = false; }
      } else if (msg.kind === 'candidate') {
        if (this.hasRemoteDesc) { try { await pc.addIceCandidate(msg.candidate); } catch (_) {} }
        else this.pending.push(msg.candidate);
      }
    },

    // --- tiles ---
    ipOf(streamId) { const m = (streamId || '').match(/^(\d+\.\d+\.\d+\.\d+)-/); return m ? m[1] : ''; },
    makeTile(label, placeholder) {
      const tile = document.createElement('div');
      tile.className = 'ax-call-tile' + (placeholder ? '' : ' has-video');
      const v = document.createElement('video');
      v.autoplay = true; v.playsInline = true;
      const av = document.createElement('div'); av.className = 'ax-call-avatar'; av.innerHTML = '<i class="bi bi-person-fill"></i>';
      const name = document.createElement('div'); name.className = 'ax-call-name'; name.textContent = label;
      const fs = document.createElement('button'); fs.type = 'button'; fs.className = 'ax-call-fs'; fs.title = 'fullscreen'; fs.innerHTML = '<i class="bi bi-arrows-fullscreen"></i>';
      tile.appendChild(v); tile.appendChild(av); tile.appendChild(name); tile.appendChild(fs);
      return tile;
    },
    addRemoteStream(stream) {
      if (this.remoteTiles[stream.id]) return;
      const peerIP = this.ipOf(stream.id);
      const tile = this.makeTile(peerIP || 'peer', false);
      const v = tile.querySelector('video');
      v.srcObject = stream; v.volume = volume / 100; v.play().catch(() => {});
      if (this.peerTiles[peerIP] && !this.peersWithVideo[peerIP]) { this.peerTiles[peerIP].replaceWith(tile); this.peerTiles[peerIP] = tile; }
      else if (!this.peerTiles[peerIP]) { stage.appendChild(tile); this.peerTiles[peerIP] = tile; }
      else { stage.appendChild(tile); }
      this.peersWithVideo[peerIP] = true;
      this.remoteTiles[stream.id] = { tile, peerIP };
      reflowStage();
      stream.onremovetrack = () => { if (stream.getTracks().length === 0) this.removeRemoteStream(stream.id); };
    },
    removeRemoteStream(streamId) {
      const e = this.remoteTiles[streamId];
      if (!e) return;
      e.tile.remove(); delete this.remoteTiles[streamId];
      if (e.peerIP && this.peerTiles[e.peerIP] === e.tile) delete this.peerTiles[e.peerIP];
      let other = false; for (const id in this.remoteTiles) if (this.remoteTiles[id].peerIP === e.peerIP) { other = true; break; }
      if (!other) delete this.peersWithVideo[e.peerIP];
      reflowStage();
    },
    syncPeerTiles(participants) {
      if (!this.inCall || !participants) return;
      const active = {};
      participants.forEach(p => {
        if (p.tunnel_ip === this.myIP) return;
        active[p.tunnel_ip] = p.name;
        if (this.peerTiles[p.tunnel_ip]) {
          const lbl = this.peerTiles[p.tunnel_ip].querySelector('.ax-call-name');
          if (lbl) lbl.textContent = p.name; return;
        }
        const tile = this.makeTile(p.name, true);
        stage.appendChild(tile); this.peerTiles[p.tunnel_ip] = tile;
      });
      for (const ip in this.peerTiles) {
        if (!active[ip]) { this.peerTiles[ip].remove(); delete this.peerTiles[ip]; delete this.peersWithVideo[ip]; }
      }
      reflowStage();
    },
    clearTiles() {
      for (const id in this.remoteTiles) { this.remoteTiles[id].tile.remove(); delete this.remoteTiles[id]; }
      for (const ip in this.peerTiles) { this.peerTiles[ip].remove(); delete this.peerTiles[ip]; }
      this.peersWithVideo = {};
    },

    startPolling() {
      if (this.pollTimer) return;
      const poll = async () => {
        let calls = [];
        try { calls = (await this.jget('/api/call/list')) || []; } catch (_) {}
        if (!this.inCall) return;
        const mine = calls.find(c => c.sfu_host === this.sfuHost);
        if (mine) this.syncPeerTiles(mine.participants || []);
        else Call.hangup(); // call ended on the host
      };
      poll();
      this.pollTimer = setInterval(poll, 3000);
    },
    stopPolling() { if (this.pollTimer) { clearInterval(this.pollTimer); this.pollTimer = null; } },
    scheduleReconnect() {
      if (!this.inCall || this.reconnectTimer) return;
      if (this.reconnectAttempts >= 5) { Call.hangup(); return; }
      this.reconnectAttempts++;
      const delay = Math.min(1000 * this.reconnectAttempts, 5000);
      const host = this.sfuHost;
      this.reconnectTimer = setTimeout(async () => {
        this.reconnectTimer = null;
        if (!this.inCall) return;
        await this.leave(true); this.sfuHost = host; await this.join();
      }, delay);
    },

    // --- controls (driven by the unified buttons via actMic/actCam/actShare) ---
    toggleMic() { if (!this.stream) return; this.micEnabled = !this.micEnabled; this.stream.getAudioTracks().forEach(t => t.enabled = this.micEnabled); this.refreshButtons(); },
    toggleCam() { if (!this.stream) return; this.camEnabled = !this.camEnabled; this.stream.getVideoTracks().forEach(t => t.enabled = this.camEnabled); tileSelf.classList.toggle('has-video', this.camEnabled); this.refreshButtons(); },
    async toggleShare() {
      if (this.screenStream) { this.stopShare(); return; }
      if (!this.pc || !navigator.mediaDevices.getDisplayMedia) return;
      let s; try { s = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }); } catch (_) { return; }
      this.screenStream = s; this.screenSenders = [];
      s.getTracks().forEach(t => { this.screenSenders.push(this.pc.addTrack(t, s)); t.addEventListener('ended', () => this.stopShare()); });
      showSelfScreen(s); // local preview tile (SFU forwards it to others separately)
      setShareState(true);
    },
    stopShare() {
      if (!this.screenStream) return;
      const s = this.screenStream; this.screenStream = null;
      s.getTracks().forEach(t => t.stop());
      if (this.pc) this.screenSenders.forEach(sn => { try { this.pc.removeTrack(sn); } catch (_) {} });
      this.screenSenders = [];
      const t = document.getElementById('call-tile-screen-self');
      if (t) { t.remove(); reflowStage(); }
      setShareState(false);
    },
    refreshButtons() { micEnabled = this.micEnabled; camEnabled = this.camEnabled; updateMediaButtons(); },

    async leave() {
      if (!this.inCall && !this.pc) return;
      this.inCall = false;
      if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null; }
      this.stopPolling();
      if (this.screenStream) this.stopShare();
      this.stopSignalStream();
      if (this.pc) { try { this.pc.onconnectionstatechange = null; this.pc.ontrack = null; this.pc.close(); } catch (_) {} this.pc = null; }
      if (this.stream) { this.stream.getTracks().forEach(t => t.stop()); this.stream = null; }
      this.clearTiles();
      videoSelf.srcObject = null; tileSelf.classList.remove('has-video');
      this.micEnabled = false; this.camEnabled = false;
      this.pending = []; this.hasRemoteDesc = false; this.negotiating = false; this.sfuHost = '';
      try { await fetch('/api/call/leave', { method: 'POST' }); } catch (_) {}
    },
  };
  window.f2fGroup = Group;

  // ---- controls (meet2-style) ----
  const $btnMic = document.getElementById('call-mic');
  const $btnCam = document.getElementById('call-cam');
  const $btnShare = document.getElementById('call-share');

  // The active media stream — group stream when in a group call, else p2p.
  function activeStream() {
    return (Call.active && Call.active.kind === 'group') ? Group.stream : localStream;
  }
  function inGroup() { return !!(Call.active && Call.active.kind === 'group'); }

  function updateMediaButtons() {
    const ls = activeStream();
    const hasMic = !!ls && ls.getAudioTracks().length > 0;
    const hasCam = !!ls && ls.getVideoTracks().length > 0;
    if ($btnMic) {
      $btnMic.disabled = !hasMic;
      $btnMic.classList.toggle('active', micEnabled);
      $btnMic.querySelector('.ax-btn-state').textContent = micEnabled ? '●' : '○';
    }
    if ($btnCam) {
      $btnCam.disabled = !hasCam;
      $btnCam.classList.toggle('active', camEnabled);
      $btnCam.querySelector('.ax-btn-state').textContent = camEnabled ? '■' : '□';
    }
    // keep the compact callbar icons in sync
    $('#ax-callbar-mute i').attr('class', 'bi ' + (micEnabled ? 'bi-mic-fill' : 'bi-mic-mute-fill'));
    $('#ax-callbar-cam i').attr('class', 'bi ' + (camEnabled ? 'bi-camera-video-fill' : 'bi-camera-video-off-fill'));
  }

  // --- shared control actions (used by the call window AND the top bar) ---
  // They dispatch to the group SFU when a group call is active, else p2p.
  function actMic() {
    if (inGroup()) { Group.toggleMic(); return; }
    if (!localStream) return;
    micEnabled = !micEnabled;
    localStream.getAudioTracks().forEach(t => { t.enabled = micEnabled; });
    updateMediaButtons();
  }
  function actCam() {
    if (inGroup()) { Group.toggleCam(); return; }
    if (!localStream) return;
    camEnabled = !camEnabled;
    localStream.getVideoTracks().forEach(t => { t.enabled = camEnabled; });
    tileSelf.classList.toggle('has-video', camEnabled);
    updateMediaButtons();
  }
  function actShare() {
    if (inGroup()) { Group.toggleShare(); return; }
    if (screenStream) stopScreenShare(); else startScreenShare();
  }
  // dm routes (chat and call alike) are keyed by pub; group by channel id.
  function actChat() {
    const a = Call.active;
    if (!a) return;
    location.hash = a.kind === 'group'
      ? 'chat:channel:' + encHash(a.id)
      : 'chat:dm:' + (a.pub || a.id);
  }
  function openCall() {
    const a = Call.active;
    if (!a) return;
    location.hash = a.kind === 'group'
      ? 'call:group:' + encHash(a.id)
      : 'call:dm:' + (a.pub || a.id);
  }
  function setShareState(active) {
    if ($btnShare) {
      $btnShare.classList.toggle('active', active);
      $btnShare.querySelector('.ax-btn-state').textContent = active ? '■' : '▢';
    }
    $('#ax-callbar-share').toggleClass('active', active);
  }

  // call-window controls
  $('#call-mic').on('click', actMic);
  $('#call-cam').on('click', actCam);
  $('#call-share').on('click', actShare);

  // top bar: clicking the background opens the call window; the buttons mirror
  // the window controls and must stop propagation so they don't also open it.
  $('#ax-callbar').on('click', openCall);
  const stopAnd = (fn) => function (e) { e.stopPropagation(); fn(); };
  $('#ax-callbar-mute').on('click', stopAnd(actMic));
  $('#ax-callbar-cam').on('click', stopAnd(actCam));
  $('#ax-callbar-share').on('click', stopAnd(actShare));
  $('#ax-callbar-chat').on('click', stopAnd(actChat));
  $('#ax-callbar-hangup').on('click', stopAnd(() => Call.hangup()));
  async function startScreenShare() {
    if (!pc || screenStream) return;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia) return;
    let stream;
    try { stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }); }
    catch (_) { return; }
    screenStream = stream;
    screenSenders = [];
    stream.getTracks().forEach(t => {
      screenSenders.push(pc.addTrack(t, stream)); // separate stream id → peer sees a new tile
      t.addEventListener('ended', () => stopScreenShare());
    });
    showSelfScreen(stream);
    setShareState(true);
    // Tell the peer which stream id is the screen, so it routes to the screen
    // tile even when we have no camera (otherwise it looks like our camera).
    try { sendSignal({ kind: 'screen', on: true, sid: stream.id }); } catch (_) {}
  }
  function stopScreenShare(silent) {
    if (!screenStream) return;
    const stream = screenStream;
    screenStream = null;
    stream.getTracks().forEach(t => t.stop());
    if (pc) screenSenders.forEach(s => { try { pc.removeTrack(s); } catch (_) {} });
    screenSenders = [];
    const t = document.getElementById('call-tile-screen-self');
    if (t) { t.remove(); reflowStage(); }
    if (!silent) {
      setShareState(false);
      // Tell the peer to drop our screen tile — a removed remote track doesn't
      // reliably fire 'ended', so the explicit signal avoids a frozen tile.
      try { sendSignal({ kind: 'screen', on: false }); } catch (_) {}
    }
  }

  // --- screen tiles (local preview + remote) ---
  const stage = document.getElementById('call-stage');

  // reflowStage picks cols × rows that maximise tile area at 16:9 (Meet/meet2
  // style) so tiles fill the window instead of sitting in fixed cells.
  let reflowRAF = 0;
  function reflowStage() {
    cancelAnimationFrame(reflowRAF);
    reflowRAF = requestAnimationFrame(function () {
      // Only lay out visible tiles (the p2p peer tile is hidden in group mode).
      const tiles = Array.from(stage.children).filter(el => el.style.display !== 'none');
      const n = tiles.length;
      if (!n) return;
      const W = stage.clientWidth, H = stage.clientHeight;
      if (W <= 0 || H <= 0) return;
      const ratio = 16 / 9, gap = 8;
      let bestTileW = 0;
      for (let cols = 1; cols <= n; cols++) {
        const rows = Math.ceil(n / cols);
        const cellW = (W - (cols - 1) * gap) / cols;
        const cellH = (H - (rows - 1) * gap) / rows;
        if (cellW <= 0 || cellH <= 0) continue;
        const tileW = Math.min(cellW, cellH * ratio);
        if (tileW > bestTileW) bestTileW = tileW;
      }
      bestTileW = Math.max(0, bestTileW - 1); // avoid sub-pixel early wrap
      const tileH = bestTileW / ratio;
      for (let i = 0; i < n; i++) {
        tiles[i].style.width = bestTileW + 'px';
        tiles[i].style.height = tileH + 'px';
      }
    });
  }
  if (window.ResizeObserver) new ResizeObserver(reflowStage).observe(stage);
  // Auto-relayout on any tile add/remove (mirrors meet2) — belt-and-braces so a
  // tile can never be added without being laid out.
  new MutationObserver(reflowStage).observe(stage, { childList: true });
  window.addEventListener('resize', reflowStage);

  function screenTile(id, label) {
    let tile = document.getElementById(id);
    if (tile) return tile;
    tile = document.createElement('div');
    tile.className = 'ax-call-tile ax-call-tile-screen has-video';
    tile.id = id;
    const v = document.createElement('video');
    v.autoplay = true; v.playsInline = true; v.muted = (id === 'call-tile-screen-self');
    const name = document.createElement('div');
    name.className = 'ax-call-name';
    name.textContent = label;
    const fs = document.createElement('button');
    fs.type = 'button';
    fs.className = 'ax-call-fs';
    fs.title = 'fullscreen';
    fs.innerHTML = '<i class="bi bi-arrows-fullscreen"></i>';
    tile.appendChild(v);
    tile.appendChild(name);
    tile.appendChild(fs);
    stage.appendChild(tile);
    reflowStage();
    return tile;
  }
  // Fullscreen a tile's video when its ⛶ button is clicked.
  $(stage).on('click', '.ax-call-fs', function (e) {
    e.stopPropagation();
    const v = $(this).closest('.ax-call-tile').find('video')[0];
    if (v && v.requestFullscreen) v.requestFullscreen().catch(() => {});
  });
  function showSelfScreen(stream) {
    const v = screenTile('call-tile-screen-self', 'you · screen').querySelector('video');
    v.srcObject = stream; v.play().catch(() => {});
  }
  function showRemoteScreen(stream) {
    const v = screenTile('call-tile-screen-peer', 'peer · screen').querySelector('video');
    v.srcObject = stream; v.volume = volume / 100; v.play().catch(() => {});
  }
  function clearRemoteScreen() {
    const t = document.getElementById('call-tile-screen-peer');
    if (t) { t.remove(); reflowStage(); }
  }

  // --- volume (drag slider, applies to remote audio) ---
  const $volTrack = document.getElementById('call-vol-track');
  const $volFill = document.getElementById('call-vol-fill');
  const $volValue = document.getElementById('call-vol-value');
  function setVolume(v) {
    volume = Math.max(0, Math.min(100, v));
    if ($volFill) $volFill.style.width = volume + '%';
    if ($volValue) $volValue.textContent = String(volume);
    if (videoPeer) videoPeer.volume = volume / 100;
    // all remote tile videos (group participants + screen), but not our own.
    stage.querySelectorAll('.ax-call-tile:not(#call-tile-self) video').forEach(v => { v.volume = volume / 100; });
  }
  if ($volTrack) {
    $volTrack.addEventListener('pointerdown', function (e) {
      const drag = (ev) => {
        const r = $volTrack.getBoundingClientRect();
        const x = Math.max(0, Math.min(r.width, ev.clientX - r.left));
        setVolume(Math.round((x / r.width) * 100));
      };
      drag(e);
      const move = (ev) => drag(ev);
      const up = () => { window.removeEventListener('pointermove', move); window.removeEventListener('pointerup', up); };
      window.addEventListener('pointermove', move);
      window.addEventListener('pointerup', up);
    });
  }
  setVolume(80);

  // --- chat: jump to the DM/channel this call belongs to (call keeps running) ---
  // Chat routes key conversations by pub (dm) / channel id (group), while
  // active.id is the display label for a dm — so use the stored pub there.
  $(document).on('click', '#call-chat', function () {
    const a = Call.active;
    if (!a) return;
    location.hash = a.kind === 'group'
      ? 'chat:channel:' + encHash(a.id)
      : 'chat:dm:' + encodeURIComponent(a.pub || a.id);
  });

  // --- hang up ---
  $(document).on('click', '#call-hangup', function () { Call.hangup(); });

  // The chat header call button starts a call for the chat we're in. The
  // channel id is "<ownerPub>/<name>" — ugly as a title, so take the human
  // name from the chat header instead.
  $('#chat-call').on('click', function () {
    const h = decodeURIComponent((location.hash || '').replace(/^#/, ''));
    const m = h.match(/^chat:(dm|channel):(.+)$/);
    if (!m) return;
    const title = $('#chat-title').text().replace(/^# /, '');
    if (m[1] === 'channel') Call.start('group', m[2], title || m[2]);
    else Call.start('dm', m[2]);
  });

  // ---- router: #call:<kind>:<id> shows the call window ----
  function activateCallTab() {
    $('.ax-tab').removeClass('ax-tab-active');
    $('.tab-panel').addClass('hidden');
    $('#tab-call').removeClass('hidden');
  }
  function showStageView() {
    $('#call-ended').hide();
    $('#call-stage, #call-bottombar, .ax-call-header').show();
    reflowStage(); // stage now has size → lay out tiles
  }
  function showEndedView(label) {
    activateCallTab();
    $('#call-stage, #call-bottombar, .ax-call-header').hide();
    $('#call-ended').css('display', 'flex');
    $('#call-ended-sub').text(label ? ('звонок с ' + label + ' завершён') : '');
  }
  function applyCallRoute() {
    const raw = (location.hash || '').replace(/^#/, '');
    const ended = raw.match(/^call:ended(?::(.+))?$/);
    if (ended) { showEndedView(ended[1] ? decodeURIComponent(ended[1]) : ''); return; }
    const m = decodeURIComponent(raw).match(/^call:(dm|group):(.+)$/);
    if (!m) return;
    const [, kind, id] = m;
    // dm routes carry the peer PUB; the active call matches on it (id holds
    // the display name). Fallback titles: group id is "<ownerPub>/<name>" —
    // show the name part; a bare pub shows a short prefix until resolved.
    const a = Call.active;
    const matches = a && (a.id === id || a.pub === id);
    const title = matches ? a.title
      : (kind === 'group' ? (id.split('/').pop() || id)
         : (PUB_RE.test(id) ? id.slice(0, 12) : id));
    // Paint the stage FIRST so the tiles window is shown regardless of join
    // timing — Call.start below sets the same hash, so it would NOT re-fire this
    // router, and the stage must already be up by then.
    activateCallTab();
    showStageView();
    // The fixed p2p peer tile is dm-only; group renders dynamic participant tiles.
    $('#call-tile-peer').toggle(kind === 'dm');
    $('#call-title').text((kind === 'group' ? '# ' : '') + title);
    $('#call-peer-name').text((kind === 'group' ? '# ' : '') + title);
    // A group route we're not already in means the user navigated here (clicked
    // a meet in the sidebar, or reloaded mid-call) WITHOUT joining — join now.
    if (kind === 'group' && !(Call.active && Call.active.kind === 'group' && Call.active.id === id)) {
      Call.start('group', id, title);
    }
  }
  window.addEventListener('hashchange', applyCallRoute);
  applyCallRoute();

  // "close" on the ended screen → reset for next call and leave the call tab.
  $('#call-ended-close').on('click', function () {
    showStageView();
    if ((location.hash || '').indexOf('#call:') === 0) location.hash = '';
    $('.ax-tab').first().trigger('click'); // back to the default (first) tab
  });

  startSignaling();

  // Release the camera/mic the instant this page unloads (reload/close) so the
  // freshly-loaded page can re-acquire them without hitting a busy device.
  window.addEventListener('pagehide', function () {
    if (localStream) { try { localStream.getTracks().forEach(t => t.stop()); } catch (_) {} }
  });

  // ---- auto-rejoin after a tab reload ----
  // If we reloaded mid-call, re-offer to the saved peer with a fresh offer; the
  // other side drops its stale connection and answers, restoring the call.
  (function autoRejoin() {
    let saved = null;
    try { saved = JSON.parse(sessionStorage.getItem(CALL_KEY) || 'null'); } catch (_) {}
    if (!saved) return;
    // Group calls re-join via the URL hash through applyCallRoute() above, so
    // only dm needs explicit rejoin here (avoids a double Call.start on reload).
    if (saved.kind !== 'dm') return;
    const pub = PUB_RE.test(saved.pub || '') ? saved.pub : saved.id; // reconnect by pub
    Call.start('dm', pub, saved.title);
  })();
});
