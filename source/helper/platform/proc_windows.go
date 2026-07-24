//go:build windows

package platform

import (
	"os/exec"
	"strconv"
)

// SetProcCredential is a no-op on Windows: there is no setuid model, and a
// child process inherits the caller's token. f2f isn't "root dropping to the
// human" here — it already runs as the invoking user's account, which is the
// property the Unix path is emulating.
func SetProcCredential(c *exec.Cmd, uid, gid int, groups []int) {}

// KillProcessGroup terminates pid and everything it spawned. Windows has no
// process groups addressable by a negative pid, so use taskkill's /T (tree)
// with /F (force) — the closest equivalent to SIGKILL on a process group.
func KillProcessGroup(pid int) error {
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
