// Package selfupdate replaces the running f2f binary in place with the latest
// GitHub release build for this OS/arch. It downloads the release asset, sanity-
// checks it (the new binary must print the expected version), and atomically
// renames it over os.Executable(). It deliberately does NOT restart the process
// — swapping the on-disk file is safe while running, but tearing down a live
// tunnel to re-exec is environment-specific (systemd/launchd/foreground), so the
// caller is told to restart to apply.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repo = "vseplet/f2f"

// Result reports the outcome of an update attempt.
type Result struct {
	From        string `json:"from"`                   // version we were running
	To          string `json:"to"`                     // latest release tag
	Updated     bool   `json:"updated"`                // false = already latest (no swap)
	Path        string `json:"path,omitempty"`         // binary that was replaced
	RestartHint string `json:"restart_hint,omitempty"` // how to apply the swap
}

// assetName is the release asset for this platform (matches .github/release.yml).
func assetName() (string, error) {
	if runtime.GOOS == "windows" {
		// The windows asset is a zip (f2f.exe + wintun.dll); in-place swap of a
		// running .exe on Windows is also blocked. Out of scope for now.
		return "", fmt.Errorf("self-update not supported on %s yet", runtime.GOOS)
	}
	return fmt.Sprintf("f2f-%s-%s", runtime.GOOS, runtime.GOARCH), nil
}

// latestTag asks GitHub for the newest release tag (e.g. "v0.4.1").
func latestTag(ctx context.Context) (string, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases: %s", resp.Status)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Tag == "" {
		return "", fmt.Errorf("no tag_name in release")
	}
	return body.Tag, nil
}

// sameVersion compares a running version to a release tag, tolerating the "v"
// prefix. A "dev"/empty current version always counts as out-of-date.
func sameVersion(current, tag string) bool {
	c := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if c == "" || c == "dev" {
		return false
	}
	return c == strings.TrimPrefix(strings.TrimSpace(tag), "v")
}

// Apply checks for a newer release and, if found, downloads and swaps the
// binary. current is the running build version (main.version). A nil error with
// Updated=false means we're already on the latest tag.
func Apply(ctx context.Context, current string) (*Result, error) {
	asset, err := assetName()
	if err != nil {
		return nil, err
	}
	tag, err := latestTag(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{From: current, To: tag}
	if sameVersion(current, tag) {
		return res, nil // up to date
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)

	// Download the asset to a temp file in the SAME dir (so the final rename is
	// an atomic same-filesystem swap, even over the running binary).
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", asset, resp.Status)
	}

	tmp, err := os.CreateTemp(dir, ".f2f-update-*")
	if err != nil {
		return nil, fmt.Errorf("temp in %s (writable?): %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return nil, err
	}

	// Sanity check: the fresh binary must run and report the tag we expect. This
	// catches a truncated/HTML-error download before we clobber the real binary.
	out, err := exec.CommandContext(ctx, tmpPath, "version").Output()
	if err != nil {
		return nil, fmt.Errorf("downloaded binary failed to run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); !sameVersion(got, tag) {
		return nil, fmt.Errorf("downloaded binary reports %q, expected %s", got, tag)
	}

	if err := os.Rename(tmpPath, exe); err != nil {
		return nil, fmt.Errorf("replace %s: %w", exe, err)
	}
	cleanup = false

	res.Updated = true
	res.Path = exe
	res.RestartHint = "restart the helper to apply (e.g. systemctl restart <unit>, or stop and re-run)"
	return res, nil
}
