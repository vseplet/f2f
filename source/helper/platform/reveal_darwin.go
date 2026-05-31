//go:build darwin

package platform

import (
	"os"
	"os/exec"
)

// RevealInFileManager asks the user's GUI shell to highlight the
// file at path. On macOS this is `open -R`, which spawns Finder.
//
// The helper runs as root via sudo, but `open` only works inside a
// user GUI session, so we drop privileges to the invoking user
// (SUDO_USER) when present. Falls through to a raw call when not
// running under sudo (unusual but valid).
func RevealInFileManager(path string) error {
	var cmd *exec.Cmd
	if su := os.Getenv("SUDO_USER"); su != "" {
		cmd = exec.Command("/usr/bin/sudo", "-u", su, "/usr/bin/open", "-R", path)
	} else {
		cmd = exec.Command("/usr/bin/open", "-R", path)
	}
	return cmd.Start()
}
