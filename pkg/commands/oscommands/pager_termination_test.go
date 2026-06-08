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

// TestTerminateProcessGracefullyKillsPagerProcessGroup verifies that
// TerminateProcessGracefully sends SIGTERM only to the direct child.
// Subprocesses (like less) are killed by the kernel's SIGHUP when ptmx.Close()
// is called, not by this function.
//
// Test scenario:
//   - A "git" process in a new process group
//   - A "pager" subprocess in the same group that ignores SIGTERM
//   - TerminateProcessGracefully sends SIGTERM to the PID only
//   - The "git" process dies from SIGTERM (via trap)
//   - The "pager" survives — it's handled by pty close elsewhere
func TestTerminateProcessGracefullyKillsPagerProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pager.pid")

	// Create a "git" process (bash) in a new process group.
	// It spawns a "pager" subprocess that ignores SIGTERM (like a broken less).
	cmd := exec.Command("bash", "-c", fmt.Sprintf(`
		set -e
		trap 'exit 0' SIGTERM

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
	if !assert.NoError(t, cmd.Start()) {
		return
	}
	if !assert.NotZero(t, cmd.Process.Pid, "process should have a PID") {
		return
	}

	pgid := cmd.Process.Pid

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

	// Act: call TerminateProcessGracefully.
	// It sends SIGTERM to the PID only. The "git" process dies (via trap).
	// The "pager" ignores SIGTERM and survives — the kernel's SIGHUP from
	// pty close handles subprocesses, not this function.
	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	// The "git" process exits and is reaped
	_, err = cmd.Process.Wait()
	assert.NoError(t, err)

	// Assert: the pager subprocess still lives.
	// It was not killed because TerminateProcessGracefully no longer signals
	// the process group — that's the pty close's job.
	err = syscall.Kill(pagerPid, syscall.Signal(0))
	assert.NoError(t, err,
		"pager subprocess (PID %d) should survive TerminateProcessGracefully. "+
			"SIGTERM was only sent to the direct child (PID %d), not the PG. "+
			"The pty close (elsewhere) triggers kernel SIGHUP to kill the pager.",
		pagerPid, pgid)
}

// TestTerminateProcessGracefullyNonPty verifies that for commands started
// without a separate process group (no PTY, child inherits parent's PGID),
// SIGTERM-to-PID kills the process.
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

	err := TerminateProcessGracefully(cmd)
	assert.Error(t, err)
}

// TestCleanupWithTimeoutAndSIGKILL verifies the full cleanup sequence
// that tasks.go performs when a task is stopped:
//
//  1. Close the pty master (triggers kernel SIGHUP to foreground PG)
//  2. Send SIGTERM to the direct child
//  3. Wait 500ms
//  4. SIGKILL the entire process group as fallback
//
// The "pager" ignores SIGHUP and SIGTERM (simulates a broken less whose
// signal handler deadlocked). After the 500ms timeout, SIGKILL forces it
// to exit.
func TestCleanupWithTimeoutAndSIGKILL(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pager.pid")

	cmd := exec.Command("bash", "-c", fmt.Sprintf(`
		set -e
		trap 'exit 0' SIGTERM

		(
			trap '' SIGHUP SIGTERM SIGINT
			while true; do sleep 1; done
		) &
		pager_pid=$!
		echo $pager_pid > %s
		wait
	`, pidFile))

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !assert.NoError(t, cmd.Start()) {
		return
	}
	pgid := cmd.Process.Pid

	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()

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

	// Step 1: Close the pty. In the real code this is ptmx.Close() which
	// triggers kernel SIGHUP to the foreground process group.
	// Simulated here by sending SIGHUP to PG (equivalent effect).
	_ = syscall.Kill(-pgid, syscall.SIGHUP)

	// Step 2: SIGTERM to the direct child (git).
	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	// Step 3: The "git" process dies from SIGTERM.
	// The "pager" ignores both SIGHUP and SIGTERM.
	_, err = cmd.Process.Wait()
	assert.NoError(t, err)

	// The pager should still be alive — it ignores SIGHUP and SIGTERM.
	err = syscall.Kill(pagerPid, syscall.Signal(0))
	assert.NoError(t, err,
		"pager should survive SIGHUP+SIGTERM (simulates broken less)")

	// Step 4: Wait 500ms, then SIGKILL the process group.
	// This simulates the timeout fallback in tasks.go.
	select {
	case <-time.After(500 * time.Millisecond):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}

	// After SIGKILL, the pager is dead.
	for i := 0; i < 100; i++ {
		err = syscall.Kill(pagerPid, syscall.Signal(0))
		if err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.ErrorIs(t, err, syscall.ESRCH,
		"pager (PID %d) should be dead after SIGKILL to PG (PGID %d). "+
			"SIGHUP and SIGTERM were ignored (simulating deadlocked signal handler), "+
			"so SIGKILL is the last resort.",
		pagerPid, pgid)
}
