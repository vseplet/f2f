// audio.js — direct WebRTC audio call between two f2f peers.
//
// Wire: signalling messages travel as JSON blobs through our local HTTP
// server (POST /api/signal/outbox) which forwards them over the tunnel to
// the peer's /api/signal/inbox. The browser subscribes to /api/signal/stream
// to receive inbound signals.
//
// ICE: we configure RTCPeerConnection with `iceServers: []`, so only host
// candidates are gathered. Since each side has the other side's tunnel IP
// reachable through utun, ICE pairs `10.99.0.1 ↔ 10.99.0.2` directly and
// no STUN/TURN is involved.

(function () {
  function start() {
    const $toggle = $('#audio-toggle-btn');
    const $controls = $('#audio-controls');
    const $micBtn = $('#audio-mic-btn');
    const $volume = $('#audio-volume');
    const $volumeLabel = $('#audio-volume-label');
    const $status = $('#audio-status');
    const $log = $('#audio-log');
    const $clearLog = $('#audio-clear-log');
    const $remote = $('#audio-remote');

    let pc = null;
    let localStream = null;
    let signalES = null;
    let pendingRemoteCandidates = [];
    let micEnabled = true;

    function setStatus(s) { $status.text(s); }
    function logLine(s) {
      const at = new Date().toISOString().slice(11, 23);
      const div = document.createElement('div');
      div.textContent = at + '  ' + s;
      $log[0].appendChild(div);
      $log[0].scrollTop = $log[0].scrollHeight;
    }

    // UI state is implicit: pc !== null means "in a call". Render reflects
    // it; each control sits idle until in-call.
    function render(state) {
      if (state === 'idle') {
        $toggle
          .removeClass('bg-rose-600 hover:bg-rose-700')
          .addClass('bg-emerald-600 hover:bg-emerald-700')
          .text('Call peer');
        $controls.addClass('hidden');
        micEnabled = true;
        $micBtn.text('Mute mic').removeClass('bg-rose-200 hover:bg-rose-300').addClass('bg-gray-200 hover:bg-gray-300');
      } else {
        $toggle
          .removeClass('bg-emerald-600 hover:bg-emerald-700')
          .addClass('bg-rose-600 hover:bg-rose-700')
          .text('Hang up');
        $controls.removeClass('hidden');
      }
    }

    function startSignaling() {
      if (signalES) return;
      signalES = new EventSource('/api/signal/stream');
      signalES.onopen = () => logLine('signal stream open');
      signalES.onmessage = async (e) => {
        try {
          await handleSignal(JSON.parse(e.data));
        } catch (err) {
          logLine('signal error: ' + err.message);
        }
      };
      signalES.onerror = () => {
        logLine('signal stream error (auto-reconnect)');
      };
    }

    async function sendSignal(msg) {
      try {
        const resp = await fetch('/api/signal/outbox', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(msg),
        });
        if (!resp.ok) {
          const text = await resp.text();
          logLine('outbox failed (' + resp.status + '): ' + text);
        }
      } catch (err) {
        logLine('outbox error: ' + err.message);
      }
    }

    function newPC() {
      const conn = new RTCPeerConnection({ iceServers: [] });
      conn.ontrack = (e) => {
        $remote[0].srcObject = e.streams[0];
        $remote[0].volume = parseInt($volume.val(), 10) / 100;
        $remote[0].play().catch(() => {});
        logLine('remote track attached');
      };
      conn.onicecandidate = (e) => {
        if (e.candidate) {
          sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
        } else {
          logLine('ICE gathering complete');
        }
      };
      conn.oniceconnectionstatechange = () => {
        const st = conn.iceConnectionState;
        setStatus(st);
        logLine('ICE state: ' + st);
        if (st === 'failed' || st === 'closed') {
          teardown();
        }
      };
      conn.onconnectionstatechange = () => {
        logLine('PC state: ' + conn.connectionState);
      };
      return conn;
    }

    async function ensureLocalStream() {
      if (localStream) return localStream;
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        throw new Error(
          'Mic access requires a secure origin. You are on ' + location.origin +
          '. Use http://localhost:' + (location.port || '80') + ' instead.',
        );
      }
      localStream = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        video: false,
      });
      logLine('mic acquired');
      return localStream;
    }

    async function call() {
      try {
        setStatus('connecting');
        render('in-call');
        await ensureLocalStream();
        pc = newPC();
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        const offer = await pc.createOffer({ offerToReceiveAudio: true });
        await pc.setLocalDescription(offer);
        await sendSignal({ kind: 'offer', sdp: offer.sdp });
        logLine('sent offer');
      } catch (err) {
        setStatus('error');
        logLine('call error: ' + err.message);
        teardown();
      }
    }

    async function hangup() {
      await sendSignal({ kind: 'hangup' });
      teardown();
    }

    function teardown() {
      if (pc) {
        try { pc.close(); } catch (_) {}
        pc = null;
      }
      if (localStream) {
        localStream.getTracks().forEach((t) => t.stop());
        localStream = null;
      }
      $remote[0].srcObject = null;
      pendingRemoteCandidates = [];
      setStatus('idle');
      render('idle');
      logLine('teardown');
    }

    async function handleSignal(msg) {
      if (msg.kind === 'offer') {
        logLine('received offer');
        if (pc) teardown();
        setStatus('incoming');
        render('in-call');
        await ensureLocalStream();
        pc = newPC();
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        for (const c of pendingRemoteCandidates) {
          try { await pc.addIceCandidate(c); } catch (_) {}
        }
        pendingRemoteCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        logLine('sent answer');
      } else if (msg.kind === 'answer') {
        logLine('received answer');
        if (!pc) { logLine('answer with no PC; ignoring'); return; }
        await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
        for (const c of pendingRemoteCandidates) {
          try { await pc.addIceCandidate(c); } catch (_) {}
        }
        pendingRemoteCandidates = [];
      } else if (msg.kind === 'candidate') {
        if (!pc || !pc.remoteDescription) {
          pendingRemoteCandidates.push(msg.candidate);
          return;
        }
        try {
          await pc.addIceCandidate(msg.candidate);
        } catch (err) {
          logLine('addIceCandidate failed: ' + err.message);
        }
      } else if (msg.kind === 'hangup') {
        logLine('peer hung up');
        teardown();
      }
    }

    // Top button toggles between call and hangup based on current state.
    $toggle.on('click', () => {
      if (pc) hangup(); else call();
    });

    // Mic mute is just track.enabled — no renegotiation needed.
    $micBtn.on('click', () => {
      if (!localStream) return;
      micEnabled = !micEnabled;
      localStream.getAudioTracks().forEach((t) => { t.enabled = micEnabled; });
      if (micEnabled) {
        $micBtn.text('Mute mic')
          .removeClass('bg-rose-200 hover:bg-rose-300')
          .addClass('bg-gray-200 hover:bg-gray-300');
      } else {
        $micBtn.text('Unmute mic')
          .removeClass('bg-gray-200 hover:bg-gray-300')
          .addClass('bg-rose-200 hover:bg-rose-300');
      }
    });

    // Volume slider drives the HTMLMediaElement.volume directly. Label
    // mirrors the value for visibility.
    $volume.on('input', () => {
      const v = parseInt($volume.val(), 10);
      $remote[0].volume = v / 100;
      $volumeLabel.text(v);
    });

    $clearLog.on('click', () => $log.empty());

    render('idle');
    startSignaling();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
