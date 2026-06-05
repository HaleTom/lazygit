//go:build !windows

package oscommands

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

func GetPlatform() *Platform {
	shell := getUserShell()

	prefixForShellFunctionsFile := ""
	if strings.HasSuffix(shell, "bash") {
		prefixForShellFunctionsFile = "shopt -s expand_aliases\n"
	}

	return &Platform{
		OS:                          runtime.GOOS,
		Shell:                       shell,
		ShellArg:                    "-c",
		PrefixForShellFunctionsFile: prefixForShellFunctionsFile,
		OpenCommand:                 "open {{filename}}",
		OpenLinkCommand:             "open {{link}}",
	}
}

func getUserShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}

	return "bash"
}

func (c *OSCommand) UpdateWindowTitle() error {
	return nil
}

func TerminateProcessGracefully(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}

	// Preserve original behavior: signal the direct child
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}

	// Signal the process group to kill subprocesses (e.g. less the pager).
	// When commands are started via PTY (creack/pty.StartWithSize), Setsid
	// creates a new session where the child PID is also the process group ID.
	// Subprocesses like less inherit the same PGID.
	//
	// ESRCH means no separate PG exists (non-PTY command) — harmless.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); err != nil && err != syscall.ESRCH {
		return err
	}

	return nil
}
