"""Shared fixtures for f2f-mac UI tests.

The tests cover the HTTP surface only — they don't exercise utun / pf, which
need sudo and real hardware. The engine is expected to be in its initial
(not-running) state for these tests.

Run with: cd tests && uv run pytest -v
"""

from __future__ import annotations

import os
import pathlib
import socket
import subprocess
import time

import httpx
import pytest

PROJECT_DIR = pathlib.Path(__file__).resolve().parent.parent
BINARY = PROJECT_DIR / "f2f-mac"


@pytest.fixture(scope="session", autouse=True)
def build_binary() -> None:
    """Make sure a fresh binary is on disk before any tests run."""
    subprocess.check_call(
        ["go", "build", "-o", str(BINARY), "./cmd/f2f-mac"],
        cwd=PROJECT_DIR,
    )


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture()
def server():
    """Spawn `f2f-mac ui` on a free port and yield its base URL.

    The fixture is function-scoped — each test gets a fresh process so the
    engine state is always known to be empty/stopped at the start of the test.
    """
    port = _free_port()
    base = f"http://127.0.0.1:{port}"
    env = os.environ.copy()
    proc = subprocess.Popen(
        [str(BINARY), "ui", "--bind", f"127.0.0.1:{port}"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        env=env,
    )

    deadline = time.time() + 5.0
    while time.time() < deadline:
        try:
            r = httpx.get(f"{base}/api/status", timeout=0.3)
            if r.status_code == 200:
                break
        except httpx.HTTPError:
            pass
        time.sleep(0.05)
    else:
        proc.kill()
        out, _ = proc.communicate(timeout=2)
        raise RuntimeError(f"f2f-mac ui did not become ready:\n{out}")

    try:
        yield base
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=2)
