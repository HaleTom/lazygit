//go:build !windows

package tasks

import (
	"os/exec"
	"syscall"
)

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid == pid {
		// The process is a process-group leader (e.g., PTY with Setsid).
		// Kill the entire group to catch grandchildren like less/highlight.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	} else {
		// Not a group leader (non-PTY command). Kill the child directly.
		_ = cmd.Process.Kill()
	}
}
