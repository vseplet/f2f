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
    const $call = $('#audio-call-btn');
    const $hangup = $('#audio-hangup-btn');
    const $status = $('#audio-status');
    const $log = $('#audio-log');
    const $clearLog = $('#audio-clear-log');
    const $local = $('#audio-local');
    const $remote = $('#audio-remote');

    let pc = null;
    let localStream = null;
    let signalES = null;
    let pendingRemoteCandidates = [];

    function setStatus(s) { $status.text(s); }
    function logLine(s) {
      const at = new Date().toISOString().slice(11, 23);
      const div = document.createElement('div');
      div.textContent = at + '  ' + s;
      $log[0].appendChild(div);
      $log[0].scrollTop = $log[0].scrollHeight;
    }
    function showHangup() { $call.addClass('hidden'); $hangup.removeClass('hidden'); }
    function showCall()   { $call.removeClass('hidden'); $hangup.addClass('hidden'); }

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
        // EventSource auto-reconnects; just note it.
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
        setStatus('ICE: ' + st);
        logLine('ICE state: ' + st);
        if (st === 'failed' || st === 'closed' || st === 'disconnected') {
          if (st !== 'disconnected') teardown();
        }
      };
      conn.onconnectionstatechange = () => {
        logLine('PC state: ' + conn.connectionState);
      };
      return conn;
    }

    async function ensureLocalStream() {
      if (localStream) return localStream;
      localStream = await navigator.mediaDevices.getUserMedia({
        audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
        video: false,
      });
      $local[0].srcObject = localStream;
      logLine('mic acquired (' + localStream.getAudioTracks().length + ' tracks)');
      return localStream;
    }

    async function call() {
      try {
        setStatus('preparing…');
        await ensureLocalStream();
        pc = newPC();
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        const offer = await pc.createOffer({ offerToReceiveAudio: true });
        await pc.setLocalDescription(offer);
        await sendSignal({ kind: 'offer', sdp: offer.sdp });
        setStatus('offer sent, waiting for answer');
        logLine('sent offer');
        showHangup();
      } catch (err) {
        setStatus('error: ' + err.message);
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
        $local[0].srcObject = null;
      }
      $remote[0].srcObject = null;
      pendingRemoteCandidates = [];
      setStatus('idle');
      showCall();
      logLine('teardown');
    }

    async function handleSignal(msg) {
      if (msg.kind === 'offer') {
        logLine('received offer');
        if (pc) {
          // Already in a call. Politely tear down and accept the new offer.
          teardown();
        }
        await ensureLocalStream();
        pc = newPC();
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        // Flush any candidates that arrived before the offer.
        for (const c of pendingRemoteCandidates) {
          try { await pc.addIceCandidate(c); } catch (_) {}
        }
        pendingRemoteCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        setStatus('answered');
        logLine('sent answer');
        showHangup();
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

    $call.on('click', call);
    $hangup.on('click', hangup);
    $clearLog.on('click', () => $log.empty());

    // Subscribe to signals immediately on page load so an incoming call from
    // the peer is detected even before the user clicks into the audio tab.
    startSignaling();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
