# f2f

Friend-to-friend: your own self-hosted P2P overlay. A private encrypted mesh
between your machines — with DNS, an HTTPS reverse proxy for `*.f2f` apps, file
sharing, calls, a remote terminal, and a built-in identity provider — with no
cloud in the middle. Peers find each other through a tiny rendezvous server and
connect directly over an AmneziaWG (WireGuard) tunnel.

Cross-platform: macOS (Apple Silicon), Linux (amd64/arm64), Windows (amd64).

## Install

Downloads the latest release binary and installs it.

**macOS / Linux**
```sh
curl -fsSL https://raw.githubusercontent.com/vseplet/f2f/main/install.sh | sh
```
Installs to `/usr/local/bin/f2f` (asks for sudo only to write there). On a
network with broken IPv6, prefix with `-4`:
```sh
curl -4 -fsSL https://raw.githubusercontent.com/vseplet/f2f/main/install.sh | sh
```
Pin a version or change the location with env vars:
```sh
F2F_VERSION=v0.2.0 F2F_BIN_DIR=~/.local/bin curl -fsSL .../install.sh | sh
```

**Windows** (PowerShell)
```powershell
irm https://raw.githubusercontent.com/vseplet/f2f/main/install.ps1 | iex
```
Installs `f2f.exe` + `wintun.dll` to `%LOCALAPPDATA%\f2f` and adds it to your
PATH. Run from a PowerShell opened **as Administrator** (creating the tunnel
adapter needs it). Pin a version with `-Version v0.2.0`.

The tunnel needs privileges everywhere: run f2f with `sudo` on macOS/Linux and
as Administrator on Windows.

## Run modes

f2f runs in one of three modes:

| command | mode | what it is |
|---|---|---|
| `sudo f2f` | **portal** | interactive; the human workstation with the web UI on `http://127.0.0.1:2202`. Create or join a camp from the picker. |
| `sudo f2f --service --camp <id> --name <n>` | **service** | headless, long-lived node (a server). No web UI. |
| `sudo f2f --task --camp <id> --name <n> --key <k>` | **task** | ephemeral, short-lived node (e.g. a CI runner). Substrate only. |

Bare `f2f` opens the interactive portal — create a new camp or join an existing
one, then it comes up. `--service`/`--task` bring up a specific camp
non-interactively.

Common flags (any mode):
- `--logs` — verbose (debug) logging; also mirrors it to the console.
- `--console` — mirror logs to the terminal (they go to a file by default).
- `--bind <addr>` — HTTP bind for the portal UI (default `127.0.0.1:2202`).

`f2f version` prints the build version.

## Camps

A **camp** is your private mesh group. Its `camp_id` is a bearer secret: anyone
who has it can join, so share it only with your own devices / invited peers.
Create and join camps from the interactive portal (bare `f2f`).

## Build from source

```sh
cd source/helper
CGO_ENABLED=0 go build -o f2f .   # pure Go, cross-compiles to any target
```
Releases are cut by tagging `v*` on `main` (see `.github/workflows/release.yml`).

## License

See [LICENSE](LICENSE).
