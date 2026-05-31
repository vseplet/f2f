//go:build linux

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
)

// RevealInFileManager opens the file's containing directory in the
// user's file manager. Unlike macOS's `open -R`, xdg-open has no
// "highlight this file" mode — opening the parent dir is the
// closest available behavior.
//
// As on macOS, the helper runs as root via sudo and the file
// manager needs the user's session (DISPLAY/WAYLAND_DISPLAY); drop
// privileges via SUDO_USER when present.
func RevealInFileManager(path string) error {
	dir := filepath.Dir(path)
	var cmd *exec.Cmd
	if su := os.Getenv("SUDO_USER"); su != "" {
		cmd = exec.Command("sudo", "-u", su, "xdg-open", dir)
	} else {
		cmd = exec.Command("xdg-open", dir)
	}
	return cmd.Start()
}
