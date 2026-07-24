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
//
// groups is the user's FULL supplementary group list. It must be passed: with
// an empty Credential.Groups Go calls setgroups(0), wiping every supplementary
// group — which strips membership of admin/wheel and so breaks group-gated
// access (including sudo). When groups is empty we keep the inherited set
// (NoSetGroups) rather than clearing it.
func SetProcCredential(c *exec.Cmd, uid, gid int, groups []int) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if len(groups) > 0 {
		cred.Groups = make([]uint32, len(groups))
		for i, g := range groups {
			cred.Groups[i] = uint32(g)
		}
	} else {
		cred.NoSetGroups = true
	}
	c.SysProcAttr.Credential = cred
}

// KillProcessGroup SIGKILLs the whole process group led by pid, so a shell and
// everything it spawned go away together. A negative pid addresses the group.
func KillProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
