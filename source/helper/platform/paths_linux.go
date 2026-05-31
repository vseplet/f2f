//go:build linux

package platform

import "path/filepath"

// AppSupportDir returns the conventional per-user directory for
// application support data, given the invoking user's home. On
// Linux this is ~/.local/share/ per the XDG Base Directory Spec
// (XDG_DATA_HOME defaults to $HOME/.local/share). We don't read the
// XDG env var because we run as root via sudo — the SUDO_USER's
// env is not preserved.
func AppSupportDir(home string) string {
	return filepath.Join(home, ".local", "share")
}
