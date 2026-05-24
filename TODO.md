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

## Overlay addressing — derive IPv6 from pub, drop camp's IP allocator

Goal: stop having the camp server hand out tunnel IPs. Every peer
computes its own overlay address deterministically from `(camp_id, pub)`
using SHA-256, and every other peer in the camp computes the *same*
address from the same inputs. Camp becomes a pure identity + endpoint
registry — no octet pool, no sticky-binding table, no `/24` cap.

Why this is worth doing:

1. **Security**. Today `peerToTunLoop` falls back to "identify peer by
   src tunnel_ip from the IP header" when the UDP source endpoint
   doesn't match any known `peerState.UDPAddr` (NAT-rebind recovery
   path). A malicious peer can put another peer's tunnel_ip into the
   `src` field of an IP packet and we'll happily update *that* peer's
   `UDPAddr` to the attacker's endpoint — endpoint hijack. With
   pub-derived addresses the receiver doesn't trust IP-header src at
   all: identity comes from UDP endpoint → known pub → computed v6
   address. Spoofing requires forging the UDP source endpoint, which
   is much harder.
2. **Architectural cleanliness**. Camp drops responsibility for
   address allocation. Its job shrinks to: "given a camp_id, here are
   the `(pub, name, endpoint)` triples currently announcing". The
   `peer_bindings` table can go away (or stay frozen as legacy).
3. **No /24 cap**. Today `LAST_HOST - FIRST_HOST = 253` peers per
   camp. IPv6 ULA gives 2^64 hosts per camp — effectively unbounded.
4. **Coordination removed**. Camp no longer arbitrates octet allocation;
   no race conditions; no "camp full" error.

### Mapping: how `(camp_id, pub)` becomes an IPv6 address

We use the IPv6 Unique Local Address range, `fc00::/7` (typically
`fd00::/8` for self-generated, RFC 4193). Its canonical layout splits
the 128-bit address into:

```
| 8 bits  | 40 bits   | 16 bits    | 64 bits          |
| 0xfd    | Global ID | Subnet ID  | Interface ID     |
```

We fill those fields like so:

```
| 8 bits  | 40 bits              | 16 bits  | 64 bits           |
| 0xfd    | sha256(camp_id)[:5]  | 0x0000   | sha256(pub)[:8]   |
| ULA     | per-camp /48 prefix  | reserved | per-pub host part |
```

Translated to bytes (16 bytes total):

```
byte 0:     0xfd                              (ULA marker)
bytes 1–5:  first 5 bytes of sha256(camp_id)  (camp's Global ID)
bytes 6–7:  0x00 0x00                         (subnet ID — reserved 0)
bytes 8–15: first 8 bytes of sha256(pub_raw)  (host derived from pub)
```

Worked example, with pub
`e2dca0d029098ea33875f557bf0f7ac3452092cce46ee9f4f7063fde2e0ab89c`
(32 raw bytes once hex-decoded):

```
sha256(pub_raw)        = 12f3478eae3148264df9c5e82cea1f11...
sha256("12345")        = 5994471abb...
sha256("testcamp")     = 5efbdf99ec...

In camp "12345":
  fd 59 94 47 1a bb 00 00 12 f3 47 8e ae 31 48 26
  → fd59:9447:1abb:0:12f3:478e:ae31:4826

In camp "testcamp":
  fd 5e fb df 99 ec 00 00 12 f3 47 8e ae 31 48 26
  → fd5e:fbdf:99ec:0:12f3:478e:ae31:4826
```

Note that the host part (`12f3:478e:ae31:4826`) is identical across
camps — it's derived purely from the pub. Only the `/48` camp prefix
changes. Same pub in a different camp = different address; different
pub in the same camp = different address. Two different pubs in the
same camp landing on the same address would require sha256 to collide
in its first 8 bytes (~ probability `10^-17` for any realistic camp
size; we ignore this as cryptographically impossible).

### What changes layer by layer

| Layer | Today | After |
|---|---|---|
| camp `PeerInfo` | carries `tunnel_ip: "10.99.0.X"` | drops `tunnel_ip` (or keeps unused) |
| camp DB `peer_bindings` | stores `octet` per `(camp_id, pub)` | not needed; can drop table |
| camp `hub.upsert` | allocates octet | no allocation logic at all |
| Mac `engine.applyPeerList` | reads `tunnel_ip` from PeerInfo | computes via `PubToAddr(camp_id, pub)` |
| Mac `peerState.TunnelIP` | string | becomes v6 string (or `netip.Addr`) |
| Mac `tunnel.OpenSubnet` | `ifconfig inet 10.99.0.1` + v4 route | `ifconfig inet6 fd…::self` + v6 route |
| Mac `packet.ExtractDst` | parses IPv4 header | parses IPv4 *and* IPv6 |
| Mac `peerToTunLoop` identification | UDP endpoint, fallback v4 src IP | UDP endpoint only — no IP-header trust |
| Mac DNS server | answers A | answers AAAA (drops the silent-empty-A AAAA reply) |
| Mac `pf` egress / firewall rules | v4 syntax | v6 syntax (pf supports both, but rule-strings change) |
| Intercepts UI | CIDRs like `192.168.1.0/24` | unchanged for *external* CIDRs — only the *peer* side becomes v6 |
| Wire protocol | IPv4 packets in UDP payload | IPv6 packets in UDP payload (transport unchanged, still v4 UDP) |

The outer transport layer **does not change**: we keep wrapping the
inner packet in UDP/IPv4 between nodes. This is standard 6in4-style
encapsulation (RFC 4213); the inner-vs-outer family mismatch is fine.

### Migration plan (staged, additive)

Designed so we never have a non-working intermediate state. Each step
is testable on its own.

**Step 0 — Foundation package, no integration.**
Add `source/mac/internal/overlay` with `PubToAddr(campID, pubHex) →
netip.Addr` and `CampPrefix(campID) → netip.Prefix`. Pure functions
plus tests with known vectors. No callers yet.

**Step 1 — Expose computed v6 in Status, parallel to existing v4.**
`engine.applyPeerList` writes `peerState.OverlayV6 = PubToAddr(...)`
alongside the current `TunnelIP`. Status JSON gains a `v6_address`
field per peer. UI shows it next to the tunnel_ip as a sanity check.
Nothing else uses it yet — pure observation.

**Step 2 — Dual-stack utun.**
`tunnel.OpenSubnet` brings up both v4 (existing) and v6 (new) on the
same utun interface. Both routes installed. Apps can choose; nothing
forces the switch. Verify v6 between two peers manually (`ping6
fd…::peer`).

**Step 3 — Switch peer-to-peer traffic to v6.**
`routeFor` looks up by v6 dst first; v4 dst still works (legacy fallback
during migration). DNS server starts returning AAAA. Browsers and apps
opening tunnel-side HTTPS gradually move to v6.

**Step 4 — Drop v4 from utun.**
Remove v4 address and route from utun. v4 inside the overlay no longer
works. Cleanup `packet.ExtractDst`, `routeFor`, etc. Status drops the
old `tunnel_ip` field entirely.

**Step 5 — Camp cleanup.**
Stop sending `tunnel_ip` in `PeerInfo`. Drop `peer_bindings` table
(or freeze it as legacy). `hub.upsert` becomes a stateless `(pub,
name, endpoint, last_seen)` upsert. Mac clients ignore `tunnel_ip`
if camp still sends it (already true after step 4, just cleanup).

### Open questions

- **Intercepts**. We route external CIDRs (e.g. `192.168.1.0/24`) to
  a peer for egress. In v6 overlay world the *peer's* address is v6,
  but the intercepted destination is still v4. We end up routing a v4
  packet into a v6 utun → packet shape conflict. Two options: keep v4
  inside utun *additionally* just for intercepts (dual-stack stays
  forever), or wrap v4 in v6 (encapsulate-twice; ugly). Probably
  dual-stack utun is the practical answer — v6 for peer-to-peer, v4
  for external CIDR egress.
- **`pf` rule complexity**. Current pf anchor templates are v4-only.
  Need to extend to emit v6 rules. Not hard but doubles the surface.
- **UI readability**. `fd59:9447:1abb:0:12f3:478e:ae31:4826` is
  uglier than `10.99.0.5`. UI should default to showing the short
  fingerprint pill (`fp 12f3478eae314826`) and fold the full address
  into a tooltip.
- **Self-IP picking**. Today camp gives us our own tunnel_ip in the
  announce reply. With derived addresses we compute it ourselves
  from `(camp_id, our_pub)`. Sanity check: hub no longer needs to
  reply with `tunnel_ip` in `PeerInfo.you`.

### Out of scope

- Adoption inside any specific service (Gitea, Vaultwarden, etc).
  Once v6 is the overlay, services bound to `[::]` work automatically;
  v4-only services keep working via dual-stack until step 4.
- Re-targeting public DNS like `*.f2f` outside the camp. The overlay
  is private; nothing on the open internet should resolve our v6.

### Scope

Roughly:

- Step 0: ~50 lines + tests.
- Step 1: ~30 lines (engine, status, UI).
- Step 2: ~80 lines (tunnel package, dual-stack ifconfig + route).
- Step 3: ~100 lines (routeFor v6 path, DNS AAAA, switching default).
- Step 4: ~50 lines of removal.
- Step 5: ~30 lines camp-side, ~30 lines mac-side cleanup.

About a month of focused work end-to-end. Steps 0 and 1 can land
without committing to the rest — they're pure additions and give us
the foundation for later moves.

## Camp identity — separate internal id (creator_pub) from display label

Goal: stop using a free-form human-typed string as the camp's
internal identifier. Two people who independently pick `family` as
their camp name *currently* end up in the same camp on the server —
silent collision, no warning. Fix by splitting camp identity into two
fields: an unforgeable cryptographic id (the creator's pub) and a
mutable human-readable label used only for display and DNS.

This is independent of the overlay-IPv6 work above but interacts with
it: the IPv6 derivation feeds `camp_id` into `sha256(camp_id)[:5]`, so
the higher the entropy of `camp_id`, the cleaner the prefix space.
With creator_pub as `camp_id` we get 256 bits of entropy — collision
across camps is cryptographically impossible.

### Current state (what's broken)

- `camp_id` is a free-form string the user types: `12345`,
  `family`, `production-mesh-2026`. No central registry.
- camp-server accepts any well-formed string as a camp namespace.
- Two users picking the same string → same camp on the server → they
  see each other's pubs, peer endpoints, etc.
- We already generate an Ed25519 `Identity` per camp on the Mac side
  (`internal/identity`) and persist it to `/var/lib/f2f/identity/<camp_id>/`,
  but the camp creator's pub is not the camp's identity in any way —
  it's just per-peer identity within an otherwise-anonymous camp.

### Target model

```
camp:
  id    = <creator_pub_hex>           # 64 hex chars, cryptographically unique, immutable
  label = "family"                    # mutable, for DNS and UI, no uniqueness guarantee
```

Two camps can share `label` ("family") freely — they're disambiguated
by `id`. The id is what camp-server stores as the namespace key, and
what `(camp_id, pub)` keys the `peer_bindings` table by.

Where each one is used:

| Use site | Field | Why |
|---|---|---|
| camp-server `/api/id/:id` | `camp_id` (creator_pub) | server-side namespace key |
| DB `peer_bindings` row | `(camp_id, pub)` | unique row per peer per camp |
| Local DNS zone | `<label>.f2f` | humans type `gitea.family.f2f` |
| `/etc/resolver/<label>.f2f` | `label` | macOS resolver routing |
| IPv6 camp prefix (when migrated) | `sha256(camp_id)[:5]` | high-entropy input → uniform prefix |
| Invite token body | `camp_id` (creator_pub) | invite is signed by creator; signature verifies because verifier knows creator_pub |
| UI "my camps" list | `label` + first 16 hex of fingerprint | human-friendly with disambiguation |

### Properties this gives us

- **Two camps with same `label` on the same machine** — works fine
  as long as you're connected to at most one at a time (DNS zone
  conflicts otherwise). UI distinguishes by fingerprint.
- **Renaming the camp is free** — `label` changes, `camp_id` doesn't,
  no one loses access. DNS zone changes; peers re-write their
  `/etc/resolver/<label>.f2f` on the next status poll.
- **Camp ownership is concrete** — the holder of `creator_priv` is
  the owner. Loses key → loses camp (unless we layered `admin_pubs[]`
  on top, see Identity & access control section above).
- **Invite tokens are verifiable end-to-end** — invitee receives
  `{camp_id, sig}`, knows `camp_id == creator_pub`, verifies signature
  directly. No camp-side TOFU required.

### What changes layer by layer

| Layer | Today | After |
|---|---|---|
| `engine.CampConfig` | one field `ID` (string) | `ID` (creator_pub hex), `Label` (string), `IsOwner` (bool) |
| `config_store` | persists `CampID` | persists `CampID`, `Label`, `IsOwner`, `CreatorPriv` (if owner) |
| camp creation UI | user types camp_id | user types `label` only; client generates creator-priv and uses pub as camp_id |
| camp-server validation | `NAME_RE` allows `[A-Za-z0-9_.-]+` | for new camps: must be 64 hex; for legacy: keep current regex during transition |
| `internal/dns` | zone = `<camp_id>.f2f` | zone = `<label>.f2f` (label-from-config) |
| UI camp picker | shows `id (name)` | shows `label · fp <first 16 hex>` |
| Invite token | references `camp_id` (string) | references `camp_id` (creator_pub) — same field name, stricter content |

### Migration plan

This is the same kind of staged plan as the IPv6 work above, but
with explicit decisions on legacy.

**Step 0 — Decision on legacy camps.** Camps created before this
change have no creator_pub. Three options were considered:

- (a) Auto-promote: prompt "make yourself owner of this legacy camp",
  generate creator-pub locally, push the new id to camp-server,
  notify other peers via the next poll. Requires server-side support
  for "rename camp id" which is a non-trivial operation.
- (b) Coexistence: legacy camps live forever with a `legacy: true`
  flag, no owner features. Adds permanent branches in code.
- **(c) Force recreation**: tell users to recreate their camps with
  the new flow. Old camps still work for joined peers but UI nudges
  them to migrate.

Choice: **(c)** — at the current scale (single-digit camps total,
all owned by us / Vsevolod), the cheapest path is to recreate. Old
camps stay reachable; new camps are crypto-id from the start.

**Step 1 — Schema extension on the Mac side.**
- `Camp.Label` field in `internal/config`.
- camp creation UI splits into "label" + "this will create your
  identity" + "save my creator-priv".
- Backward compat: if persisted config has only `ID` and ID is not
  64-hex, treat it as legacy: `Label = ID`, no creator-priv.

**Step 2 — Camp-server accepts creator-pub-shaped camp_ids.**
- camp-server validator: accept anything matching `NAME_RE` (legacy)
  OR 64-hex (new). Hub doesn't care about the format; it's just
  a string key.
- No DB schema change — `peer_bindings.camp_id` is already `TEXT`.

**Step 3 — DNS zone uses label.**
- `internal/dns` zone constructed from `Camp.Label`, not `Camp.ID`.
- `/etc/resolver/<label>.f2f` resolver file (replaces
  `<camp_id>.f2f`).
- On migration: when engine starts and detects `Label != legacy ID`,
  remove the old `<camp_id>.f2f` resolver file alongside writing the
  new one.

**Step 4 — Invite tokens carry creator_pub.**
- `identity.GenerateInvite` already takes `campID` — caller passes
  creator_pub instead of free-form id.
- camp-server verifies `invite.body.camp_id == invite_creator_pub`,
  then verifies `sig` over the invite body.
- Doesn't activate the invite-only flow itself — see Identity TODO
  Phase 2.

**Step 5 — Deprecate camp creation with free-form id in UI.**
- UI no longer offers "type any string as camp id".
- Legacy camps remain visible and usable, marked as such.

### Open questions

- **Single-camp-at-a-time vs multi-camp**: today the engine binds to
  one camp via `Camp` (single pointer). If we add multi-camp support
  later, DNS conflicts on `<label>.f2f` between two camps you're in
  simultaneously become a real problem. Solution then: include
  fingerprint in zone (`family-12f3478e.f2f`). Postpone until
  multi-camp is on the table.
- **QR / invite-link format**: 64-hex creator_pub is ugly to paste.
  Likely we want a base32-encoded shorter form for sharing as URL
  (`f2f://camp/<base32_creator_pub>?label=family`). Format design
  separate from the rest.
- **Owner key rotation**: if creator-priv leaks or is lost, camp is
  effectively dead. Need `admin_pubs[]` (Identity TODO Phase 1) for
  rescue. Out of scope here.
- **camp_id length in URLs and logs**: camp_id is now 64 chars
  everywhere it used to be a 5-10 char human name. Log lines like
  `camp: peer X joined camp <64 hex>` are unfriendly. Use short
  fingerprint in log output (first 16 hex), full pub only when
  needed for debugging.

### Scope

- Step 1 (Mac config extension): ~50 lines + UI tweaks.
- Step 2 (server validator): ~5 lines.
- Step 3 (DNS zone): ~20 lines.
- Step 4 (invite verify): ~30 lines mac + ~20 lines server.
- Step 5 (UI cleanup): ~20 lines.

About a long evening / weekend's work end-to-end, much cheaper than
the IPv6 migration because no in-flight wire format changes. Logically
sequenced ahead of IPv6 (camp_id becomes high-entropy first, then
overlay derivation rides on top).
