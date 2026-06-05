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

// TestTerminateProcessGracefullyKillsPagerProcessGroup demonstrates the bug
// from issue #5675: when lazygit runs a git command via PTY (`Setsid`,
// creating a new session where child PID = PGID), and git spawns the pager
// (e.g. less) as a subprocess in the same process group, the current
// TerminateProcessGracefully only signals the direct child (git), not the
// process group. Git exits, but the pager survives.
//
// This test simulates that scenario:
//   - A "git" process in a new process group
//   - A "pager" subprocess in the same group that ignores SIGTERM
//   - TerminateProcessGracefully should signal the process group so that
//     the pager is also terminated
//
// EXPECTED: the pager subprocess is dead after TerminateProcessGracefully.
// CURRENT (buggy): the pager survives because only the direct child PID
// received SIGTERM.
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

	// Act: call the current TerminateProcessGracefully.
	// This sends SIGTERM to the direct child PID (pgid) only, NOT to
	// the process group. The "git" process exits (it traps SIGTERM),
	// but the "pager" ignores SIGTERM and survives.
	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	// The "git" process traps SIGTERM and exits immediately. Wait for reaping.
	_, err = cmd.Process.Wait()
	assert.NoError(t, err)

	// Assert: the pager subprocess SHOULD be dead.
	// Signal(0) probes process existence: nil = alive, ESRCH = dead.
	err = syscall.Kill(pagerPid, syscall.Signal(0))
	assert.ErrorIs(t, err, syscall.ESRCH,
		"BUG: pager subprocess (PID %d) survived TerminateProcessGracefully. "+
			"SIGTERM was sent only to the direct child (PID %d), not to the "+
			"process group (PGID %d). The fix should signal the process group "+
			"(kill(-pid, SIGHUP/SIGTERM)) and fall back to SIGKILL after a "+
			"short timeout.",
		pagerPid, pgid, pgid)
}
