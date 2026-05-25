// meet2.js — group calls via embedded Pion SFU.
//
// One participant creates a call (their engine becomes the SFU host).
// Others discover the call via polling /api/call/state (engine polls
// all peers' /api/call/state through the tunnel). Join sends the
// request to the SFU host, and WebRTC signals go there too.

(function () {
  const POLL_MS = 3000;

  function start() {
    const $status     = document.getElementById('m2-status');
    const $grid       = document.getElementById('m2-grid');
    const $controls   = document.getElementById('m2-controls');
    const $actions    = document.getElementById('m2-actions');
    const $btnCreate  = document.getElementById('m2-btn-create');
    const $btnJoin    = document.getElementById('m2-btn-join');
    const $btnLeave   = document.getElementById('m2-btn-leave');
    const $btnMic     = document.getElementById('m2-btn-mic');
    const $btnCam     = document.getElementById('m2-btn-cam');
    const $videoSelf  = document.getElementById('m2-video-self');
    const $labelSelf  = document.getElementById('m2-label-self');
    const $partList   = document.getElementById('m2-participant-list');
    const $logBody    = document.getElementById('m2-log-body');
    const $logCount   = document.getElementById('m2-log-count');
    const $logClear   = document.getElementById('m2-log-clear');

    let pc = null;
    let signalES = null;
    let localStream = null;
    let micEnabled = false;
    let camEnabled = false;
    let inCall = false;
    let myTunnelIP = '';
    let myName = '';
    let sfuHost = '';  // tunnel_ip of the SFU host (empty = we are host)
    let logCount = 0;
    let pendingCandidates = [];
    let hasRemoteDesc = false;

    function log(msg) {
      logCount++;
      $logCount.textContent = logCount;
      const line = document.createElement('div');
      line.className = 'ax-log-line';
      const ts = new Date().toLocaleTimeString('en-GB', { hour12: false });
      line.textContent = ts + ' ' + msg;
      $logBody.appendChild(line);
      $logBody.scrollTop = $logBody.scrollHeight;
    }

    $logClear.addEventListener('click', function () {
      $logBody.innerHTML = '';
      logCount = 0;
      $logCount.textContent = '0';
    });

    async function fetchJSON(url, opts) {
      const r = await fetch(url, opts);
      if (!r.ok) {
        const t = await r.text();
        throw new Error(r.status + ': ' + t);
      }
      if (r.status === 204) return null;
      return r.json();
    }

    async function getStatus() {
      try {
        const s = await fetchJSON('/api/status');
        if (s && s.running) {
          myTunnelIP = s.local_ip || '';
          myName = s.camp_name || 'you';
          $labelSelf.textContent = myName + ' @ ' + myTunnelIP;
        }
        return s;
      } catch (e) {
        return null;
      }
    }

    async function pollCallState() {
      try {
        const cs = await fetchJSON('/api/call/state');
        updateCallUI(cs);
      } catch (e) {
        // ignore
      }
    }

    function updateCallUI(cs) {
      if (!cs || !cs.call_id) {
        $status.innerHTML = '<span class="text-zinc-500">no active call in camp</span>';
        $btnCreate.style.display = '';
        $btnJoin.style.display = 'none';
        sfuHost = '';
        if (!inCall) {
          $controls.style.display = 'none';
          $partList.textContent = '—';
        }
        return;
      }
      sfuHost = cs.sfu_host || '';
      var hostLabel = cs.sfu_host;
      if (cs.sfu_host === myTunnelIP) {
        hostLabel = 'you (host)';
      }
      var parts = (cs.participants || []).map(function (p) { return p.name; }).join(', ');
      if (!inCall) {
        $status.innerHTML = '<span class="text-emerald-400">call active</span> — host: ' +
          hostLabel + (parts ? ' — in call: ' + parts : '');
        $btnCreate.style.display = 'none';
        $btnJoin.style.display = '';
      } else {
        $status.innerHTML = '<span class="text-emerald-400">in call</span> — host: ' +
          hostLabel + (parts ? ' — ' + parts : '');
      }
      renderParticipants(cs.participants || []);
    }

    function renderParticipants(list) {
      if (!list.length) {
        $partList.textContent = '—';
        return;
      }
      $partList.textContent = list.map(function (p) {
        return p.name + ' (' + p.tunnel_ip + ')';
      }).join(' · ');
    }

    // --- WebRTC ---

    function createPC() {
      var conn = new RTCPeerConnection({ iceServers: [] });

      conn.onicecandidate = function (e) {
        if (e.candidate) {
          sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
        }
      };

      conn.ontrack = function (e) {
        log('remote track: ' + e.track.kind + ' stream=' + (e.streams[0] ? e.streams[0].id : '?'));
        if (e.streams && e.streams[0]) {
          addRemoteStream(e.streams[0]);
        }
      };

      conn.onconnectionstatechange = function () {
        log('pc state: ' + conn.connectionState);
        if (conn.connectionState === 'failed' || conn.connectionState === 'closed') {
          leaveCall();
        }
      };

      conn.onnegotiationneeded = async function () {
        try {
          var offer = await conn.createOffer();
          await conn.setLocalDescription(offer);
          var resp = await sendSignal({ kind: 'offer', sdp: offer.sdp });
          // If SFU renegotiated while we waited for the POST response,
          // our PC is already back in 'stable' — skip the stale answer.
          if (resp && resp.kind === 'answer' && conn.signalingState === 'have-local-offer') {
            await conn.setRemoteDescription({ type: 'answer', sdp: resp.sdp });
            hasRemoteDesc = true;
            await flushPendingCandidates();
          }
        } catch (err) {
          log('negotiation: ' + err.message);
        }
      };

      return conn;
    }

    // sendSignal delivers a WebRTC signal to the SFU.
    // If we are the SFU host, it goes to our local /api/call/signal.
    // If remote, it goes to the SFU host through the tunnel via our
    // local proxy endpoint (which the Go server forwards).
    async function sendSignal(msg) {
      try {
        var r = await fetch('/api/call/signal', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(msg),
        });
        if (r.status === 204) return null;
        if (!r.ok) {
          log('signal error: ' + r.status);
          return null;
        }
        return r.json();
      } catch (e) {
        log('signal send failed: ' + e.message);
        return null;
      }
    }

    function startSignalStream() {
      if (signalES) return;
      signalES = new EventSource('/api/call/signal/stream');
      signalES.onopen = function () { log('signal stream open'); };
      signalES.onerror = function () { log('signal stream error'); };
      signalES.onmessage = async function (e) {
        try {
          await handleSignal(JSON.parse(e.data));
        } catch (err) {
          log('signal handle error: ' + err.message);
        }
      };
    }

    function stopSignalStream() {
      if (signalES) {
        signalES.close();
        signalES = null;
      }
    }

    async function flushPendingCandidates() {
      while (pendingCandidates.length > 0) {
        var c = pendingCandidates.shift();
        try { await pc.addIceCandidate(c); } catch (e) { /* ignore stale */ }
      }
    }

    async function handleSignal(msg) {
      if (!pc) return;
      if (msg.kind === 'offer' && msg.from === 'sfu') {
        hasRemoteDesc = false;
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        hasRemoteDesc = true;
        var answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        await flushPendingCandidates();
      } else if (msg.kind === 'answer' && msg.from === 'sfu') {
        await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
        hasRemoteDesc = true;
        await flushPendingCandidates();
      } else if (msg.kind === 'candidate' && msg.from === 'sfu') {
        if (hasRemoteDesc) {
          try { await pc.addIceCandidate(msg.candidate); } catch (e) { /* ignore */ }
        } else {
          pendingCandidates.push(msg.candidate);
        }
      }
    }

    // --- media ---

    async function acquireMedia() {
      try {
        localStream = await navigator.mediaDevices.getUserMedia({ audio: true, video: true });
        micEnabled = true;
        camEnabled = true;
      } catch (e) {
        try {
          localStream = await navigator.mediaDevices.getUserMedia({ audio: true });
          micEnabled = true;
          camEnabled = false;
        } catch (e2) {
          log('no media: ' + e2.message);
          localStream = null;
          return;
        }
      }
      $videoSelf.srcObject = localStream;
      updateMediaButtons();
    }

    function updateMediaButtons() {
      $btnMic.disabled = !localStream;
      $btnCam.disabled = !localStream;
      $btnMic.querySelector('.ax-btn-state').textContent = micEnabled ? '●' : '○';
      $btnCam.querySelector('.ax-btn-state').textContent = camEnabled ? '■' : '□';
    }

    // --- remote video tiles ---
    var remoteTiles = {};

    function addRemoteStream(stream) {
      if (remoteTiles[stream.id]) return;
      var tile = document.createElement('div');
      tile.className = 'm2-tile';
      tile.id = 'm2-tile-' + stream.id;
      var video = document.createElement('video');
      video.autoplay = true;
      video.playsInline = true;
      video.srcObject = stream;
      var label = document.createElement('div');
      label.className = 'm2-tile-label';
      label.textContent = 'peer';
      tile.appendChild(video);
      tile.appendChild(label);
      $grid.appendChild(tile);
      remoteTiles[stream.id] = tile;

      stream.onremovetrack = function () {
        if (stream.getTracks().length === 0) {
          removeRemoteStream(stream.id);
        }
      };
    }

    function removeRemoteStream(streamId) {
      var tile = remoteTiles[streamId];
      if (tile) {
        tile.remove();
        delete remoteTiles[streamId];
      }
    }

    function clearRemoteTiles() {
      for (var id in remoteTiles) {
        remoteTiles[id].remove();
        delete remoteTiles[id];
      }
    }

    // --- call actions ---

    async function createCall() {
      log('creating call...');
      try {
        var cs = await fetchJSON('/api/call/create', { method: 'POST' });
        log('call created: ' + cs.call_id);
        sfuHost = myTunnelIP;
        await joinSFU();
      } catch (e) {
        log('create failed: ' + e.message);
      }
    }

    async function joinCall() {
      if (!sfuHost) {
        log('no sfu host known');
        return;
      }
      log('joining call on ' + sfuHost + '...');
      try {
        // Register ourselves as a participant on the SFU host.
        // This goes through our local server which proxies to the
        // SFU host's tunnel listener.
        await fetchJSON('/api/call/join', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ tunnel_ip: myTunnelIP, name: myName, sfu_host: sfuHost }),
        });
        await joinSFU();
      } catch (e) {
        log('join failed: ' + e.message);
      }
    }

    async function joinSFU() {
      await acquireMedia();
      pc = createPC();
      if (localStream) {
        var tracks = localStream.getTracks();
        for (var i = 0; i < tracks.length; i++) {
          pc.addTrack(tracks[i], localStream);
        }
      } else {
        // No local media — add recv-only transceivers so we can still
        // receive remote tracks and trigger a proper SDP negotiation.
        pc.addTransceiver('audio', { direction: 'recvonly' });
        pc.addTransceiver('video', { direction: 'recvonly' });
      }
      startSignalStream();
      inCall = true;
      $actions.style.display = 'none';
      $controls.style.display = '';
      $btnLeave.style.display = '';
      log('connected to SFU');
    }

    async function leaveCall() {
      if (!inCall) return;
      inCall = false;
      stopSignalStream();
      if (pc) {
        pc.close();
        pc = null;
      }
      if (localStream) {
        localStream.getTracks().forEach(function (t) { t.stop(); });
        localStream = null;
      }
      $videoSelf.srcObject = null;
      clearRemoteTiles();
      $controls.style.display = 'none';
      $btnLeave.style.display = 'none';
      $actions.style.display = '';
      pendingCandidates = [];
      hasRemoteDesc = false;
      sfuHost = '';
      try {
        await fetchJSON('/api/call/leave', { method: 'POST' });
      } catch (e) { /* ignore */ }
      log('left call');
    }

    // --- button handlers ---

    $btnCreate.addEventListener('click', createCall);
    $btnJoin.addEventListener('click', joinCall);
    $btnLeave.addEventListener('click', leaveCall);

    $btnMic.addEventListener('click', function () {
      if (!localStream) return;
      micEnabled = !micEnabled;
      localStream.getAudioTracks().forEach(function (t) { t.enabled = micEnabled; });
      updateMediaButtons();
    });

    $btnCam.addEventListener('click', function () {
      if (!localStream) return;
      camEnabled = !camEnabled;
      localStream.getVideoTracks().forEach(function (t) { t.enabled = camEnabled; });
      updateMediaButtons();
    });

    // --- init ---

    getStatus().then(function () {
      pollCallState();
      setInterval(pollCallState, POLL_MS);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
