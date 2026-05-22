// audio.js — direct WebRTC audio+video call between two f2f peers.
//
// Signalling rides over the local HTTP server (POST /api/signal/outbox
// forwards across the tunnel; SSE /api/signal/stream delivers inbound).
// ICE uses an empty iceServers list — only host candidates, paired across
// the overlay subnet. Camp's server-side rewrite of mDNS-masked candidates
// keeps this working in stock Chrome/Firefox.

(function () {
  // The dB meter is split in two halves: the first NUM_DB_BARS_HALF
  // columns are the local mic, the last NUM_DB_BARS_HALF are the peer's
  // incoming audio. Each half has its own AnalyserNode in the same
  // shared AudioContext.
  const NUM_DB_BARS_HALF = 32;
  const NUM_DB_BARS = NUM_DB_BARS_HALF * 2;

  function start() {
    // DOM
    const $paneYou    = document.getElementById('ax-pane-you');
    const $panePeer   = document.getElementById('ax-pane-peer');
    const $paneYouScreen  = document.getElementById('ax-pane-you-screen');
    const $panePeerScreen = document.getElementById('ax-pane-peer-screen');
    const $videoYou   = document.getElementById('ax-video-you');
    const $videoPeer  = document.getElementById('ax-video-peer');
    const $videoYouScreen  = document.getElementById('ax-video-you-screen');
    const $videoPeerScreen = document.getElementById('ax-video-peer-screen');
    const $resYou     = document.getElementById('ax-res-you');
    const $resPeer    = document.getElementById('ax-res-peer');
    const $resYouScreen  = document.getElementById('ax-res-you-screen');
    const $resPeerScreen = document.getElementById('ax-res-peer-screen');
    const $youHost    = document.getElementById('ax-you-host');
    const $peerHost   = document.getElementById('ax-peer-host');
    const $youScreenHost  = document.getElementById('ax-you-screen-host');
    const $peerScreenHost = document.getElementById('ax-peer-screen-host');
    const $callBtn    = document.getElementById('ax-call-btn');
    const $callMeta   = document.getElementById('ax-call-meta');
    const $callState  = $callBtn.querySelector('.ax-btn-state');
    const $callLabel  = $callBtn.querySelector('.ax-btn-label');
    const $micBtn     = document.getElementById('ax-mic-btn');
    const $micState   = $micBtn.querySelector('.ax-btn-state');
    const $camBtn     = document.getElementById('ax-cam-btn');
    const $camState   = $camBtn.querySelector('.ax-btn-state');
    const $shareBtn   = document.getElementById('ax-share-btn');
    const $shareState = $shareBtn.querySelector('.ax-btn-state');
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
    let screenStream = null;
    let screenSenders = []; // RTCRtpSenders for screen tracks (so we can removeTrack on stop)
    let isOfferer = false;  // true on the side that originated the call; only this side initiates renegotiation
    let peerScreenStreamId = null; // remembered from incoming screen-share signal
    let callStartedAt = 0;
    let callTimer = 0;
    let logCount = 0;
    let audioCtx = null;
    let localAnalyser = null;
    let peerAnalyser = null;
    let localBuf = null;
    let peerBuf = null;
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
      // Mirror the cam-pane label into the screen-share panes so the
      // user can read who's casting at a glance.
      $youScreenHost.textContent = $youHost.textContent + ' · screen';
      $peerScreenHost.textContent = $peerHost.textContent + ' · screen';

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
        // The sender announces its screen-share stream id via a side
        // signal (kind: "screen-share") before renegotiating, so we
        // know which incoming stream is the desktop and which is cam.
        const isScreen = stream && peerScreenStreamId && stream.id === peerScreenStreamId;
        if (isScreen) {
          if (e.track.kind === 'video') {
            $videoPeerScreen.srcObject = stream;
            $videoPeerScreen.volume = volume / 100;
            $panePeerScreen.classList.remove('hidden');
            $panePeerScreen.classList.add('has-video');
            $videoPeerScreen.addEventListener('loadedmetadata', () => {
              $resPeerScreen.textContent = `${$videoPeerScreen.videoHeight}p · ${Math.round($videoPeerScreen.getVideoPlaybackQuality?.().droppedVideoFrames || 30)}fps`;
            }, { once: true });
            e.track.addEventListener('ended', hidePeerScreen);
            e.track.addEventListener('mute',  hidePeerScreen);
            e.track.addEventListener('unmute', () => $panePeerScreen.classList.add('has-video'));
          } else if (e.track.kind === 'audio') {
            // System audio bundled with screen — play through screen pane.
            if (!$videoPeerScreen.srcObject) $videoPeerScreen.srcObject = stream;
            $videoPeerScreen.volume = volume / 100;
            $videoPeerScreen.play().catch(() => {});
          }
        } else if (e.track.kind === 'video') {
          $videoPeer.srcObject = stream;
          // Don't muffle here — in same-stream multi-track WebRTC the
          // same element ends up carrying audio too, and the audio
          // branch below sets the real volume. WebKit was treating
          // volume=0 set here as authoritative and ignoring later
          // writes until the user dragged the slider (the bug already
          // fixed in source/desktop, ported here).
          $videoPeer.muted = false;
          $videoPeer.volume = volume / 100;
          $panePeer.classList.add('has-video');
          e.track.addEventListener('mute',   () => $panePeer.classList.remove('has-video'));
          e.track.addEventListener('unmute', () => $panePeer.classList.add('has-video'));
          $videoPeer.addEventListener('loadedmetadata', () => {
            $resPeer.textContent =
              `${$videoPeer.videoHeight}p · ${Math.round($videoPeer.getVideoPlaybackQuality?.().droppedVideoFrames || 30)}fps`;
          }, { once: true });
        } else if (e.track.kind === 'audio') {
          if (!$videoPeer.srcObject) $videoPeer.srcObject = stream;
          $videoPeer.volume = volume / 100;
          $videoPeer.play().catch(() => {});
          // Visualise peer's audio in the right half of the meter.
          if (!peerAnalyser) setupDbMeterPeer(new MediaStream([e.track]));
        }
        logLine('event', 'remote ' + e.track.kind + ' track attached (' + (isScreen ? 'screen' : 'cam') + ')');
      };
      conn.onnegotiationneeded = async () => {
        // Only the originating offerer renegotiates — keeps things glare-free
        // in a 1-on-1 call. If the answerer ever wants to add a track they
        // also call scheduleRenegotiation, which will run only on the offerer.
        if (!isOfferer) return;
        try {
          const offer = await conn.createOffer();
          if (conn.signalingState !== 'stable') return; // racing with another negotiation
          await conn.setLocalDescription(offer);
          await sendSignal({ kind: 'offer', sdp: offer.sdp });
          logLine('event', 'sent renegotiation offer');
        } catch (err) {
          logLine('event', 'renegotiation failed: ' + err.message);
        }
      };

      function hidePeerScreen() {
        $videoPeerScreen.srcObject = null;
        $panePeerScreen.classList.add('hidden');
        $panePeerScreen.classList.remove('has-video');
        $resPeerScreen.textContent = '—';
      }
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
      if (micEnabled) setupDbMeterLocal(localStream);
      updateMicCamUI();
      const parts = [micEnabled && 'mic', camEnabled && 'cam'].filter(Boolean).join('+');
      logLine('event', parts + ' acquired');
      return localStream;
    }

    function ensureAudioCtx() {
      if (audioCtx) {
        if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
        return audioCtx;
      }
      const AudioCtxCtor = window.AudioContext || /** @type {*} */(window).webkitAudioContext;
      audioCtx = new AudioCtxCtor();
      if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
      return audioCtx;
    }

    function attachAnalyser(stream) {
      const ctx = ensureAudioCtx();
      const source = ctx.createMediaStreamSource(stream);
      const a = ctx.createAnalyser();
      a.fftSize = 1024;
      a.smoothingTimeConstant = 0.6;
      source.connect(a);
      return { analyser: a, buf: new Uint8Array(a.frequencyBinCount) };
    }

    function setupDbMeterLocal(stream) {
      const { analyser, buf } = attachAnalyser(stream);
      localAnalyser = analyser;
      localBuf = buf;
      startDbTick();
    }

    function setupDbMeterPeer(stream) {
      const { analyser, buf } = attachAnalyser(stream);
      peerAnalyser = analyser;
      peerBuf = buf;
      startDbTick();
    }

    function startDbTick() {
      if (dbRAF) return;
      const tick = () => {
        fillHalf(0,                   localAnalyser, localBuf);
        fillHalf(NUM_DB_BARS_HALF,    peerAnalyser,  peerBuf);
        dbRAF = requestAnimationFrame(tick);
      };
      tick();
    }

    function fillHalf(offset, analyser, buf) {
      if (!analyser || !buf) {
        for (let i = 0; i < NUM_DB_BARS_HALF; i++) {
          const bar = dbBarEls[offset + i];
          bar.style.height = '6%';
          bar.classList.remove('live', 'warm', 'hot');
        }
        return;
      }
      analyser.getByteFrequencyData(buf);
      // Slice the spectrum into NUM_DB_BARS_HALF log-spaced buckets for a more
      // pleasing visual than evenly-spaced bins.
      for (let i = 0; i < NUM_DB_BARS_HALF; i++) {
        const lo = Math.floor(Math.pow(i       / NUM_DB_BARS_HALF, 2.0) * buf.length);
        const hi = Math.floor(Math.pow((i + 1) / NUM_DB_BARS_HALF, 2.0) * buf.length);
        let max = 0;
        for (let j = lo; j <= hi && j < buf.length; j++) {
          if (buf[j] > max) max = buf[j];
        }
        const norm = max / 255;
        const h = 6 + norm * 94; // 6%..100%
        const bar = dbBarEls[offset + i];
        bar.style.height = h.toFixed(0) + '%';
        bar.classList.remove('live', 'warm', 'hot');
        if (norm > 0.85)      bar.classList.add('hot');
        else if (norm > 0.55) bar.classList.add('warm');
        else if (norm > 0.04) bar.classList.add('live');
      }
    }

    function stopDbMeter() {
      if (dbRAF) cancelAnimationFrame(dbRAF), dbRAF = 0;
      if (audioCtx) { audioCtx.close().catch(() => {}); audioCtx = null; }
      localAnalyser = null;
      peerAnalyser = null;
      localBuf = null;
      peerBuf = null;
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
        $shareBtn.disabled = true;
        stopCallTimer();
      } else if (s === 'connecting') {
        $callBtn.classList.add('state-connecting');
        $callState.textContent = '●';
        $callLabel.textContent = 'connecting…';
        $callMeta.textContent = '';
        $micBtn.disabled = !localStream;
        $camBtn.disabled = !localStream;
        $shareBtn.disabled = true;
      } else if (s === 'connected') {
        $callBtn.classList.add('state-connected');
        $callState.textContent = '■';
        $callLabel.textContent = 'hang up';
        $paneYou.classList.add('active');
        $panePeer.classList.add('active');
        $micBtn.disabled = false;
        $camBtn.disabled = false;
        $shareBtn.disabled = false;
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
        isOfferer = true;
        // Caller side creates the data channel; the answerer picks it up
        // via pc.ondatachannel. Must happen before createOffer.
        attachDataChannel(pc.createDataChannel('chat', { ordered: true }));
        if (localStream) localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
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
      if (screenStream) {
        screenStream.getTracks().forEach((t) => t.stop());
        screenStream = null;
      }
      screenSenders = [];
      peerScreenStreamId = null;
      isOfferer = false;
      stopDbMeter();
      $videoYou.srcObject = null;
      $videoPeer.srcObject = null;
      $videoYouScreen.srcObject = null;
      $videoPeerScreen.srcObject = null;
      $paneYou.classList.remove('has-video', 'active');
      $panePeer.classList.remove('has-video', 'active');
      $paneYouScreen.classList.add('hidden');
      $paneYouScreen.classList.remove('has-video');
      $panePeerScreen.classList.add('hidden');
      $panePeerScreen.classList.remove('has-video');
      $resYou.textContent = '—';
      $resPeer.textContent = '—';
      $resYouScreen.textContent = '—';
      $resPeerScreen.textContent = '—';
      $shareState.textContent = '▢';
      $shareBtn.classList.remove('active');
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
        if (pc && pc.signalingState !== 'closed') {
          // Renegotiation from an existing call (peer added/removed a
          // track, e.g. started/stopped screen share). Don't teardown —
          // re-answer in place.
          try {
            await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            await sendSignal({ kind: 'answer', sdp: answer.sdp });
            logLine('event', 'sent renegotiation answer');
          } catch (err) {
            logLine('event', 'renegotiation answer failed: ' + err.message);
          }
          return;
        }
        logLine('event', 'received offer');
        setState('connecting');
        await ensureLocalStream();
        pc = newPC();
        isOfferer = false;
        if (localStream) localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
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
      } else if (msg.kind === 'screen-share') {
        // Peer announces its screen MediaStream id. Stash it so the next
        // ontrack with that stream id gets routed to the screen pane.
        // on=false clears the mapping and hides the pane on the next
        // track.ended event.
        peerScreenStreamId = msg.on ? msg.streamId : null;
        logLine('event', 'peer screen share ' + (msg.on ? 'on (' + msg.streamId + ')' : 'off'));
      } else if (msg.kind === 'hangup') {
        logLine('event', 'peer hung up');
        teardown();
      }
    }

    // ---- controls ----
    // primeAudio unlocks autoplay for the rest of this page lifetime:
    // browsers gate <audio>/<video>.play() with non-zero volume on a
    // recent user gesture. The first call() click is that gesture; by
    // the time ontrack fires (several async hops later) the gesture is
    // stale and audio plays silently until the user touches something
    // (like the volume slider — exactly what was reported). Kicking off
    // a no-op AudioContext + a 1-frame silent <audio> here flips the
    // page into "user has allowed audio" mode permanently.
    let audioPrimed = false;
    function primeAudio() {
      if (audioPrimed) return;
      audioPrimed = true;
      try {
        const C = window.AudioContext || /** @type {*} */(window).webkitAudioContext;
        const ctx = new C();
        if (ctx.state === 'suspended') ctx.resume().catch(() => {});
        const buf = ctx.createBuffer(1, 1, 22050);
        const src = ctx.createBufferSource();
        src.buffer = buf;
        src.connect(ctx.destination);
        src.start(0);
      } catch (_) { /* no audio support, give up silently */ }
    }
    $callBtn.addEventListener('click', () => {
      primeAudio();
      pc ? hangup() : call();
    });
    // Answerers never click the call button — incoming offers connect
    // automatically — so we also prime on every user gesture and on
    // each one also nudge the actual <video> elements: WebKit autoplay
    // gating remembers user intent per-element, not just page-wide, so
    // .play() called from inside a user-gesture handler is what
    // unmutes the stream. Without this the audio sat at volume/100 but
    // played silently until the user wiggled the slider.
    function unlockMedia() {
      primeAudio();
      if ($videoPeer.srcObject)       { $videoPeer.muted = false;       $videoPeer.play().catch(() => {}); }
      if ($videoPeerScreen.srcObject) { $videoPeerScreen.muted = false; $videoPeerScreen.play().catch(() => {}); }
    }
    document.addEventListener('pointerdown', unlockMedia, { capture: true });
    document.addEventListener('keydown', unlockMedia, { capture: true });
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

    $shareBtn.addEventListener('click', () => {
      if (!pc) return;
      if (screenStream) stopScreenShare();
      else startScreenShare();
    });

    async function startScreenShare() {
      if (!pc || screenStream) return;
      if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia) {
        alert('getDisplayMedia not supported by this browser.');
        return;
      }
      let stream;
      try {
        stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true });
      } catch (err) {
        if (err.name !== 'NotAllowedError') logLine('event', 'getDisplayMedia: ' + err.message);
        return;
      }
      screenStream = stream;
      // Tell the peer "this stream id is screen" BEFORE renegotiation so
      // they can tag the incoming track when it arrives.
      await sendSignal({ kind: 'screen-share', on: true, streamId: stream.id });
      stream.getTracks().forEach((t) => {
        screenSenders.push(pc.addTrack(t, stream));
        t.addEventListener('ended', stopScreenShare);
      });
      // Local preview.
      $videoYouScreen.srcObject = stream;
      $paneYouScreen.classList.remove('hidden');
      $paneYouScreen.classList.add('has-video');
      $videoYouScreen.addEventListener('loadedmetadata', () => {
        $resYouScreen.textContent = `${$videoYouScreen.videoHeight}p`;
      }, { once: true });
      $shareState.textContent = '■';
      $shareBtn.classList.add('active');
      logLine('event', 'screen share started (' + stream.id + ')');
      // addTrack triggers onnegotiationneeded on the offerer. On the
      // answerer we'd be stuck without an offer route — so for now if a
      // non-offerer starts share, we send an immediate offer manually.
      if (!isOfferer) await forceRenegotiate();
    }

    async function stopScreenShare() {
      if (!screenStream) return;
      const stream = screenStream;
      screenStream = null;
      stream.getTracks().forEach((t) => t.stop());
      if (pc) {
        screenSenders.forEach((s) => { try { pc.removeTrack(s); } catch (_) {} });
      }
      screenSenders = [];
      $videoYouScreen.srcObject = null;
      $paneYouScreen.classList.add('hidden');
      $paneYouScreen.classList.remove('has-video');
      $resYouScreen.textContent = '—';
      $shareState.textContent = '▢';
      $shareBtn.classList.remove('active');
      await sendSignal({ kind: 'screen-share', on: false });
      logLine('event', 'screen share stopped');
      if (!isOfferer && pc) await forceRenegotiate();
    }

    // forceRenegotiate is the answerer's manual offer path — the
    // onnegotiationneeded handler bails on non-offerers to avoid glare,
    // but mid-call add/removeTrack from the answerer side still needs to
    // get an offer out. We just promote ourselves to offerer for the
    // remainder of the call.
    async function forceRenegotiate() {
      if (!pc) return;
      try {
        const offer = await pc.createOffer();
        await pc.setLocalDescription(offer);
        await sendSignal({ kind: 'offer', sdp: offer.sdp });
        isOfferer = true;
        logLine('event', 'sent renegotiation offer (promoted to offerer)');
      } catch (err) {
        logLine('event', 'forceRenegotiate failed: ' + err.message);
      }
    }

    // macOS mice without a trackpad don't translate wheel-vertical into
    // horizontal scroll on overflow-x containers. Forward deltaY → scrollLeft
    // when the user is hovering the panes row, unless shift is held (in
    // which case the browser already does horizontal scroll).
    const $panes = document.querySelector('#tab-audio .ax-panes');
    if ($panes) {
      $panes.addEventListener('wheel', (e) => {
        if (e.shiftKey) return;
        const dy = e.deltaY;
        if (Math.abs(dy) <= Math.abs(e.deltaX)) return;
        $panes.scrollLeft += dy;
        e.preventDefault();
      }, { passive: false });
    }

    // Fullscreen icons on each pane. Click → request fullscreen on the
    // associated <video>.
    document.querySelectorAll('.ax-pane-fs').forEach((btn) => {
      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        const target = document.getElementById(btn.getAttribute('data-fs'));
        if (!target || !target.requestFullscreen) return;
        target.requestFullscreen().catch(() => {});
      });
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

    setState('idle');
    startSignaling();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
