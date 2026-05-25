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
        var calls = await fetchJSON('/api/call/list');
        updateCallListUI(calls || []);
      } catch (e) {
        // ignore
      }
    }

    function updateCallListUI(calls) {
      // Find the call we're currently in (if any).
      var myCall = null;
      if (inCall && sfuHost) {
        for (var i = 0; i < calls.length; i++) {
          if (calls[i].sfu_host === sfuHost) {
            myCall = calls[i];
            break;
          }
        }
      }

      if (inCall && myCall) {
        var parts = (myCall.participants || []).map(function (p) { return p.name; }).join(', ');
        $status.innerHTML = '<span class="text-emerald-400">in call</span>' + (parts ? ' — ' + parts : '');
        $btnCreate.style.display = 'none';
        $btnJoin.style.display = 'none';
        syncPeerTiles(myCall.participants || []);
        return;
      }

      if (inCall && !myCall) {
        // Our call disappeared (host left)
        leaveCall();
        return;
      }

      // Not in a call — show available calls + create button.
      $controls.style.display = 'none';
      $btnCreate.style.display = '';

      if (!calls.length) {
        $status.innerHTML = '<span class="text-zinc-500">no active calls in camp</span>';
        $btnJoin.style.display = 'none';
        $partList.textContent = '—';
        return;
      }

      // Render call list
      var html = '';
      for (var i = 0; i < calls.length; i++) {
        var c = calls[i];
        var hostLabel = c.sfu_host;
        if (c.sfu_host === myTunnelIP) hostLabel = 'you';
        var parts = (c.participants || []).map(function (p) { return p.name; }).join(', ');
        html += '<div class="m2-call-item" style="margin-bottom:6px">' +
          '<span class="text-emerald-400">●</span> host: ' + hostLabel +
          (parts ? ' — ' + parts : '') +
          (c.sfu_host !== myTunnelIP ?
            ' <button class="m2-join-btn" data-host="' + c.sfu_host + '">join</button>' : '') +
          '</div>';
      }
      $partList.innerHTML = html;

      // Bind join buttons
      var btns = $partList.querySelectorAll('.m2-join-btn');
      for (var j = 0; j < btns.length; j++) {
        btns[j].addEventListener('click', function () {
          sfuHost = this.getAttribute('data-host');
          joinCall();
        });
      }
      $btnJoin.style.display = 'none';
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

    // --- remote tiles ---
    var remoteTiles = {};      // streamId → {tile, peerIP}
    var peerTiles = {};        // tunnelIP → tile element (placeholder or video)
    var peersWithVideo = {};   // tunnelIP → true if we have a video stream

    function addRemoteStream(stream) {
      if (remoteTiles[stream.id]) return;
      // SFU stream ID format: "tunnelIP-originalStreamID"
      var peerIP = extractPeerIP(stream.id);

      var tile = document.createElement('div');
      tile.className = 'm2-tile';
      var video = document.createElement('video');
      video.autoplay = true;
      video.playsInline = true;
      video.srcObject = stream;
      var label = document.createElement('div');
      label.className = 'm2-tile-label';
      label.textContent = peerIP;
      tile.appendChild(video);
      tile.appendChild(label);

      // Replace placeholder if one exists for this peer
      if (peerIP && peerTiles[peerIP]) {
        peerTiles[peerIP].replaceWith(tile);
      } else {
        $grid.appendChild(tile);
      }
      if (peerIP) {
        peerTiles[peerIP] = tile;
        peersWithVideo[peerIP] = true;
      }
      remoteTiles[stream.id] = { tile: tile, peerIP: peerIP };
      log('video tile for ' + peerIP);

      stream.onremovetrack = function () {
        if (stream.getTracks().length === 0) {
          removeRemoteStream(stream.id);
        }
      };
    }

    function extractPeerIP(streamId) {
      // "100.80.47.142-xxxxx" → "100.80.47.142"
      var m = streamId.match(/^(\d+\.\d+\.\d+\.\d+)-/);
      return m ? m[1] : '';
    }

    function removeRemoteStream(streamId) {
      var entry = remoteTiles[streamId];
      if (entry) {
        entry.tile.remove();
        if (entry.peerIP) {
          delete peerTiles[entry.peerIP];
          delete peersWithVideo[entry.peerIP];
        }
        delete remoteTiles[streamId];
      }
    }

    // Called from pollCallState — ensure every remote participant has a tile.
    function syncPeerTiles(participants) {
      if (!inCall || !participants) return;
      var activeIPs = {};
      for (var i = 0; i < participants.length; i++) {
        var p = participants[i];
        if (p.tunnel_ip === myTunnelIP) continue;
        activeIPs[p.tunnel_ip] = p.name;

        // Already has a tile (video or placeholder)
        if (peerTiles[p.tunnel_ip]) {
          // Update label
          var lbl = peerTiles[p.tunnel_ip].querySelector('.m2-tile-label');
          if (lbl) lbl.textContent = p.name + ' @ ' + p.tunnel_ip;
          continue;
        }

        // Create placeholder
        var tile = document.createElement('div');
        tile.className = 'm2-tile m2-tile-placeholder';
        var noVid = document.createElement('div');
        noVid.className = 'm2-no-video';
        noVid.textContent = p.name;
        var label = document.createElement('div');
        label.className = 'm2-tile-label';
        label.textContent = p.name + ' @ ' + p.tunnel_ip;
        tile.appendChild(noVid);
        tile.appendChild(label);
        $grid.appendChild(tile);
        peerTiles[p.tunnel_ip] = tile;
      }
      // Remove tiles for peers that left
      for (var ip in peerTiles) {
        if (!activeIPs[ip]) {
          peerTiles[ip].remove();
          delete peerTiles[ip];
          delete peersWithVideo[ip];
        }
      }
    }

    function clearRemoteTiles() {
      for (var id in remoteTiles) {
        remoteTiles[id].tile.remove();
        delete remoteTiles[id];
      }
      for (var ip in peerTiles) {
        peerTiles[ip].remove();
        delete peerTiles[ip];
      }
      peersWithVideo = {};
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
      $btnCreate.style.display = '';
      $btnJoin.style.display = 'none';
      $status.innerHTML = '<span class="text-zinc-500">no active call in camp</span>';
      $partList.textContent = '—';
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
