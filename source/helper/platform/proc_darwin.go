//go:build darwin

package platform

import (
	"os/exec"
	"syscall"
)

// SetProcCredential makes cmd start as uid/gid instead of inheriting our own
// (root, since f2f runs under sudo). Privilege reduction only — it can't raise
// privileges. Leaves the rest of SysProcAttr alone so callers that add their
// own flags afterwards (pty.Start sets Setsid/Setctty) still work.
func SetProcCredential(c *exec.Cmd, uid, gid int) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
}

// KillProcessGroup SIGKILLs the whole process group led by pid, so a shell and
// everything it spawned go away together. A negative pid addresses the group.
func KillProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
