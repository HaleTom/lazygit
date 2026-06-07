//go:build !windows

package oscommands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
)

// TestTerminateProcessGracefullyKillsPagerProcessGroup demonstrates the fix
// for issue #5675: when lazygit runs a git command via PTY (`Setsid`,
// creating a new session where child PID = PGID), and git spawns the pager
// (e.g. less) as a subprocess in the same process group,
// TerminateProcessGracefully now signals the process group so the pager is
// also terminated.
//
// Test scenario:
//   - A "git" process in a new process group
//   - A "pager" subprocess in the same group that ignores SIGTERM
//   - TerminateProcessGracefully sends SIGTERM to the PID (original behavior)
//     AND SIGHUP to the process group (so the pager is also killed)
//
// Without the fix, the pager survives as an orphan. With the fix, both die.
func TestTerminateProcessGracefullyKillsPagerProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pager.pid")

	// Create a "git" process (bash) in a new process group.
	// It spawns a "pager" subprocess that ignores SIGTERM (like a broken less).
	// Both are in the same PG — bash runs background jobs in the same group
	// when non-interactive.
	cmd := exec.Command("bash", "-c", fmt.Sprintf(`
		set -e
		# Simulate git: trap SIGTERM and exit cleanly
		trap 'exit 0' SIGTERM

		# Simulate the pager (less): a subprocess that ignores SIGTERM
		(
			trap '' SIGTERM SIGINT
			while true; do sleep 1; done
		) &
		pager_pid=$!
		echo $pager_pid > %s
		wait
	`, pidFile))

	// Start in a new process group, just like PTY.StartWithSize does via Setsid
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	assert.NoError(t, cmd.Start())
	if !assert.NotZero(t, cmd.Process.Pid, "process should have a PID") {
		return
	}

	pgid := cmd.Process.Pid

	// Cleanup: kill the process group if the test fails
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()

	// Read the pager's PID from the file the child wrote
	var pagerPid int
	for i := 0; i < 100; i++ {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			fmt.Sscanf(string(data), "%d", &pagerPid)
			if pagerPid > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !assert.Greater(t, pagerPid, 0, "pager subprocess should have started") {
		return
	}
	if !assert.NotEqual(t, pgid, pagerPid, "pager should be a different process") {
		return
	}

	// Act: call TerminateProcessGracefully.
	// The fix sends SIGTERM to the PID (original behavior) AND SIGHUP to the
	// process group. The "git" process dies from SIGTERM (via trap). The
	// "pager" ignores SIGTERM but receives SIGHUP from the process-group
	// signal and dies.
	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	// The "git" process exits and is reaped
	_, err = cmd.Process.Wait()
	assert.NoError(t, err)

	// Assert: the pager subprocess is dead.
	// Signal(0) probes process existence: nil = alive, ESRCH = dead.
	err = syscall.Kill(pagerPid, syscall.Signal(0))
	assert.ErrorIs(t, err, syscall.ESRCH,
		"pager subprocess (PID %d) survived. SIGTERM killed the parent (PID %d), "+
			"but SIGHUP to the PG (PGID %d) should have killed the pager.",
		pagerPid, pgid, pgid)
}

// TestTerminateProcessGracefullyNonPty verifies that for commands started
// without a separate process group (no PTY, child inherits parent's PGID),
// the original SIGTERM-to-PID behavior is preserved. The PG signal
// (kill(-pid, SIGHUP)) returns ESRCH since the PID is not a valid PGID,
// and that error is safely ignored.
func TestTerminateProcessGracefullyNonPty(t *testing.T) {
	cmd := exec.Command("bash", "-c", `
		trap 'exit 0' SIGTERM
		while true; do sleep 1; done
	`)

	assert.NoError(t, cmd.Start())

	// Cleanup: kill the process if the test fails
	defer func() {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	}()

	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	// Wait for the process to be reaped
	_, _ = cmd.Process.Wait()

	// Signal(0) probes process existence: nil = alive, ESRCH = dead.
	err = syscall.Kill(cmd.Process.Pid, syscall.Signal(0))
	assert.ErrorIs(t, err, syscall.ESRCH,
		"process should be dead from SIGTERM (non-PTY, no separate PG)")
}

// TestPtyCloseLeavesOrphans demonstrates the bug described in GNU_less.md:
// when a pty master is closed, the kernel sends SIGHUP to the foreground
// process group. If a process in that group ignores SIGHUP (like a broken less
// whose signal handler deadlocks before reaching _exit()), it survives as an
// orphan and spins at ~99% CPU.
//
// This test starts a "pager" process that ignores SIGHUP (simulating broken
// less) in a pty using the same creack/pty package lazygit uses, then closes
// the pty master. Without lazygit's cleanup fix (timeout + SIGKILL fallback),
// the pager survives — just like a broken less after pty close.
//
// The test asserts the CURRENT (buggy) behavior: the pager is still alive
// after the pty closes. The EXPECTED behavior is that the pager should be
// killed by SIGKILL after the timeout.
func TestPtyCloseLeavesOrphans(t *testing.T) {
	// Simulate broken less: a pager that ignores SIGHUP (signal handler
	// deadlocked and never reached _exit()).
	pidFile := filepath.Join(t.TempDir(), "pager.pid")

	cmd := exec.Command("bash", "-c", fmt.Sprintf(`
		set -e
		# Simulate git: traps SIGTERM and exits
		trap 'exit 0' SIGTERM

		# Simulate broken less: a subprocess that ignores SIGHUP
		(
			trap '' SIGHUP SIGINT
			while true; do sleep 1; done
		) &
		pager_pid=$!
		echo $pager_pid > %s
		wait
	`, pidFile))

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	assert.NoError(t, err)
	defer ptmx.Close()

	pgid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()

	// Wait for pager to start
	var pagerPid int
	for i := 0; i < 100; i++ {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pagerPid)
			if pagerPid > 0 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !assert.Greater(t, pagerPid, 0, "pager subprocess should have started") {
		return
	}

	// Close the pty master. Kernel sends SIGHUP to the foreground PG.
	// The "git" process (bash) receives SIGHUP but also gets SIGTERM.
	// The "pager" ignores SIGHUP — simulating a broken less whose signal
	// handler deadlocked.
	ptmx.Close()

	// Send SIGTERM to the direct child (git). This is what lazygit does
	// after closing the pty.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_, _ = cmd.Process.Wait()

	// Give signals time to deliver
	time.Sleep(300 * time.Millisecond)

	/* EXPECTED:
	// The pager should be dead. LazyGit's cleanup should escalate to
	// SIGKILL after the timeout since SIGHUP was ignored.
	err := syscall.Kill(pagerPid, syscall.Signal(0))
	assert.ErrorIs(t, err, syscall.ESRCH,
		"pager should be killed by timeout+SIGKILL fallback")
	ACTUAL: */
	err = syscall.Kill(pagerPid, syscall.Signal(0))
	assert.NoError(t, err,
		"BUG: pager (PID %d) survives after pty master closes. "+
			"The kernel's SIGHUP was ignored (simulating broken less with "+
			"deadlocked signal handler). Without lazygit's timeout+SIGKILL "+
			"fallback, this process spins at ~99%% CPU indefinitely. "+
			"See GNU_less.md for the root cause and fix.", pagerPid)
}
