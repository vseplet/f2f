"""HTTP-level smoke tests for `f2f-mac ui`.

These do NOT run the engine (no sudo, no utun). They cover:
- the embedded SPA is served correctly
- the JSON API is reachable and returns sane shapes
- engine mutation endpoints reject calls when the engine is stopped
- SSE log stream connects and produces an initial event
"""

from __future__ import annotations

import json

import httpx


def test_status_initial_state(server: str) -> None:
    r = httpx.get(f"{server}/api/status")
    assert r.status_code == 200
    data = r.json()
    assert data["running"] is False
    assert data["egress_active"] is False
    assert data["intercepts"] == []
    assert data["tx_packets"] == 0
    assert data["rx_packets"] == 0


def test_index_html_served(server: str) -> None:
    r = httpx.get(f"{server}/")
    assert r.status_code == 200
    assert "text/html" in r.headers["content-type"]
    assert "f2f-mac" in r.text
    # The SPA loads both vendored libraries.
    assert "/vendor/tailwind.min.js" in r.text
    assert "/vendor/jquery.min.js" in r.text


def test_app_js_served(server: str) -> None:
    r = httpx.get(f"{server}/app.js")
    assert r.status_code == 200
    assert r.text.startswith("$(function")


def test_jquery_vendored(server: str) -> None:
    r = httpx.get(f"{server}/vendor/jquery.min.js")
    assert r.status_code == 200
    assert "jQuery" in r.text or "jquery" in r.text.lower()
    assert len(r.content) > 50_000


def test_tailwind_vendored(server: str) -> None:
    r = httpx.get(f"{server}/vendor/tailwind.min.js")
    assert r.status_code == 200
    # Tailwind play CDN script is large — sanity-check size.
    assert len(r.content) > 100_000


def test_ifaces_returns_list(server: str) -> None:
    r = httpx.get(f"{server}/api/ifaces")
    assert r.status_code == 200
    data = r.json()
    assert isinstance(data, list)
    # Even on a CI box there should be at least one non-loopback interface.
    for iface in data:
        assert "name" in iface
        assert not iface["name"].startswith("utun")
        assert not iface["name"].startswith("lo")


def test_add_intercept_when_stopped_fails(server: str) -> None:
    r = httpx.post(f"{server}/api/intercepts", json={"spec": "1.1.1.1"})
    assert r.status_code == 400
    body = r.json()
    assert "error" in body
    assert "running" in body["error"].lower()


def test_remove_intercept_when_stopped_fails(server: str) -> None:
    r = httpx.delete(f"{server}/api/intercepts/i1")
    # Engine refuses with "not running"; we map that to 404 in the handler.
    assert r.status_code in (400, 404)
    assert "error" in r.json()


def test_stop_when_already_stopped_is_idempotent(server: str) -> None:
    r = httpx.post(f"{server}/api/stop")
    assert r.status_code == 200
    data = r.json()
    assert data["running"] is False


def test_start_with_bad_config_returns_error(server: str) -> None:
    # listen without peer must be rejected.
    r = httpx.post(
        f"{server}/api/start",
        json={
            "local_ip": "10.99.0.1",
            "peer_ip": "10.99.0.2",
            "listen": ":9999",
            "peer": "",
        },
    )
    assert r.status_code == 400
    assert "error" in r.json()


def test_start_without_root_fails_cleanly(server: str) -> None:
    # We are not root in tests, so utun creation fails. The engine returns an
    # error and the server propagates it. State must stay "stopped".
    r = httpx.post(
        f"{server}/api/start",
        json={
            "local_ip": "10.99.0.1",
            "peer_ip": "10.99.0.2",
        },
    )
    assert r.status_code == 400
    assert "error" in r.json()

    # And status confirms nothing came up.
    r = httpx.get(f"{server}/api/status")
    assert r.json()["running"] is False


def test_log_stream_emits_connected_comment(server: str) -> None:
    with httpx.Client(timeout=2.0) as client, client.stream(
        "GET", f"{server}/api/log/stream"
    ) as r:
        assert r.status_code == 200
        assert "text/event-stream" in r.headers["content-type"]
        buf = ""
        for chunk in r.iter_text():
            buf += chunk
            if ": connected" in buf:
                break
            if len(buf) > 2000:
                break
        assert ": connected" in buf


def test_status_json_is_valid(server: str) -> None:
    """Defensive: encoding/json should never produce something that breaks
    json.loads (catches accidental NaN/Inf in counters)."""
    r = httpx.get(f"{server}/api/status")
    assert r.status_code == 200
    # raises if invalid
    json.loads(r.text)
