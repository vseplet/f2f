# TODO

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

## Drop / file sharing — Stage 4: per-file ACL with peer allowlist

Goal: each shared file can be restricted to a specific list of peers.
Anyone outside the allowlist neither sees the file in `/api/files`,
nor can connect to download it.

Current state (Stages 1-3 done): any peer in the camp can see any
file via `/api/files` polling, and can download it via BT once they
have the magnet. For friends-circle this is fine; for larger camps
or sensitive content, missing.

Design — two enforcement layers, both required:

1. **Discovery filter.** `/api/files` already runs on the tunnel
   listener, so `r.RemoteAddr` gives us the caller's tunnel_ip. We
   resolve that to a peer name via `engine.peers` and skip entries
   whose `allowed_peers` list doesn't include the caller (empty list
   = public to the whole camp, current behavior). This is the soft
   barrier — info_hash never leaks to unauthorised peers.

2. **BT-level enforcement.** anacrolix exposes a `Torrent`-level
   connection filter (or we wrap the listener with a custom
   `net.Conn` accept hook). Reject incoming TCP from any IP not in
   the file's allowlist. This is the hard barrier — even if a peer
   somehow guesses or shares the info_hash, they can't actually
   connect.

Data model addition:

```go
type SeedHandle struct {
    ...existing...
    AllowedPeers []string // peer names; empty = camp-public
}
```

Persistence — keep allowlist next to the file:
`~/Library/Application Support/f2f/shared/.f2f-meta/<info_hash>.json`
with `{allowed_peers: [...]}`. On engine start, reload.

UI additions in the drop tab:

- Each "my shared files" row gains an "audience" pill: "everyone" or
  "alice, bob". Click → open a small modal with checkboxes for camp
  peers. Save → PUT `/api/files/mine/<hash>/audience`.
- The "camp library" view stays the same — peers only see files they
  can actually access (filtered server-side).

API additions:

- `PUT /api/files/mine/<info_hash>/audience` body `{allowed_peers: []}`.
- `GET /api/files` (tunnel) and `GET /api/files/mine` already exist —
  former gets ACL filter, latter unchanged.

Scope: ~200 lines (engine ACL filter + persistence + connection
reject + UI modal). Probably one PR. Depends on figuring out the
right anacrolix hook for connection-level rejection (the
discovery-layer block is straightforward).

## Identity & access control — full architecture (staged)

Goal: cryptographic identity for users (not just devices, not just
tunnel_ip-trust), proper camp gating so a leaked `camp_id` doesn't let
strangers in, and OAuth/OIDC for peer-hosted services that prove "this
is user X" without passwords.

Today: identity == peer name string, anyone with `camp_id` can join,
auth in HTTP-through-tunnel is just "you came from this tunnel_ip so
you must be peer Y". Fine for two friends, insufficient for anything
larger or any service that needs accounts.

### Layers

1. **User identity**. Each user owns an Ed25519 keypair
   `(user_priv, user_pub)`. `user_pub` is the stable cross-device,
   cross-camp identifier. Persisted at `~/.f2f/identity/`. Generated
   on first install OR imported via 12-word BIP-39 recovery phrase.
   `sub` claim in OIDC tokens is derived from `user_pub`.

2. **Device identity**. Each machine has its own Ed25519 keypair
   `(device_priv, device_pub)` generated automatically. A
   user_priv-signed attestation `{device_pub, device_name, issued_at,
   user_pub}` proves "this device belongs to this user". Tunnel_ip
   still per-device (sticky binding), but identity is by user_pub.

3. **Camp ownership**. Camp gains a `creator_pub` field set at
   creation, plus `admin_pubs[]` (creator can promote). Admins
   approve/reject pending joiners, ban members, change camp policy.
   Camp policy modes: `open` (current — anyone with id), `invite`
   (signed invite token required), `closed` (member must be on
   pre-approved user_pub list, edits require admin signature).

4. **Member onboarding**. New peer joins via:
   - Open mode: just announce (current).
   - Invite mode: present invite token signed by existing
     admin/member; camp validates signature + freshness + single-use.
   - Closed mode: must be pre-listed by admin.
   On join, peer's `user_pub` and current device attestation get
   added to camp roster.

5. **Multi-device pairing**. QR-pair flow (à la Signal / KeePassXC /
   WhatsApp Web):
   - Primary device shows QR + short numeric code.
   - New device scans → connects to primary via local mDNS / TCP /
     camp-relay using QR seed as pre-shared key.
   - Primary prompts user: "device 'iPhone' wants to join your
     identity?".
   - On approve, primary signs device attestation for new device's
     pubkey. `user_priv` itself never leaves primary device.
   Fallback: 12-word recovery phrase re-derives user_priv on a fresh
   device if primary is lost.

6. **Revocation**. Camp roster grows `revoked_devices[]` and
   `revoked_users[]`. Admins publish revocations. Engines check on
   poll and refuse to trust revoked attestations.

7. **mTLS reverse-proxy**. Replace today's tunnel_ip-trust with
   ClientAuth=RequireAndVerifyClientCert. Client certs signed by
   user's local CA carrying `user_pub` in extension. Server-side
   ClientCAs pool = trusted peer CAs. Server learns identity from
   `r.TLS.PeerCertificates[0]`.

8. **Per-peer OIDC IdP**. Each engine runs OIDC endpoints
   (`/authorize`, `/token`, `/userinfo`, `/jwks`,
   `/.well-known/openid-configuration`) for services hosted on that
   same peer. Service config example: Gitea on Alice's machine
   targets `https://auth.alice.<camp>.f2f` as OpenID provider.
   `sub = user_pub_hash`, `device = device_name`. Identity confirmed
   via mTLS at TLS layer, OIDC just wraps it in standard format for
   services that need OAuth flow.

### Phased delivery

- **Phase 1 — User identity + camp owner/admin role.**
  Engine: generate/load user_priv, derive user_pub. UI: show identity
  fingerprint on first start, offer "create new / import phrase".
  camp-server: add `creator_pub`, `admin_pubs[]`, peer status
  (pending/active/banned), admin API endpoints, optional approval
  mode. UI: pending-members list for admins with approve/ban
  buttons. No multi-device yet — one user = one device.

- **Phase 2 — Invite-only camps + revocation.**
  Signed invite tokens generated by admins, validated at announce.
  Member-list + ban actions broadcast as signed events.

- **Phase 3 — Multi-device pairing.**
  Device attestation, QR-pair flow, recovery phrase, primary device
  promotion. Identity now spans multiple devices.

- **Phase 4 — mTLS reverse-proxy.**
  Replace tunnel_ip-trust with verified client certs. Backwards
  compatible by keeping HTTP fallback while rolling out.

- **Phase 5 — OIDC provider per peer.**
  Embed `github.com/zitadel/oidc/v3/pkg/op` (or similar) for the
  standard endpoints, JWT signing with a per-peer OIDC key.
  Client-registration UI for services. JIT user provisioning
  expected to work with most OIDC consumers (Gitea, Grafana,
  Vaultwarden, etc).

Scope: each phase is multi-week. Total is on the order of a quarter
of focused work. Do NOT start any phase without explicit scope cut on
camp-server changes — that's where backwards-compat will hurt.
