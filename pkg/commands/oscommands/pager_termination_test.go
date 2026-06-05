//go:build !windows

package oscommands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

// TestTerminateProcessGracefullyAlreadyDead verifies that calling
// TerminateProcessGracefully on a process that has already exited
// handles the error gracefully — no crash or panic.
func TestTerminateProcessGracefullyAlreadyDead(t *testing.T) {
	cmd := exec.Command("bash", "-c", "exit 0")
	assert.NoError(t, cmd.Start())
	_, _ = cmd.Process.Wait()

	// Process is already dead — both signals will fail with ESRCH
	err := TerminateProcessGracefully(cmd)
	assert.Error(t, err)
}
