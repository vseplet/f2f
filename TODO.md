# TODO

## Two HTTP listeners for the mac UI (loopback + tunnel_ip)

Goal: keep `f2f-mac ui` bound to `127.0.0.1` by default but still let the
remote peer reach `/api/signal/inbox` through the tunnel, without exposing
the UI to the LAN.

Current state: the UI binds `127.0.0.1:8080` by default, but WebRTC
signalling between peers (`web/server.go:handleSignalOutbox`) POSTs to
`http://<peer_tunnel_ip>:<port>/api/signal/inbox` — so the inbox listener
must accept connections that arrive on the utun interface. Today the only
way to make that work is binding `0.0.0.0`, which also exposes the UI
(including `/api/start` etc.) to anyone on the same Wi-Fi.

Plan:

- `web.Server` gains a second `http.Server` (`tunnelSrv`) with a separate
  mux that registers **only** `POST /api/signal/inbox`. Everything else
  on that listener returns 404.
- New methods `BindTunnel(ip string) error` / `UnbindTunnel() error` —
  start/stop the second listener on `ip:<same port as loopback>`.
- `engine.Start` calls `web.BindTunnel(cfg.LocalIP)` after utun comes up;
  `engine.Stop` calls `web.UnbindTunnel()`. Use a callback so `engine`
  doesn't pull in `web`.
- On config reload / room change (`LocalIP` shifts) — `Unbind` → `Bind`.
- Same TCP port for both listeners (e.g. both on `:8080`); two binds on
  different IPs is fine on macOS.

Why it's safe by default: `10.99.0.0/24` only has a route via utun on
this machine. A LAN neighbour has no path to `10.99.0.1`, so the tunnel
listener is physically unreachable from outside the tunnel — no
middleware or auth needed.

When this task becomes obsolete: once WebRTC signalling moves off
HTTP-through-tunnel and onto camp WebSocket, the inbox endpoint
disappears entirely and the second listener can be removed. Until then,
this is the cleanest workaround.

Scope: ~50–80 lines across `internal/web/server.go` and
`internal/engine/engine.go`. No changes to camp, utun, routes, or pf.

## Group calls — staged roadmap (mesh → Go-SFU → cloud SFU)

Goal: support video/audio calls for more than two participants without
giving up the peer-to-peer philosophy.

Key insight specific to f2f: WebRTC media already flows through our
Go-managed UDP tunnel (`web/server.go:rewriteMDNS` deliberately rewrites
ICE host candidates to the peer's `tunnel_ip` so the browser picks the
utun path). That means a "peer-relay" architecture for us is **SFU in
Go**, not SFU in a browser tab — qualitatively different from how
classic WebRTC SFUs are usually discussed.

### Stage 1 — mesh up to 4 participants

Pure WebRTC mesh between browsers (each side opens N−1
`RTCPeerConnection`s, encoder/decoder per peer). Engine state changes:

- `engine.peerPtr` (single peer) → `engine.peers map[string]*PeerState`
  keyed by name or tunnel_ip.
- utun goes from point-to-point (`10.99.0.1 → 10.99.0.2`) to subnet
  (`10.99.0.0/24`). Outgoing packets from utun look up the destination
  tunnel_ip in the peers map → dispatch UDP to that peer's endpoint.
- Signal routing in UI uses the peer name we want to talk to (we already
  have this in `signalMsg.To`); HTTP-through-tunnel forwarding picks the
  right peer's `tunnel_ip:port`.
- UI gallery: thumbnails for each remote peer, per-peer mute/volume.

Realistic ceiling: 4 people on home FTTH, 3 on mobile/4G. Above that
upload bandwidth and encoder CPU kill it (see analysis in chat).

NAT caveat: every pair needs an independent hole-punch. If any pair is
both-symmetric-NAT they don't see each other (no relay yet). Acceptable
in stage 1 with a clear error message.

### Stage 2 — Go-SFU via Pion (relay through one peer's engine)

Run [Pion](https://github.com/pion/webrtc) inside `engine` to terminate
WebRTC against the **local** browser only. The browser opens a single
`RTCPeerConnection` to its own Go (over loopback / utun), not N
peer-to-peer connections. Between Go nodes, media flows over our own
UDP tunnel using our own framing — no peer-to-peer WebRTC.

Architecture:

```
Browser A ──SRTP──> Go_A (Pion)  ──tunnel──>  Go_B (Pion)  ──SRTP──> Browser B
                       │                                ↑
                       └──tunnel──> Go_C ──SRTP──> Browser C
```

One of the Go nodes is elected as the **relay** for the room. It
receives one stream from each Go (one per browser), forwards to the
others. CPU at the relay-Go is low (Pion RTP routing, no transcoding).
Bandwidth at the relay-Go is `N × stream` in both directions — for
symmetric 200/200 FTTH this is fine up to ~8–10 people in 720p.

Wins over Stage 1:

- Browser-side: one `RTCPeerConnection`, one encoder run regardless of
  N. Drastically less browser CPU/memory.
- E2E encryption via Insertable Streams API: browsers encrypt payload
  with a room-shared key; Go sees only RTP headers and just routes.
  Relay-Go cannot eavesdrop on media. Documented pattern in Jitsi /
  Signal group calls.
- Simulcast/SVC handled at the Go layer (Pion supports it); relay
  selects which layer to forward per consumer based on their reported
  bandwidth.
- Relay-Go failover: re-electing a new relay only requires
  reconnecting Go ↔ Go links over the tunnel; browser PCs stay up
  (they're talking to local Go). Much cheaper than tearing down and
  rebuilding mesh.

Relay election (do this in `engine` + `rendezvous`):

- camp exposes per-peer hints: `upload_mbps_estimate`, `cgnat_detected`,
  `is_mobile`. Each peer measures itself at startup (rough speedtest,
  STUN behaviour probe).
- camp picks the candidate with the best score; broadcasts the choice
  on `peer-joined` / room-state events.
- Re-election triggers: relay disconnects, or a better candidate joins
  and stays for >60s.

Open questions for this stage:

- Authoritative SDP renegotiation between local browser and local Go
  when remote peers come and go. Pion has facilities; need to wire
  them.
- How to expose per-peer-stream stats in the UI (jitter, packet loss
  per remote stream).
- TURN-style fallback when picked relay isn't reachable from someone
  (both behind symmetric NAT). Could route through a second peer if
  there is one with better reachability.

Scope: large — roughly Pion integration, room state in `engine`, a
small SFU loop, UI gallery, relay election. Likely a multi-week project,
splittable into commits per layer.

### Stage 3 — hosted SFU (only if we ever outgrow stage 2)

Drop in an off-the-shelf SFU on our infra ([LiveKit](https://livekit.io)
or [mediasoup](https://mediasoup.org) on fly.io) for rooms of 20+ or
audiences where home-relay can't be relied on. Becomes "f2f with
optional cloud media plane" — same client, different relay tier
selectable per room. Not on the near roadmap; included here so we
remember the trade-off exists.

Cost reality: ~$100–500/month at modest concurrency. Justifies itself
only if there's a reason to push past stage 2.

## Sleep/wake recovery — auto-heal stale NAT state without restart

Goal: after the Mac wakes from sleep, peers should re-establish
reachability automatically. Currently we have to stop both engines, wait
for camp eviction (~60s), and start again.

Symptom we observed: Vsevolod's machine slept overnight while Fedor's
stayed up. On wake, the camp peer list still looked correct
(`/api/status` showed both peers with the same `udp_endpoint` strings),
but reachability was asymmetric — Fedor → Vsevolod packets arrived,
Vsevolod → Fedor were silently lost. Manual full-restart of both engines
fixed it.

Likely cause: during sleep our UDP socket is suspended for hours. The
router's NAT mapping for that socket expires (NAT timeouts are
60-300s). On wake, the engine's `time.Ticker`s resume and announce/punch
loops fire — but the first outbound packet creates a *new* external
port mapping in our NAT. Camp learns the new mapping on the next
announce, the remote peer learns it on the next poll. In theory that's
~50s of disruption then self-heal.

Why it doesn't always self-heal:
- `AnnounceClient.Run` logs UDP send errors but never re-creates the
  socket if it's wedged after wake. On macOS the socket can come back
  in a half-broken state where writes silently fail.
- DNS resolve for `f2f-camp.fly.dev` in the HTTP poller can fail in
  the first seconds after wake (Wi-Fi DHCP still negotiating).
- Hole-punch cadence drops to 25s once a peer is fresh, so the
  recovery window is large — if anything jitters during it, we miss it.

Plan:

- New file `internal/engine/sleepwake_darwin.go` — subscribe to
  IORegisterForSystemPower via cgo (or use a Go wrapper like
  `github.com/prashantgupta24/mac-sleep-notifier`).
- On wake notification, the engine:
  1. Closes and re-opens the UDP socket (forces a fresh external NAT
     mapping).
  2. Re-binds the same local port; rebuilds `e.udp` pointer atomically;
     restarts `peerToTunLoop` against the new socket.
  3. Resets `peer.LastSeenMs = 0` for every peer → hole-punch loop
     switches back to 1Hz burst until each peer responds.
  4. Triggers an immediate `AnnounceOnce` and an immediate camp
     peer-list poll, instead of waiting for the next tick.
- Robustness fallback (helps even without the IOKit hook): if any
  peer's `LastSeenMs` has been stale for >60s while we're actively
  punching, treat it as a wake-equivalent — recreate socket, reset all
  LastSeen, re-announce.

Out of scope here: peer-side recovery on Windows (Fedor side). Their
engine notices our new endpoint via the camp poll just like in any
other rebind scenario; nothing additional is required there.

Scope: ~80–150 lines including the IOKit binding. New file in
`internal/engine/`, small wiring in `engine.Start`. No changes to camp
or UI.

## Egress: react to default-route changes while running

Goal: if the user's default route iface changes while the engine is up
(switch from Wi-Fi to Ethernet, dock/undock, VPN toggle), automatically
re-apply pf NAT against the new iface.

Current state: `engine.Start` auto-picks the default route iface via
`detectDefaultRouteIface()`. That's correct at startup, but if the
iface changes mid-session, pf NAT keeps pointing at the old one — the
remote peer's traffic then routes out the wrong interface (or
nowhere).

Plan:

- Poll `detectDefaultRouteIface()` every ~5s alongside the existing
  peer-list poller, or subscribe to the BSD PF_ROUTE socket for
  RTM_ADD/RTM_DELETE notifications (cleaner, no polling).
- When the default iface changes: tear down current pf anchor, call
  `egress.Open` against the new iface.
- Log a clear `egress: iface changed en0 → en1` line so it's obvious
  in diagnostics.

Out of scope: handling multiple simultaneous egress interfaces (only
ever one default route at a time).

Scope: small once decided on polling vs PF_ROUTE — ~50 lines either
way, all in `internal/engine/`.
