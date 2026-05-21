// meet.js — minimal WebRTC call between two f2f-desktop lite clients.
//
// Signalling rides on the engine's hole-punched UDP socket via the
// 0xF2-prefixed signal-frame transport (see internal/lite/client.go).
// Each signal-frame body is a JSON envelope:
//   { kind: 'offer' | 'answer' | 'candidate' | 'hangup', sdp/candidate }
//
// ICE uses Google's public STUN to discover server-reflexive candidates,
// since lite clients don't have an overlay subnet for host-candidate
// matching to be useful by itself.

import { SendSignal } from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

const ICE_SERVERS = [
  { urls: 'stun:stun.l.google.com:19302' },
];

export function startMeet() {
  // DOM
  const $paneYou = document.getElementById('ax-pane-you');
  const $panePeer = document.getElementById('ax-pane-peer');
  const $videoYou = document.getElementById('ax-video-you');
  const $videoPeer = document.getElementById('ax-video-peer');
  const $youHost = document.getElementById('ax-you-host');
  const $peerHost = document.getElementById('ax-peer-host');
  const $resYou = document.getElementById('ax-res-you');
  const $resPeer = document.getElementById('ax-res-peer');
  const $callBtn = document.getElementById('ax-call-btn');
  const $callMeta = document.getElementById('ax-call-meta');
  const $callState = $callBtn.querySelector('.ax-btn-state');
  const $callLabel = $callBtn.querySelector('.ax-btn-label');
  const $micBtn = document.getElementById('ax-mic-btn');
  const $micState = $micBtn.querySelector('.ax-btn-state');
  const $camBtn = document.getElementById('ax-cam-btn');
  const $camState = $camBtn.querySelector('.ax-btn-state');
  const $callSel = document.getElementById('ax-call-peer');
  const $logBody = document.getElementById('ax-log-body');
  const $logCount = document.getElementById('ax-log-count');
  const $logClear = document.getElementById('ax-log-clear');

  // State
  let pc = null;
  let localStream = null;
  let pendingCandidates = []; // ICE that arrived before setRemoteDescription
  let micEnabled = false;
  let camEnabled = false;
  let callPartner = '';       // tunnel_ip of the peer we're talking to
  let logCount = 0;

  // ---- log ----
  function timestamp() {
    const t = new Date();
    return t.toTimeString().slice(0, 8) + '.' + String(t.getMilliseconds()).padStart(3, '0');
  }
  function logLine(msg) {
    const row = document.createElement('div');
    row.className = 'ax-log-event';
    const c1 = document.createElement('span'); c1.className = 'ax-log-time'; c1.textContent = timestamp();
    const c2 = document.createElement('span'); c2.className = 'ax-log-icon'; c2.textContent = '·';
    const c3 = document.createElement('span'); c3.className = 'ax-log-msg'; c3.textContent = msg;
    row.append(c1, c2, c3);
    $logBody.appendChild(row);
    $logBody.scrollTop = $logBody.scrollHeight;
    logCount++;
    $logCount.textContent = String(logCount);
  }
  $logClear.addEventListener('click', () => {
    $logBody.replaceChildren(); logCount = 0; $logCount.textContent = '0';
  });

  // ---- call state visual ----
  function setState(state) {
    $callBtn.classList.remove('state-connecting', 'state-connected');
    if (state === 'connecting') {
      $callBtn.classList.add('state-connecting');
      $callState.textContent = '●'; $callLabel.textContent = 'connecting…';
    } else if (state === 'connected') {
      $callBtn.classList.add('state-connected');
      $callState.textContent = '●'; $callLabel.textContent = 'hang up';
    } else {
      $callState.textContent = '▶'; $callLabel.textContent = 'call peer';
    }
  }

  function updateMicCamUI() {
    if (micEnabled) { $micBtn.classList.add('active'); $micState.textContent = '●'; }
    else            { $micBtn.classList.remove('active'); $micState.textContent = '○'; }
    if (camEnabled) { $camBtn.classList.add('active'); $camState.textContent = '■'; }
    else            { $camBtn.classList.remove('active'); $camState.textContent = '□'; }
    const hasVideo = camEnabled && localStream && localStream.getVideoTracks().some((t) => t.enabled);
    $paneYou.classList.toggle('has-video', !!hasVideo);
  }

  // ---- signal transport ----
  async function sendSignal(msg) {
    if (!callPartner) { logLine('no callPartner; cannot send signal'); return; }
    try {
      await SendSignal(callPartner, JSON.stringify(msg));
    } catch (err) {
      logLine('send failed: ' + (err?.message || err));
    }
  }

  EventsOn('signal', async (ev) => {
    let msg;
    try { msg = JSON.parse(ev.body); }
    catch { logLine('non-JSON signal from ' + ev.from + ': ' + ev.body); return; }
    if (!msg || !msg.kind) return;
    // The first incoming signal binds the callee partner; subsequent
    // ICE candidates etc. use the same.
    if (!callPartner || callPartner === ev.from) callPartner = ev.from;
    await handleSignal(msg);
  });

  // ---- local media ----
  // Fault-tolerant: try audio+video → audio-only → video-only → no
  // local stream. A peer without devices can still RECEIVE remote
  // audio/video — they just can't send anything. Returning null from
  // here is normal in that case; callers must guard against it.
  async function ensureLocalStream() {
    if (localStream) return localStream;
    const attempts = [
      { audio: true, video: true },
      { audio: true, video: false },
      { audio: false, video: true },
    ];
    let lastErr = null;
    for (const constraints of attempts) {
      try {
        localStream = await navigator.mediaDevices.getUserMedia(constraints);
        break;
      } catch (err) {
        lastErr = err;
        // Common errors: NotFoundError (no device), NotAllowedError (user
        // denied), OverconstrainedError (impossible combo). Try the next
        // fallback rather than giving up.
      }
    }
    if (!localStream) {
      logLine('no local media: ' + (lastErr ? lastErr.message : 'unknown') + ' — receive-only call');
      $micBtn.disabled = true; $camBtn.disabled = true;
      micEnabled = false; camEnabled = false;
      updateMicCamUI();
      return null;
    }
    micEnabled = localStream.getAudioTracks().length > 0;
    camEnabled = localStream.getVideoTracks().length > 0;
    localStream.getAudioTracks().forEach((t) => (t.enabled = micEnabled));
    localStream.getVideoTracks().forEach((t) => (t.enabled = camEnabled));
    $videoYou.srcObject = localStream;
    try { await $videoYou.play(); } catch (_) {}
    $micBtn.disabled = !micEnabled;
    $camBtn.disabled = !camEnabled;
    updateMicCamUI();
    const v = localStream.getVideoTracks()[0];
    if (v && v.getSettings) {
      const s = v.getSettings();
      if (s.width && s.height) $resYou.textContent = `${s.width}×${s.height}`;
    }
    return localStream;
  }

  // ---- peer connection ----
  function newPC() {
    const conn = new RTCPeerConnection({ iceServers: ICE_SERVERS });
    conn.onicecandidate = (e) => {
      if (e.candidate) sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
    };
    conn.oniceconnectionstatechange = () => {
      logLine('ice: ' + conn.iceConnectionState);
      if (conn.iceConnectionState === 'connected' || conn.iceConnectionState === 'completed') {
        setState('connected');
      } else if (conn.iceConnectionState === 'failed' || conn.iceConnectionState === 'disconnected' || conn.iceConnectionState === 'closed') {
        // anacrolix-like soft fail: don't auto-teardown on disconnected
        // (it can recover); but failed/closed means done.
        if (conn.iceConnectionState !== 'disconnected') teardown();
      }
    };
    conn.ontrack = (e) => {
      logLine('ontrack: ' + e.track.kind);
      const [stream] = e.streams;
      if (!stream) return;
      $videoPeer.srcObject = stream;
      $panePeer.classList.add('has-video');
      try { $videoPeer.play(); } catch (_) {}
      const v = stream.getVideoTracks()[0];
      if (v) {
        v.addEventListener('unmute', () => {
          const s = v.getSettings ? v.getSettings() : {};
          if (s.width && s.height) $resPeer.textContent = `${s.width}×${s.height}`;
        });
      }
    };
    return conn;
  }

  // ---- call lifecycle ----
  async function call() {
    const to = $callSel.value;
    if (!to) { $callMeta.textContent = 'pick a peer'; return; }
    callPartner = to;
    $callMeta.textContent = '';
    try {
      setState('connecting');
      await ensureLocalStream(); // may be null if no devices — fine
      pc = newPC();
      if (localStream) {
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
      } else {
        // No outgoing tracks — explicitly add recv-only transceivers so
        // the SDP includes m-lines for the remote side to send into.
        pc.addTransceiver('audio', { direction: 'recvonly' });
        pc.addTransceiver('video', { direction: 'recvonly' });
      }
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await sendSignal({ kind: 'offer', sdp: offer.sdp });
      logLine('sent offer → ' + to);
    } catch (err) {
      logLine('call error: ' + err.message);
      teardown();
    }
  }

  async function hangup() {
    await sendSignal({ kind: 'hangup' });
    teardown();
  }

  function teardown() {
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    if (localStream) { localStream.getTracks().forEach((t) => t.stop()); localStream = null; }
    pendingCandidates = [];
    $videoYou.srcObject = null;
    $videoPeer.srcObject = null;
    $paneYou.classList.remove('has-video');
    $panePeer.classList.remove('has-video');
    $resYou.textContent = '—';
    $resPeer.textContent = '—';
    micEnabled = false; camEnabled = false;
    $micBtn.disabled = true; $camBtn.disabled = true;
    updateMicCamUI();
    callPartner = '';
    setState('idle');
    logLine('teardown');
  }

  // ---- incoming signal ----
  async function handleSignal(msg) {
    if (msg.kind === 'offer') {
      // If we already have a PC (renegotiation), answer in place.
      if (pc && pc.signalingState !== 'closed') {
        try {
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
          const answer = await pc.createAnswer();
          await pc.setLocalDescription(answer);
          await sendSignal({ kind: 'answer', sdp: answer.sdp });
          logLine('sent renegotiation answer');
        } catch (err) {
          logLine('renegotiation failed: ' + err.message);
        }
        return;
      }
      logLine('received offer from ' + callPartner);
      setState('connecting');
      try {
        await ensureLocalStream(); // tolerates no devices
        pc = newPC();
        if (localStream) {
          localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        }
        // setRemoteDescription creates transceivers based on the offer;
        // no need to addTransceiver manually here.
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        for (const c of pendingCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
        pendingCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        logLine('sent answer');
      } catch (err) {
        logLine('answer flow failed: ' + err.message);
        teardown();
      }
    } else if (msg.kind === 'answer') {
      logLine('received answer');
      if (!pc) return;
      await pc.setRemoteDescription({ type: 'answer', sdp: msg.sdp });
      for (const c of pendingCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
      pendingCandidates = [];
    } else if (msg.kind === 'candidate') {
      if (!pc || !pc.remoteDescription) { pendingCandidates.push(msg.candidate); return; }
      try { await pc.addIceCandidate(msg.candidate); }
      catch (err) { logLine('addIceCandidate failed: ' + err.message); }
    } else if (msg.kind === 'hangup') {
      logLine('peer hung up');
      teardown();
    }
  }

  // ---- controls ----
  $callBtn.addEventListener('click', () => {
    if (pc) hangup(); else call();
  });
  $micBtn.addEventListener('click', () => {
    if (!localStream) return;
    micEnabled = !micEnabled;
    localStream.getAudioTracks().forEach((t) => (t.enabled = micEnabled));
    updateMicCamUI();
  });
  $camBtn.addEventListener('click', () => {
    if (!localStream) return;
    camEnabled = !camEnabled;
    localStream.getVideoTracks().forEach((t) => (t.enabled = camEnabled));
    updateMicCamUI();
  });

  // Identity labels
  window.f2fSetIdentity = function ({ myName, myTunnelIP }) {
    $youHost.textContent = (myName || 'you') + ' @ ' + (myTunnelIP || 'localhost');
  };
  window.f2fSetPeerLabel = function (name, tunnelIP) {
    $peerHost.textContent = (name || 'peer') + ' @ ' + (tunnelIP || '—');
  };

  // Init UI
  setState('idle');
  updateMicCamUI();
}
