// audio.js — direct WebRTC audio+video call between two f2f peers.
//
// Signalling rides over the local HTTP server (POST /api/signal/outbox
// forwards across the tunnel; SSE /api/signal/stream delivers inbound).
// ICE uses an empty iceServers list — only host candidates, paired across
// the overlay subnet. Camp's server-side rewrite of mDNS-masked candidates
// keeps this working in stock Chrome/Firefox.

(function () {
  const NUM_DB_BARS = 32;

  function start() {
    // DOM
    const $paneYou    = document.getElementById('ax-pane-you');
    const $panePeer   = document.getElementById('ax-pane-peer');
    const $videoYou   = document.getElementById('ax-video-you');
    const $videoPeer  = document.getElementById('ax-video-peer');
    const $resYou     = document.getElementById('ax-res-you');
    const $resPeer    = document.getElementById('ax-res-peer');
    const $youHost    = document.getElementById('ax-you-host');
    const $peerHost   = document.getElementById('ax-peer-host');
    const $callBtn    = document.getElementById('ax-call-btn');
    const $callMeta   = document.getElementById('ax-call-meta');
    const $callState  = $callBtn.querySelector('.ax-btn-state');
    const $callLabel  = $callBtn.querySelector('.ax-btn-label');
    const $micBtn     = document.getElementById('ax-mic-btn');
    const $micState   = $micBtn.querySelector('.ax-btn-state');
    const $camBtn     = document.getElementById('ax-cam-btn');
    const $camState   = $camBtn.querySelector('.ax-btn-state');
    const $volTrack   = document.getElementById('ax-vol-track');
    const $volFill    = document.getElementById('ax-vol-fill');
    const $volValue   = document.getElementById('ax-vol-value');
    const $dbBars     = document.getElementById('ax-db-bars');
    const $logBody    = document.getElementById('ax-log-body');
    const $logCount   = document.getElementById('ax-log-count');
    const $logClear   = document.getElementById('ax-log-clear');
    const $chatInput  = document.getElementById('ax-chat-input');

    // Build the dB meter bars once.
    const dbBarEls = [];
    for (let i = 0; i < NUM_DB_BARS; i++) {
      const b = document.createElement('div');
      b.className = 'ax-db-bar';
      $dbBars.appendChild(b);
      dbBarEls.push(b);
    }

    // State
    let pc = null;
    let dataChannel = null;
    let localStream = null;
    let signalES = null;
    let pendingRemoteCandidates = [];
    let micEnabled = false;
    let camEnabled = false;
    let callStartedAt = 0;
    let callTimer = 0;
    let logCount = 0;
    let audioCtx = null;
    let analyser = null;
    let dbBuf = null;
    let dbRAF = 0;
    let volume = 80;
    let myName = 'you';
    let peerName = 'peer';

    // Pane labels reflect identity, not raw addresses. When camp is up
    // we show "<name> @ <tunnel_ip>" for both you and peer; otherwise we
    // fall back to whatever the engine tells us in plain.
    // Also: if the engine transitions from running→stopped while we have
    // a live PC, drop the call — without this the WebRTC pipe lingers
    // (~30s ICE timeout) since the browser doesn't know its transport
    // just disappeared.
    let lastRunning = null;
    function applyIdentity(s) {
      if (s && s.camp_active) {
        myName = s.camp_name || 'you';
        peerName = s.camp_peer_name || 'peer';
        $youHost.textContent = myName + ' @ ' + (s.local_ip || '?');
        if (s.camp_peer_name && s.peer_ip) {
          $peerHost.textContent = peerName + ' @ ' + s.peer_ip;
        } else {
          $peerHost.textContent = 'waiting…';
        }
      } else if (s && s.running) {
        myName = 'you';
        peerName = 'peer';
        $youHost.textContent = 'you @ ' + (s.local_ip || 'localhost');
        $peerHost.textContent = 'peer @ ' + (s.peer_ip || s.peer_addr || '—');
      } else {
        myName = 'you';
        peerName = 'peer';
        $youHost.textContent = 'you @ ' + (location.hostname || 'localhost');
        $peerHost.textContent = 'peer @ —';
      }

      const running = !!(s && s.running);
      if (lastRunning === true && !running && pc) {
        logLine('event', 'engine stopped, hanging up call');
        teardown();
      }
      lastRunning = running;
    }
    function pollStatus() {
      fetch('/api/status').then((r) => r.json()).then(applyIdentity).catch(() => {});
    }
    pollStatus();
    setInterval(pollStatus, 3000);

    // ---- logging ----
    function timestamp() {
      const t = new Date();
      return t.toTimeString().slice(0, 8) + '.' +
        String(t.getMilliseconds()).padStart(3, '0');
    }
    function appendRow(row) {
      $logBody.appendChild(row);
      $logBody.scrollTop = $logBody.scrollHeight;
      logCount++;
      $logCount.textContent = String(logCount);
    }
    function logLine(kind, msg) {
      const row = document.createElement('div');
      row.className = 'ax-log-event';
      const c1 = document.createElement('span');
      c1.className = 'ax-log-time';
      c1.textContent = timestamp();
      const c2 = document.createElement('span');
      c2.className = 'ax-log-icon' + (kind === 'event' ? ' event' : '');
      c2.textContent = kind === 'event' ? '›' : '·';
      const c3 = document.createElement('span');
      c3.className = 'ax-log-msg';
      c3.textContent = msg;
      row.append(c1, c2, c3);
      appendRow(row);
    }
    // Chat lines share the same body as system events, distinguished by
    // the second column carrying "<who>" instead of a bullet/arrow icon.
    function logChat(who, msg) {
      const row = document.createElement('div');
      row.className = 'ax-log-event is-chat' + (who === myName ? ' from-you' : '');
      const c1 = document.createElement('span');
      c1.className = 'ax-log-time';
      c1.textContent = timestamp();
      const c2 = document.createElement('span');
      c2.className = 'ax-log-icon';
      c2.textContent = '<' + who + '>';
      const c3 = document.createElement('span');
      c3.className = 'ax-log-msg';
      c3.textContent = msg;
      row.append(c1, c2, c3);
      appendRow(row);
    }
    $logClear.addEventListener('click', () => {
      $logBody.replaceChildren();
      logCount = 0;
      $logCount.textContent = '0';
    });

    $chatInput.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter') return;
      e.preventDefault();
      const text = $chatInput.value.trim();
      if (!text) return;
      if (sendChat(text)) $chatInput.value = '';
    });

    // ---- signalling ----
    function startSignaling() {
      if (signalES) return;
      signalES = new EventSource('/api/signal/stream');
      signalES.onopen = () => logLine('dot', 'signal stream open');
      signalES.onmessage = async (e) => {
        try { await handleSignal(JSON.parse(e.data)); }
        catch (err) { logLine('event', 'signal error: ' + err.message); }
      };
      signalES.onerror = () => logLine('dot', 'signal stream error');
    }
    async function sendSignal(msg) {
      try {
        const r = await fetch('/api/signal/outbox', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(msg),
        });
        if (!r.ok) logLine('event', 'outbox ' + r.status + ': ' + (await r.text()));
      } catch (err) {
        logLine('event', 'outbox error: ' + err.message);
      }
    }

    // ---- chat data channel ----
    function attachDataChannel(channel) {
      dataChannel = channel;
      channel.onopen = () => {
        $chatInput.disabled = false;
        $chatInput.placeholder = 'type a message…';
        $chatInput.focus();
        logLine('event', 'chat channel open');
      };
      channel.onclose = () => {
        $chatInput.disabled = true;
        $chatInput.placeholder = 'connect to chat';
      };
      channel.onmessage = (e) => {
        const text = String(e.data || '').slice(0, 4096).trim();
        if (text) logChat(peerName, text);
      };
    }
    function sendChat(text) {
      if (!dataChannel || dataChannel.readyState !== 'open') return false;
      try { dataChannel.send(text); }
      catch (err) { logLine('event', 'chat send failed: ' + err.message); return false; }
      logChat(myName, text);
      return true;
    }

    // ---- peer connection ----
    function newPC() {
      const conn = new RTCPeerConnection({ iceServers: [] });
      // Answerer receives the channel via this event; the caller creates
      // it directly with createDataChannel() before generating the offer.
      conn.ondatachannel = (e) => attachDataChannel(e.channel);
      conn.ontrack = (e) => {
        const stream = e.streams[0];
        if (e.track.kind === 'video') {
          $videoPeer.srcObject = stream;
          $videoPeer.volume = 0;
          $panePeer.classList.add('has-video');
          e.track.addEventListener('mute',   () => $panePeer.classList.remove('has-video'));
          e.track.addEventListener('unmute', () => $panePeer.classList.add('has-video'));
          // Resolution badge updates as soon as metadata arrives.
          $videoPeer.addEventListener('loadedmetadata', () => {
            $resPeer.textContent =
              `${$videoPeer.videoHeight}p · ${Math.round($videoPeer.getVideoPlaybackQuality?.().droppedVideoFrames || 30)}fps`;
          }, { once: true });
        } else if (e.track.kind === 'audio') {
          // Audio playback goes through the peer <video> element's audio
          // track so volume is unified with video. Volume reflects slider.
          if (!$videoPeer.srcObject) $videoPeer.srcObject = stream;
          $videoPeer.volume = volume / 100;
          $videoPeer.play().catch(() => {});
        }
        logLine('event', 'remote ' + e.track.kind + ' track attached');
      };
      conn.onicecandidate = (e) => {
        if (e.candidate) sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
        else logLine('event', 'ICE gathering complete');
      };
      conn.oniceconnectionstatechange = () => {
        const st = conn.iceConnectionState;
        logLine('event', 'ICE state: ' + st);
        if (st === 'connected' || st === 'completed') {
          setState('connected');
        } else if (st === 'failed' || st === 'closed') {
          teardown();
        } else if (st === 'disconnected') {
          // Often transient. Don't tear down; let WebRTC try to recover.
        }
      };
      conn.onconnectionstatechange = () => logLine('dot', 'PC state: ' + conn.connectionState);
      return conn;
    }

    // ---- media ----
    // Returns the acquired MediaStream, or null when neither mic nor cam
    // is available. Null is a valid call mode — the user becomes a
    // receive-only peer (no local tracks, but they can still see/hear the
    // remote and use chat via the data channel).
    async function ensureLocalStream() {
      if (localStream) return localStream;
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        logLine('event', 'no getUserMedia (insecure origin?) — receive-only');
        return null;
      }

      const audioConstraint = {
        echoCancellation: true, noiseSuppression: true, autoGainControl: true,
      };
      const videoConstraint = { width: { ideal: 640 }, height: { ideal: 480 } };

      const tryGUM = async (c) => {
        try { return await navigator.mediaDevices.getUserMedia(c); }
        catch (err) {
          logLine('event', 'getUserMedia(' + Object.keys(c).join('+') + ') failed: ' + err.name);
          return null;
        }
      };

      // Cascade through what the user actually has: mic+cam → mic → cam.
      // Anything that succeeds is fine, even cam-only (you'll receive
      // peer audio but not transmit your own). All-missing → receive-only.
      localStream =
        (await tryGUM({ audio: audioConstraint, video: videoConstraint })) ||
        (await tryGUM({ audio: audioConstraint })) ||
        (await tryGUM({ video: videoConstraint }));

      if (!localStream) {
        logLine('event', 'no mic or cam — call continues in receive-only mode');
        micEnabled = false;
        camEnabled = false;
        $paneYou.classList.remove('has-video');
        updateMicCamUI();
        return null;
      }

      micEnabled = localStream.getAudioTracks().length > 0;
      camEnabled = localStream.getVideoTracks().length > 0;

      $videoYou.srcObject = localStream;
      if (camEnabled) {
        $paneYou.classList.add('has-video');
        const vt = localStream.getVideoTracks()[0];
        if (vt) {
          const s = vt.getSettings();
          $resYou.textContent = (s.height || 480) + 'p · ' + (s.frameRate || 30) + 'fps';
        }
      } else {
        $paneYou.classList.remove('has-video');
      }
      if (micEnabled) setupDbMeter(localStream);
      updateMicCamUI();
      const parts = [micEnabled && 'mic', camEnabled && 'cam'].filter(Boolean).join('+');
      logLine('event', parts + ' acquired');
      return localStream;
    }

    function setupDbMeter(stream) {
      stopDbMeter();
      const AudioCtxCtor = window.AudioContext || /** @type {*} */(window).webkitAudioContext;
      audioCtx = new AudioCtxCtor();
      const source = audioCtx.createMediaStreamSource(stream);
      analyser = audioCtx.createAnalyser();
      analyser.fftSize = 1024;
      analyser.smoothingTimeConstant = 0.6;
      source.connect(analyser);
      dbBuf = new Uint8Array(analyser.frequencyBinCount);
      const tick = () => {
        if (!analyser) return;
        analyser.getByteFrequencyData(dbBuf);
        // Slice the spectrum into NUM_DB_BARS log-spaced buckets for a more
        // pleasing visual than evenly-spaced bins.
        for (let i = 0; i < NUM_DB_BARS; i++) {
          const lo = Math.floor(Math.pow(i      / NUM_DB_BARS, 2.0) * dbBuf.length);
          const hi = Math.floor(Math.pow((i + 1) / NUM_DB_BARS, 2.0) * dbBuf.length);
          let max = 0;
          for (let j = lo; j <= hi && j < dbBuf.length; j++) {
            if (dbBuf[j] > max) max = dbBuf[j];
          }
          const norm = max / 255;
          const h = 6 + norm * 94; // 6%..100%
          const bar = dbBarEls[i];
          bar.style.height = h.toFixed(0) + '%';
          bar.classList.remove('live', 'warm', 'hot');
          if (norm > 0.85)      bar.classList.add('hot');
          else if (norm > 0.55) bar.classList.add('warm');
          else if (norm > 0.04) bar.classList.add('live');
        }
        dbRAF = requestAnimationFrame(tick);
      };
      tick();
    }
    function stopDbMeter() {
      if (dbRAF) cancelAnimationFrame(dbRAF), dbRAF = 0;
      if (audioCtx) { audioCtx.close().catch(() => {}); audioCtx = null; }
      analyser = null;
      dbBuf = null;
      for (const b of dbBarEls) {
        b.style.height = '2px';
        b.classList.remove('live', 'warm', 'hot');
      }
    }

    // ---- state machine ----
    function setState(s) {
      $callBtn.classList.remove('state-connecting', 'state-connected');
      if (s === 'idle') {
        $callState.textContent = '▶';
        $callLabel.textContent = 'call peer';
        $callMeta.textContent = '';
        $paneYou.classList.remove('active');
        $panePeer.classList.remove('active');
        $micBtn.disabled = true;
        $camBtn.disabled = true;
        stopCallTimer();
      } else if (s === 'connecting') {
        $callBtn.classList.add('state-connecting');
        $callState.textContent = '●';
        $callLabel.textContent = 'connecting…';
        $callMeta.textContent = '';
        $micBtn.disabled = !localStream;
        $camBtn.disabled = !localStream;
      } else if (s === 'connected') {
        $callBtn.classList.add('state-connected');
        $callState.textContent = '■';
        $callLabel.textContent = 'hang up';
        $paneYou.classList.add('active');
        $panePeer.classList.add('active');
        $micBtn.disabled = false;
        $camBtn.disabled = false;
        startCallTimer();
      }
    }

    function startCallTimer() {
      callStartedAt = Date.now();
      if (callTimer) clearInterval(callTimer);
      callTimer = setInterval(() => {
        const s = Math.floor((Date.now() - callStartedAt) / 1000);
        const mm = String(Math.floor(s / 60)).padStart(2, '0');
        const ss = String(s % 60).padStart(2, '0');
        $callMeta.textContent = mm + ':' + ss;
      }, 1000);
    }
    function stopCallTimer() {
      if (callTimer) clearInterval(callTimer), callTimer = 0;
      callStartedAt = 0;
    }

    function updateMicCamUI() {
      const hasMic = !!localStream && localStream.getAudioTracks().length > 0;
      const hasCam = !!localStream && localStream.getVideoTracks().length > 0;
      $micBtn.classList.toggle('active', micEnabled);
      $camBtn.classList.toggle('active', camEnabled);
      $micState.textContent = micEnabled ? '●' : '○';
      $camState.textContent = camEnabled ? '■' : '□';
      // Buttons stay disabled when the underlying track doesn't exist at
      // all (no device on this machine), regardless of call state.
      $micBtn.disabled = !hasMic;
      $camBtn.disabled = !hasCam;
      $paneYou.classList.toggle('has-video', camEnabled && hasCam);
    }

    // ---- call lifecycle ----
    async function call() {
      try {
        setState('connecting');
        await ensureLocalStream();
        pc = newPC();
        // Caller side creates the data channel; the answerer picks it up
        // via pc.ondatachannel. Must happen before createOffer.
        attachDataChannel(pc.createDataChannel('chat', { ordered: true }));
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        const offer = await pc.createOffer({ offerToReceiveAudio: true, offerToReceiveVideo: true });
        await pc.setLocalDescription(offer);
        await sendSignal({ kind: 'offer', sdp: offer.sdp });
        logLine('event', 'sent offer');
      } catch (err) {
        logLine('event', 'call error: ' + err.message);
        teardown();
      }
    }
    async function hangup() {
      await sendSignal({ kind: 'hangup' });
      teardown();
    }
    function teardown() {
      if (dataChannel) { try { dataChannel.close(); } catch (_) {} dataChannel = null; }
      if (pc) { try { pc.close(); } catch (_) {} pc = null; }
      if (localStream) {
        localStream.getTracks().forEach((t) => t.stop());
        localStream = null;
      }
      stopDbMeter();
      $videoYou.srcObject = null;
      $videoPeer.srcObject = null;
      $paneYou.classList.remove('has-video', 'active');
      $panePeer.classList.remove('has-video', 'active');
      $resYou.textContent = '—';
      $resPeer.textContent = '—';
      $chatInput.disabled = true;
      $chatInput.placeholder = 'connect to chat';
      $chatInput.value = '';
      pendingRemoteCandidates = [];
      micEnabled = false;
      camEnabled = false;
      updateMicCamUI();
      setState('idle');
      logLine('dot', 'teardown');
    }

    async function handleSignal(msg) {
      if (msg.kind === 'offer') {
        logLine('event', 'received offer');
        if (pc) teardown();
        setState('connecting');
        await ensureLocalStream();
        pc = newPC();
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        for (const c of pendingRemoteCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
        pendingRemoteCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        logLine('event', 'sent answer');
      } else if (msg.kind === 'answer') {
        logLine('event', 'received answer');
        if (!pc) return;
        await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
        for (const c of pendingRemoteCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
        pendingRemoteCandidates = [];
      } else if (msg.kind === 'candidate') {
        if (!pc || !pc.remoteDescription) { pendingRemoteCandidates.push(msg.candidate); return; }
        try { await pc.addIceCandidate(msg.candidate); }
        catch (err) { logLine('event', 'addIceCandidate failed: ' + err.message); }
      } else if (msg.kind === 'hangup') {
        logLine('event', 'peer hung up');
        teardown();
      }
    }

    // ---- controls ----
    $callBtn.addEventListener('click', () => { pc ? hangup() : call(); });
    $micBtn.addEventListener('click', () => {
      if (!localStream) return;
      micEnabled = !micEnabled;
      localStream.getAudioTracks().forEach((t) => { t.enabled = micEnabled; });
      updateMicCamUI();
    });
    $camBtn.addEventListener('click', () => {
      if (!localStream) return;
      camEnabled = !camEnabled;
      localStream.getVideoTracks().forEach((t) => { t.enabled = camEnabled; });
      updateMicCamUI();
    });

    function setVolume(v) {
      volume = Math.max(0, Math.min(100, v));
      $volFill.style.width = volume + '%';
      $volValue.textContent = String(volume);
      $videoPeer.volume = volume / 100;
    }
    $volTrack.addEventListener('pointerdown', (e) => {
      const drag = (ev) => {
        const r = $volTrack.getBoundingClientRect();
        const x = Math.max(0, Math.min(r.width, ev.clientX - r.left));
        setVolume(Math.round((x / r.width) * 100));
      };
      drag(e);
      const move = (ev) => drag(ev);
      const up = () => {
        window.removeEventListener('pointermove', move);
        window.removeEventListener('pointerup', up);
      };
      window.addEventListener('pointermove', move);
      window.addEventListener('pointerup', up);
    });
    setVolume(80);

    // ---- keyboard shortcuts ----
    // Active only when the audio tab is visible and the user isn't typing
    // into a form field.
    function audioTabActive() {
      return !document.getElementById('tab-audio').classList.contains('hidden');
    }
    function inField(e) {
      const t = e.target;
      return t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
    }
    document.addEventListener('keydown', (e) => {
      if (!audioTabActive() || inField(e)) return;
      if (e.key === 'Enter' && !pc) { e.preventDefault(); call(); return; }
      if (e.key === 'm') { e.preventDefault(); $micBtn.click(); return; }
      if (e.key === 'v') { e.preventDefault(); $camBtn.click(); return; }
      if (e.key === 'c') { e.preventDefault(); $logClear.click(); return; }
      if ((e.metaKey || e.ctrlKey) && e.key === '.') { e.preventDefault(); if (pc) hangup(); return; }
    });

    setState('idle');
    startSignaling();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
