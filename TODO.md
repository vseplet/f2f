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
