// meet2.js — group calls via embedded Pion SFU.
//
// One participant creates a call (their engine becomes the SFU host).
// Others discover the call via polling /api/call/state (engine polls
// all peers' /api/call/state through the tunnel). Join sends the
// request to the SFU host, and WebRTC signals go there too.

(function () {
  const POLL_MS = 3000;

  function start() {
    const $grid       = document.getElementById('m2-grid');
    const $groupSel   = document.getElementById('m2-group-select');
    const $btnCreate  = document.getElementById('m2-btn-create');
    const $btnLeave   = document.getElementById('m2-btn-leave');
    const $btnMic     = document.getElementById('m2-btn-mic');
    const $btnCam     = document.getElementById('m2-btn-cam');
    const $videoSelf  = document.getElementById('m2-video-self');
    const $labelSelf  = document.getElementById('m2-label-self');
    const $logBody    = document.getElementById('m2-log-body');
    const $logCount   = document.getElementById('m2-log-count');
    const $logClear   = document.getElementById('m2-log-clear');

    const $chatInput  = document.getElementById('m2-chat-input');
    const $btnShare   = document.getElementById('m2-btn-share');

    let pc = null;
    let dataChannel = null;
    let signalES = null;
    let localStream = null;
    let screenStream = null;
    let screenSenders = [];
    let micEnabled = false;
    let camEnabled = false;
    let inCall = false;
    let myTunnelIP = '';
    let myName = '';
    let sfuHost = '';
    let logCount = 0;
    let pendingCandidates = [];
    let hasRemoteDesc = false;

    function timestamp() {
      return new Date().toLocaleTimeString('en-GB', { hour12: false });
    }

    function appendRow(row) {
      logCount++;
      $logCount.textContent = logCount;
      $logBody.appendChild(row);
      $logBody.scrollTop = $logBody.scrollHeight;
    }

    function log(msg) {
      var row = document.createElement('div');
      row.className = 'ax-log-event';
      var c1 = document.createElement('span');
      c1.className = 'ax-log-time';
      c1.textContent = timestamp();
      var c2 = document.createElement('span');
      c2.className = 'ax-log-icon event';
      c2.textContent = '›';
      var c3 = document.createElement('span');
      c3.className = 'ax-log-msg';
      c3.textContent = msg;
      row.append(c1, c2, c3);
      appendRow(row);
    }

    function logChat(who, text) {
      var row = document.createElement('div');
      row.className = 'ax-log-event is-chat' + (who === myName ? ' from-you' : '');
      var c1 = document.createElement('span');
      c1.className = 'ax-log-time';
      c1.textContent = timestamp();
      var c2 = document.createElement('span');
      c2.className = 'ax-log-icon';
      c2.textContent = '<' + who + '>';
      var c3 = document.createElement('span');
      c3.className = 'ax-log-msg';
      c3.textContent = text;
      row.append(c1, c2, c3);
      appendRow(row);
    }

    $logClear.addEventListener('click', function () {
      $logBody.innerHTML = '';
      logCount = 0;
      $logCount.textContent = '0';
    });

    function attachDataChannel(channel) {
      dataChannel = channel;
      channel.onopen = function () {
        $chatInput.disabled = false;
        $chatInput.placeholder = 'type a message...';
        log('chat channel open');
      };
      channel.onclose = function () {
        $chatInput.disabled = true;
        $chatInput.placeholder = 'join a call to chat';
      };
      channel.onmessage = function (e) {
        var text = String(e.data || '').slice(0, 4096).trim();
        if (text) {
          try {
            var msg = JSON.parse(text);
            logChat(msg.name || 'peer', msg.text || text);
          } catch (err) {
            logChat('peer', text);
          }
        }
      };
    }

    function sendChat(text) {
      if (!dataChannel || dataChannel.readyState !== 'open') return false;
      try {
        dataChannel.send(JSON.stringify({ name: myName, text: text }));
      } catch (err) {
        log('chat send failed: ' + err.message);
        return false;
      }
      logChat(myName, text);
      return true;
    }

    $chatInput.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter') return;
      var text = $chatInput.value.trim();
      if (!text) return;
      if (sendChat(text)) $chatInput.value = '';
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
      // Update dropdown with available groups.
      var selectedVal = $groupSel.value;
      var opts = '<option value="">— group —</option>';
      for (var i = 0; i < calls.length; i++) {
        var c = calls[i];
        var label = c.sfu_host === myTunnelIP ? 'you' : c.sfu_host;
        var parts = (c.participants || []).map(function (p) { return p.name; }).join(', ');
        if (parts) label += ' (' + parts + ')';
        opts += '<option value="' + c.sfu_host + '">' + label + '</option>';
      }
      $groupSel.innerHTML = opts;
      if (inCall && sfuHost) {
        $groupSel.value = sfuHost;
      } else if (selectedVal) {
        $groupSel.value = selectedVal;
      }

      // If in a call, sync tiles and check if call still exists.
      if (inCall && sfuHost) {
        var myCall = null;
        for (var i = 0; i < calls.length; i++) {
          if (calls[i].sfu_host === sfuHost) { myCall = calls[i]; break; }
        }
        if (myCall) {
          syncPeerTiles(myCall.participants || []);
        } else {
          leaveCall();
        }
      }
    }

    // --- WebRTC ---

    function createPC() {
      var conn = new RTCPeerConnection({ iceServers: [] });

      // Chat DataChannel — create before offer so it's in the SDP.
      var dc = conn.createDataChannel('chat');
      attachDataChannel(dc);

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

      conn.ondatachannel = function (e) {
        attachDataChannel(e.channel);
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
        log('SFU offer (state=' + pc.signalingState + ')');
        if (pc.signalingState === 'have-local-offer') {
          await pc.setLocalDescription({ type: 'rollback' });
          log('rolled back local offer');
        }
        hasRemoteDesc = false;
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        hasRemoteDesc = true;
        var answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        log('sent answer to SFU');
        await flushPendingCandidates();
      } else if (msg.kind === 'answer' && msg.from === 'sfu') {
        await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
        hasRemoteDesc = true;
        await flushPendingCandidates();
      } else if (msg.kind === 'renegotiate' && msg.from === 'sfu') {
        var needed = msg.tracks || [];
        log('SFU requested renegotiation (' + needed.length + ' sender tracks)');
        try {
          var needCount = {};
          for (var ti = 0; ti < needed.length; ti++) {
            var k = needed[ti].kind;
            needCount[k] = (needCount[k] || 0) + 1;
          }
          var transceivers = pc.getTransceivers();
          var haveCount = {};
          for (var tj = 0; tj < transceivers.length; tj++) {
            var tr = transceivers[tj];
            if (tr.direction === 'recvonly' || tr.direction === 'sendrecv') {
              var rk = tr.receiver.track.kind;
              haveCount[rk] = (haveCount[rk] || 0) + 1;
            }
          }
          for (var kind in needCount) {
            var deficit = needCount[kind] - (haveCount[kind] || 0);
            for (var d = 0; d < deficit; d++) {
              pc.addTransceiver(kind, { direction: 'recvonly' });
              log('added recvonly transceiver: ' + kind);
            }
          }
          var offer = await pc.createOffer();
          await pc.setLocalDescription(offer);
          var resp = await sendSignal({ kind: 'offer', sdp: offer.sdp });
          if (resp && resp.kind === 'answer' && pc.signalingState === 'have-local-offer') {
            await pc.setRemoteDescription({ type: 'answer', sdp: resp.sdp });
            hasRemoteDesc = true;
            await flushPendingCandidates();
          }
        } catch (err) {
          log('renegotiation failed: ' + err.message);
        }
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
      var hasMic = !!localStream && localStream.getAudioTracks().length > 0;
      var hasCam = !!localStream && localStream.getVideoTracks().length > 0;
      $btnMic.disabled = !hasMic;
      $btnCam.disabled = !hasCam;
      $btnMic.classList.toggle('active', micEnabled);
      $btnCam.classList.toggle('active', camEnabled);
      $btnMic.querySelector('.ax-btn-state').textContent = micEnabled ? '●' : '○';
      $btnCam.querySelector('.ax-btn-state').textContent = camEnabled ? '■' : '□';
    }

    // --- remote tiles ---
    var remoteTiles = {};      // streamId → {tile, peerIP}
    var peerTiles = {};        // tunnelIP → tile element (placeholder or video)
    var peersWithVideo = {};   // tunnelIP → true if we have a video stream

    function addRemoteStream(stream) {
      if (remoteTiles[stream.id]) return;
      var peerIP = extractPeerIP(stream.id);

      var tile = document.createElement('div');
      tile.className = 'm2-tile';
      var video = document.createElement('video');
      video.autoplay = true;
      video.playsInline = true;
      video.volume = volume / 100;
      video.srcObject = stream;
      var label = document.createElement('div');
      label.className = 'm2-tile-label';
      label.textContent = peerIP || 'peer';
      var fsBtn = document.createElement('button');
      fsBtn.className = 'm2-tile-fs';
      fsBtn.title = 'fullscreen';
      fsBtn.textContent = '⛶';
      tile.appendChild(video);
      tile.appendChild(label);
      tile.appendChild(fsBtn);

      if (peerTiles[peerIP] && !peersWithVideo[peerIP]) {
        peerTiles[peerIP].replaceWith(tile);
        peerTiles[peerIP] = tile;
      } else if (!peerTiles[peerIP]) {
        $grid.appendChild(tile);
        peerTiles[peerIP] = tile;
      } else {
        $grid.appendChild(tile);
      }
      peersWithVideo[peerIP] = true;
      remoteTiles[stream.id] = { tile: tile, peerIP: peerIP };
      log('video tile for ' + peerIP);

      stream.onremovetrack = function () {
        if (stream.getTracks().length === 0) {
          removeRemoteStream(stream.id);
        }
      };
      stream.getTracks().forEach(function (t) {
        t.onended = function () {
          var alive = stream.getTracks().some(function (tr) { return tr.readyState === 'live'; });
          if (!alive) removeRemoteStream(stream.id);
        };
        t.onmute = function () {
          var alive = stream.getTracks().some(function (tr) { return !tr.muted && tr.readyState === 'live'; });
          if (!alive) removeRemoteStream(stream.id);
        };
      });
    }

    function extractPeerIP(streamId) {
      // "100.80.47.142-xxxxx" → "100.80.47.142"
      var m = streamId.match(/^(\d+\.\d+\.\d+\.\d+)-/);
      return m ? m[1] : '';
    }

    function removeRemoteStream(streamId) {
      var entry = remoteTiles[streamId];
      if (!entry) return;
      entry.tile.remove();
      delete remoteTiles[streamId];
      if (!entry.peerIP) return;
      if (peerTiles[entry.peerIP] === entry.tile) {
        delete peerTiles[entry.peerIP];
      }
      var hasOther = false;
      for (var id in remoteTiles) {
        if (remoteTiles[id].peerIP === entry.peerIP) { hasOther = true; break; }
      }
      if (!hasOther) delete peersWithVideo[entry.peerIP];
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
        var fsBtn = document.createElement('button');
        fsBtn.className = 'm2-tile-fs';
        fsBtn.title = 'fullscreen';
        fsBtn.textContent = '⛶';
        tile.appendChild(noVid);
        tile.appendChild(label);
        tile.appendChild(fsBtn);
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
      if (inCall) await leaveCall();
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
      var target = sfuHost;
      if (inCall) await leaveCall();
      sfuHost = target;
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
      $btnCreate.style.display = 'none';
      $btnLeave.style.display = '';
      $btnLeave.querySelector('span:last-child').textContent =
        sfuHost === myTunnelIP ? 'leave (host)' : 'leave';
      $btnShare.disabled = false;
      updateMediaButtons();
      log('connected to SFU');
    }

    async function leaveCall() {
      if (!inCall) return;
      inCall = false;
      if (screenStream) stopScreenShare();
      stopSignalStream();
      dataChannel = null;
      $chatInput.disabled = true;
      $chatInput.placeholder = 'join a call to chat';
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
      $btnLeave.style.display = 'none';
      $btnLeave.querySelector('span:last-child').textContent = 'leave';
      $btnCreate.style.display = '';
      micEnabled = false;
      camEnabled = false;
      $btnShare.disabled = true;
      updateMediaButtons();
      pendingCandidates = [];
      hasRemoteDesc = false;
      sfuHost = '';
      $groupSel.value = '';
      try {
        await fetchJSON('/api/call/leave', { method: 'POST' });
      } catch (e) { /* ignore */ }
      log('left call');
    }

    // --- button handlers ---

    $btnCreate.addEventListener('click', createCall);
    $btnLeave.addEventListener('click', leaveCall);

    $groupSel.addEventListener('change', function () {
      var host = $groupSel.value;
      if (!host) return;
      sfuHost = host;
      joinCall();
    });

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

    // --- fullscreen ---

    $grid.addEventListener('click', function (e) {
      var btn = e.target.closest('.m2-tile-fs');
      if (!btn) return;
      e.stopPropagation();
      var tile = btn.closest('.m2-tile');
      var video = tile && tile.querySelector('video');
      if (video && video.requestFullscreen) {
        video.requestFullscreen().catch(function () {});
      }
    });

    // --- screen share ---

    $btnShare.addEventListener('click', function () {
      if (!pc) return;
      if (screenStream) stopScreenShare();
      else startScreenShare();
    });

    async function startScreenShare() {
      if (!pc || screenStream) return;
      if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia) {
        log('getDisplayMedia not supported');
        return;
      }
      var stream;
      try {
        stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true });
      } catch (err) {
        if (err.name !== 'NotAllowedError') log('getDisplayMedia: ' + err.message);
        return;
      }
      screenStream = stream;
      screenSenders = [];
      stream.getTracks().forEach(function (t) {
        screenSenders.push(pc.addTrack(t, stream));
        t.addEventListener('ended', stopScreenShare);
      });
      // Local preview tile
      var tile = document.createElement('div');
      tile.className = 'm2-tile m2-tile-screen';
      tile.id = 'm2-tile-screen-self';
      var video = document.createElement('video');
      video.autoplay = true;
      video.muted = true;
      video.playsInline = true;
      video.srcObject = stream;
      var label = document.createElement('div');
      label.className = 'm2-tile-label';
      label.textContent = myName + ' · screen';
      var fsBtn = document.createElement('button');
      fsBtn.className = 'm2-tile-fs';
      fsBtn.title = 'fullscreen';
      fsBtn.textContent = '⛶';
      tile.appendChild(video);
      tile.appendChild(label);
      tile.appendChild(fsBtn);
      $grid.appendChild(tile);
      $btnShare.querySelector('.ax-btn-state').textContent = '■';
      $btnShare.classList.add('active');
      log('screen share started');
    }

    function stopScreenShare() {
      if (!screenStream) return;
      var stream = screenStream;
      screenStream = null;
      stream.getTracks().forEach(function (t) { t.stop(); });
      if (pc) {
        screenSenders.forEach(function (s) { try { pc.removeTrack(s); } catch (_) {} });
      }
      screenSenders = [];
      var selfScreen = document.getElementById('m2-tile-screen-self');
      if (selfScreen) selfScreen.remove();
      $btnShare.querySelector('.ax-btn-state').textContent = '▢';
      $btnShare.classList.remove('active');
      log('screen share stopped');
    }

    // --- volume ---
    var $volTrack = document.getElementById('m2-vol-track');
    var $volFill  = document.getElementById('m2-vol-fill');
    var $volValue = document.getElementById('m2-vol-value');
    var volume = 80;

    function setVolume(v) {
      volume = Math.max(0, Math.min(100, v));
      $volFill.style.width = volume + '%';
      $volValue.textContent = String(volume);
      // Apply to all remote video elements
      var videos = $grid.querySelectorAll('.m2-tile:not(.m2-tile-self) video');
      for (var i = 0; i < videos.length; i++) {
        videos[i].volume = volume / 100;
      }
    }

    $volTrack.addEventListener('pointerdown', function (e) {
      var drag = function (ev) {
        var r = $volTrack.getBoundingClientRect();
        var x = Math.max(0, Math.min(r.width, ev.clientX - r.left));
        setVolume(Math.round((x / r.width) * 100));
      };
      drag(e);
      var move = function (ev) { drag(ev); };
      var up = function () {
        window.removeEventListener('pointermove', move);
        window.removeEventListener('pointerup', up);
      };
      window.addEventListener('pointermove', move);
      window.addEventListener('pointerup', up);
    });
    setVolume(80);

    // --- init ---

    getStatus().then(function () {
      pollCallState();
      setInterval(pollCallState, POLL_MS);
      // Auto-rejoin own call after page reload.
      fetchJSON('/api/call/state').then(function (cs) {
        if (cs && cs.sfu_host === myTunnelIP && !inCall) {
          log('rejoining own call...');
          sfuHost = myTunnelIP;
          joinCall();
        }
      }).catch(function () {});
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
