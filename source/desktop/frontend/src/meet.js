// meet.js — WebRTC call between two f2f-desktop lite clients with
// audio, video, and screen share (getDisplayMedia).
//
// Signalling rides on the engine's hole-punched UDP socket via the
// 0xF2-prefixed signal-frame transport (see internal/lite/client.go).
// Each signal-frame body is a JSON envelope:
//   { kind: 'offer'|'answer'|'candidate'|'hangup'|'screen-share', ... }
//
// ICE uses Google's public STUN to discover server-reflexive candidates.

import { SendSignal } from '../wailsjs/go/main/App';
import { EventsOn } from '../wailsjs/runtime/runtime';

const ICE_SERVERS = [
  { urls: 'stun:stun.l.google.com:19302' },
];

export function startMeet() {
  // ---- DOM ----
  const $paneYou = document.getElementById('ax-pane-you');
  const $panePeer = document.getElementById('ax-pane-peer');
  const $paneYouScreen = document.getElementById('ax-pane-you-screen');
  const $panePeerScreen = document.getElementById('ax-pane-peer-screen');
  const $videoYou = document.getElementById('ax-video-you');
  const $videoPeer = document.getElementById('ax-video-peer');
  const $videoYouScreen = document.getElementById('ax-video-you-screen');
  const $videoPeerScreen = document.getElementById('ax-video-peer-screen');
  const $youHost = document.getElementById('ax-you-host');
  const $peerHost = document.getElementById('ax-peer-host');
  const $youScreenHost = document.getElementById('ax-you-screen-host');
  const $peerScreenHost = document.getElementById('ax-peer-screen-host');
  const $resYou = document.getElementById('ax-res-you');
  const $resPeer = document.getElementById('ax-res-peer');
  const $resYouScreen = document.getElementById('ax-res-you-screen');
  const $resPeerScreen = document.getElementById('ax-res-peer-screen');
  const $callBtn = document.getElementById('ax-call-btn');
  const $callMeta = document.getElementById('ax-call-meta');
  const $callState = $callBtn.querySelector('.ax-btn-state');
  const $callLabel = $callBtn.querySelector('.ax-btn-label');
  const $micBtn = document.getElementById('ax-mic-btn');
  const $micState = $micBtn.querySelector('.ax-btn-state');
  const $camBtn = document.getElementById('ax-cam-btn');
  const $camState = $camBtn.querySelector('.ax-btn-state');
  const $shareBtn = document.getElementById('ax-share-btn');
  const $shareState = $shareBtn.querySelector('.ax-btn-state');
  const $callSel = document.getElementById('ax-call-peer');
  const $volTrack = document.getElementById('ax-vol-track');
  const $volFill = document.getElementById('ax-vol-fill');
  const $volValue = document.getElementById('ax-vol-value');
  const $logBody = document.getElementById('ax-log-body');
  const $logCount = document.getElementById('ax-log-count');
  const $logClear = document.getElementById('ax-log-clear');
  const $chatInput = document.getElementById('ax-chat-input');

  // ---- state ----
  let pc = null;
  let localStream = null;
  let pendingCandidates = [];
  let micEnabled = false;
  let camEnabled = false;
  let callPartner = '';
  let isOfferer = false;
  let screenStream = null;
  let screenSenders = [];          // RTCRtpSenders for screen tracks
  let peerScreenStreamId = null;   // remembered from incoming screen-share signal
  let dataChannel = null;          // chat — caller creates, answerer receives
  let volume = 80;
  let logCount = 0;
  let myName = 'you';
  let peerName = 'peer';

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
  // Chat shares the same log body. who === myName means "from me",
  // styled differently if the CSS opts in via .from-you.
  function logChat(who, msg) {
    const row = document.createElement('div');
    row.className = 'ax-log-event is-chat' + (who === myName ? ' from-you' : '');
    const c1 = document.createElement('span'); c1.className = 'ax-log-time'; c1.textContent = timestamp();
    const c2 = document.createElement('span'); c2.className = 'ax-log-icon'; c2.textContent = '<' + who + '>';
    const c3 = document.createElement('span'); c3.className = 'ax-log-msg'; c3.textContent = msg;
    row.append(c1, c2, c3);
    $logBody.appendChild(row);
    $logBody.scrollTop = $logBody.scrollHeight;
    logCount++;
    $logCount.textContent = String(logCount);
  }

  // ---- call state UI ----
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

  // ---- volume slider ----
  function applyVolumeUI() {
    $volFill.style.width = volume + '%';
    $volValue.textContent = String(volume);
  }
  function applyVolumeToPeers() {
    // Peer cam-pane: video element carries the cam audio (volume applies).
    // Peer screen-pane: separate element (system audio bundled with display).
    if ($videoPeer) $videoPeer.volume = volume / 100;
    if ($videoPeerScreen) $videoPeerScreen.volume = volume / 100;
  }
  function setVolumeFromEvent(e) {
    const r = $volTrack.getBoundingClientRect();
    const x = (e.touches ? e.touches[0].clientX : e.clientX) - r.left;
    let v = Math.round((x / r.width) * 100);
    if (v < 0) v = 0; if (v > 100) v = 100;
    volume = v;
    applyVolumeUI();
    applyVolumeToPeers();
  }
  let volDragging = false;
  $volTrack.addEventListener('mousedown', (e) => { volDragging = true; setVolumeFromEvent(e); });
  window.addEventListener('mousemove', (e) => { if (volDragging) setVolumeFromEvent(e); });
  window.addEventListener('mouseup', () => { volDragging = false; });
  applyVolumeUI();

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
    if (!callPartner || callPartner === ev.from) callPartner = ev.from;
    await handleSignal(msg);
  });

  // ---- local media (cam+mic) ----
  async function ensureLocalStream() {
    if (localStream) return localStream;
    const attempts = [
      { audio: true, video: true },
      { audio: true, video: false },
      { audio: false, video: true },
    ];
    let lastErr = null;
    for (const c of attempts) {
      try { localStream = await navigator.mediaDevices.getUserMedia(c); break; }
      catch (err) { lastErr = err; }
    }
    if (!localStream) {
      logLine('no local media: ' + (lastErr ? lastErr.message : 'unknown') + ' — receive-only');
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

  // ---- chat data channel ----
  function attachDataChannel(ch) {
    dataChannel = ch;
    ch.onopen = () => {
      $chatInput.disabled = false;
      $chatInput.placeholder = 'message peer';
      logLine('chat channel open');
    };
    ch.onclose = () => {
      $chatInput.disabled = true;
      $chatInput.placeholder = 'chat closed';
    };
    ch.onmessage = (e) => {
      logChat(peerName, String(e.data));
    };
  }
  function sendChat(text) {
    if (!dataChannel || dataChannel.readyState !== 'open') return false;
    try { dataChannel.send(text); }
    catch (err) { logLine('chat send: ' + err.message); return false; }
    logChat(myName, text);
    return true;
  }
  $chatInput.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    e.preventDefault();
    const t = $chatInput.value.trim();
    if (!t) return;
    if (sendChat(t)) $chatInput.value = '';
  });

  // ---- peer connection ----
  function newPC() {
    const conn = new RTCPeerConnection({ iceServers: ICE_SERVERS });
    // Answerer side receives the channel via this event; caller creates
    // it directly with createDataChannel() before generating the offer.
    conn.ondatachannel = (e) => attachDataChannel(e.channel);
    conn.onicecandidate = (e) => {
      if (e.candidate) sendSignal({ kind: 'candidate', candidate: e.candidate.toJSON() });
    };
    conn.oniceconnectionstatechange = () => {
      const st = conn.iceConnectionState;
      logLine('ice: ' + st);
      if (st === 'connected' || st === 'completed') setState('connected');
      else if (st === 'failed' || st === 'closed') teardown();
      // 'disconnected' is often transient — let WebRTC recover.
    };
    // Renegotiation on add/removeTrack — but only the offerer drives it
    // to keep glare out. Answerer-side track changes go through
    // forceRenegotiate (manual offer + promotion to offerer).
    conn.onnegotiationneeded = async () => {
      if (!isOfferer) return;
      try {
        if (conn.signalingState !== 'stable') return;
        const offer = await conn.createOffer();
        if (conn.signalingState !== 'stable') return;
        await conn.setLocalDescription(offer);
        await sendSignal({ kind: 'offer', sdp: offer.sdp });
        logLine('sent renegotiation offer');
      } catch (err) {
        logLine('renegotiation failed: ' + err.message);
      }
    };
    conn.ontrack = (e) => {
      const stream = e.streams[0];
      const isScreen = stream && peerScreenStreamId && stream.id === peerScreenStreamId;
      if (isScreen) {
        if (e.track.kind === 'video') {
          $videoPeerScreen.srcObject = stream;
          $videoPeerScreen.volume = volume / 100;
          $panePeerScreen.classList.remove('hidden');
          $panePeerScreen.classList.add('has-video');
          $videoPeerScreen.addEventListener('loadedmetadata', () => {
            $resPeerScreen.textContent = `${$videoPeerScreen.videoHeight}p`;
          }, { once: true });
          const hide = () => {
            $videoPeerScreen.srcObject = null;
            $panePeerScreen.classList.add('hidden');
            $panePeerScreen.classList.remove('has-video');
            $resPeerScreen.textContent = '—';
          };
          e.track.addEventListener('ended', hide);
          e.track.addEventListener('mute', hide);
          e.track.addEventListener('unmute', () => $panePeerScreen.classList.add('has-video'));
        } else if (e.track.kind === 'audio') {
          if (!$videoPeerScreen.srcObject) $videoPeerScreen.srcObject = stream;
          $videoPeerScreen.volume = volume / 100;
          $videoPeerScreen.play().catch(() => {});
        }
        logLine('remote ' + e.track.kind + ' track (screen)');
      } else if (e.track.kind === 'video') {
        $videoPeer.srcObject = stream;
        $videoPeer.volume = 0; // cam-video element doesn't carry audio
        $panePeer.classList.add('has-video');
        e.track.addEventListener('mute',   () => $panePeer.classList.remove('has-video'));
        e.track.addEventListener('unmute', () => $panePeer.classList.add('has-video'));
        $videoPeer.addEventListener('loadedmetadata', () => {
          $resPeer.textContent = `${$videoPeer.videoHeight}p`;
        }, { once: true });
        try { $videoPeer.play(); } catch (_) {}
        logLine('remote video track (cam)');
      } else if (e.track.kind === 'audio') {
        if (!$videoPeer.srcObject) $videoPeer.srcObject = stream;
        $videoPeer.volume = volume / 100;
        $videoPeer.play().catch(() => {});
        logLine('remote audio track');
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
      await ensureLocalStream();
      pc = newPC();
      isOfferer = true;
      // Caller-side data channel for chat. Must be created BEFORE the
      // offer is generated so the SDP carries the m=application line.
      attachDataChannel(pc.createDataChannel('chat', { ordered: true }));
      if (localStream) {
        localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
      } else {
        pc.addTransceiver('audio', { direction: 'recvonly' });
        pc.addTransceiver('video', { direction: 'recvonly' });
      }
      $shareBtn.disabled = false;
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
    if (dataChannel) { try { dataChannel.close(); } catch (_) {} dataChannel = null; }
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    if (localStream) { localStream.getTracks().forEach((t) => t.stop()); localStream = null; }
    if (screenStream) { screenStream.getTracks().forEach((t) => t.stop()); screenStream = null; }
    screenSenders = [];
    peerScreenStreamId = null;
    pendingCandidates = [];
    $chatInput.disabled = true;
    $chatInput.placeholder = 'connect to chat';
    $chatInput.value = '';
    $videoYou.srcObject = null;
    $videoPeer.srcObject = null;
    $videoYouScreen.srcObject = null;
    $videoPeerScreen.srcObject = null;
    $paneYou.classList.remove('has-video');
    $panePeer.classList.remove('has-video');
    $paneYouScreen.classList.add('hidden'); $paneYouScreen.classList.remove('has-video');
    $panePeerScreen.classList.add('hidden'); $panePeerScreen.classList.remove('has-video');
    $resYou.textContent = '—'; $resPeer.textContent = '—';
    $resYouScreen.textContent = '—'; $resPeerScreen.textContent = '—';
    micEnabled = false; camEnabled = false;
    $micBtn.disabled = true; $camBtn.disabled = true;
    $shareBtn.disabled = true; $shareBtn.classList.remove('active');
    $shareState.textContent = '▢';
    updateMicCamUI();
    isOfferer = false;
    callPartner = '';
    setState('idle');
    logLine('teardown');
  }

  // ---- screen share ----
  $shareBtn.addEventListener('click', () => {
    if (!pc) return;
    if (screenStream) stopScreenShare(); else startScreenShare();
  });
  async function startScreenShare() {
    if (!pc || screenStream) return;
    if (!navigator.mediaDevices || !navigator.mediaDevices.getDisplayMedia) {
      alert('getDisplayMedia not supported by this browser/webview.');
      return;
    }
    let stream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: true });
    } catch (err) {
      if (err.name !== 'NotAllowedError') logLine('getDisplayMedia: ' + err.message);
      return;
    }
    screenStream = stream;
    // Tell the peer this stream id is screen BEFORE renegotiation so
    // the incoming track on the other side gets tagged correctly.
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
    logLine('screen share started (' + stream.id + ')');
    // addTrack triggers onnegotiationneeded on the offerer. Answerer-
    // side needs a manual offer to renegotiate.
    if (!isOfferer) await forceRenegotiate();
  }
  async function stopScreenShare() {
    if (!screenStream) return;
    const stream = screenStream;
    screenStream = null;
    stream.getTracks().forEach((t) => t.stop());
    if (pc) screenSenders.forEach((s) => { try { pc.removeTrack(s); } catch (_) {} });
    screenSenders = [];
    $videoYouScreen.srcObject = null;
    $paneYouScreen.classList.add('hidden');
    $paneYouScreen.classList.remove('has-video');
    $resYouScreen.textContent = '—';
    $shareState.textContent = '▢';
    $shareBtn.classList.remove('active');
    await sendSignal({ kind: 'screen-share', on: false });
    logLine('screen share stopped');
    if (!isOfferer && pc) await forceRenegotiate();
  }
  // Answerer-side manual offer: onnegotiationneeded bails on non-offerer
  // to avoid glare, but answerer-side add/removeTrack still needs an
  // offer out. Promote ourselves to offerer for the rest of the call.
  async function forceRenegotiate() {
    if (!pc) return;
    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await sendSignal({ kind: 'offer', sdp: offer.sdp });
      isOfferer = true;
      logLine('forced renegotiation (promoted to offerer)');
    } catch (err) {
      logLine('forceRenegotiate failed: ' + err.message);
    }
  }

  // ---- incoming signal ----
  async function handleSignal(msg) {
    if (msg.kind === 'offer') {
      if (pc && pc.signalingState !== 'closed') {
        try {
          await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
          const answer = await pc.createAnswer();
          await pc.setLocalDescription(answer);
          await sendSignal({ kind: 'answer', sdp: answer.sdp });
          logLine('sent renegotiation answer');
        } catch (err) {
          logLine('renegotiation answer failed: ' + err.message);
        }
        return;
      }
      logLine('received offer from ' + callPartner);
      setState('connecting');
      try {
        await ensureLocalStream();
        pc = newPC();
        isOfferer = false;
        if (localStream) {
          localStream.getTracks().forEach((t) => pc.addTrack(t, localStream));
        }
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        for (const c of pendingCandidates) { try { await pc.addIceCandidate(c); } catch (_) {} }
        pendingCandidates = [];
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        await sendSignal({ kind: 'answer', sdp: answer.sdp });
        logLine('sent answer');
        $shareBtn.disabled = false;
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
      $shareBtn.disabled = false;
    } else if (msg.kind === 'candidate') {
      if (!pc || !pc.remoteDescription) { pendingCandidates.push(msg.candidate); return; }
      try { await pc.addIceCandidate(msg.candidate); }
      catch (err) { logLine('addIceCandidate failed: ' + err.message); }
    } else if (msg.kind === 'screen-share') {
      peerScreenStreamId = msg.on ? msg.streamId : null;
      logLine('peer screen share ' + (msg.on ? 'on (' + msg.streamId + ')' : 'off'));
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

  // Fullscreen buttons in pane headers — CSS-based, not the browser
  // Fullscreen API (which is unreliable inside WKWebView). The 'fs'
  // class makes the pane absolute-fill the document. Esc closes it.
  function exitAnyFs() {
    document.querySelectorAll('.ax-pane.fs').forEach((p) => p.classList.remove('fs'));
    document.body.classList.remove('fs-active');
  }
  document.querySelectorAll('.ax-pane-fs').forEach((btn) => {
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      const pane = btn.closest('.ax-pane');
      if (!pane) return;
      const wasOn = pane.classList.contains('fs');
      exitAnyFs();
      if (!wasOn) {
        pane.classList.add('fs');
        document.body.classList.add('fs-active');
      }
    });
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') exitAnyFs();
  });

  // ---- identity hooks (called from main.js refresh()) ----
  window.f2fSetIdentity = function (info) {
    if (info && info.myName) myName = info.myName;
    $youHost.textContent = myName + ' @ ' + (info?.myTunnelIP || 'localhost');
    if ($youScreenHost) $youScreenHost.textContent = $youHost.textContent + ' · screen';
  };
  window.f2fSetPeerLabel = function (name, tunnelIP) {
    if (name) peerName = name;
    $peerHost.textContent = peerName + ' @ ' + (tunnelIP || '—');
    if ($peerScreenHost) $peerScreenHost.textContent = $peerHost.textContent + ' · screen';
  };

  setState('idle');
  updateMicCamUI();
}
