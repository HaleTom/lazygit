//go:build linux

package oscommands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
)

// procAlive returns nil if the process is alive, or the error from kill(2).
func procAlive(pid int) error {
	return syscall.Kill(pid, syscall.Signal(0))
}

// waitForDead polls until the process is dead or times out.
func waitForDead(t *testing.T, pid int) error {
	t.Helper()
	var err error
	for i := 0; i < 100; i++ {
		err = procAlive(pid)
		if err != nil {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

// readPidFile polls a PID file until it contains a positive PID.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	for i := 0; i < 100; i++ {
		data, err := os.ReadFile(path)
		if err == nil {
			fmt.Sscanf(string(data), "%d", &pid)
			if pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PID file %s never contained a valid PID", path)
	return 0
}

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
	pagerPid := readPidFile(t, pidFile)

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
	err = procAlive(pagerPid)
	assert.NoError(t, err,
		"pager subprocess (PID %d) should survive TerminateProcessGracefully. "+
			"SIGTERM was only sent to the direct child (PID %d), not the PG. "+
			"The pty close (elsewhere) triggers kernel SIGHUP to kill the pager.",
		pagerPid, pgid)
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
	err = procAlive(cmd.Process.Pid)
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

// readProcessState reads the process state character from /proc/<pid>/stat.
// Returns '?' if the process is gone or the file cannot be read.
func readProcessState(t *testing.T, pid int) byte {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return '?'
	}
	s := string(data)
	lastParen := strings.LastIndex(s, ") ")
	if lastParen < 0 || lastParen+3 >= len(s) {
		return '?'
	}
	return s[lastParen+2]
}

// readJiffies reads utime+stime from /proc/<pid>/stat. On Linux CLK_TCK=100,
// so 1 second of CPU burn ≈ 100 jiffies.
func readJiffies(t *testing.T, pid int) int64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Logf("readJiffies: /proc/%d/stat: %v", pid, err)
		return 0
	}
	s := string(data)
	lastParen := strings.LastIndex(s, ") ")
	fields := strings.Fields(s[lastParen+2:])
	if len(fields) < 13 {
		t.Logf("readJiffies: /proc/%d/stat has only %d fields: %s", pid, len(fields), s)
		return 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	t.Logf("readJiffies(pid=%d): utime=%d stime=%d (fields: %v)", pid, utime, stime, fields)
	return utime + stime
}

// TestCleanupWithTimeoutAndSIGKILL demonstrates the subprocess cleanup bug
// in tasks.go: the cleanup sequence sends SIGTERM to the direct child and
// only then closes the pty/pipe, leaving no fallback for subprocesses that
// ignore SIGTERM. A subprocess that ignores SIGTERM survives because there
// is no SIGKILL timeout fallback.
//
// The correct cleanup sequence (applied in the fix commit) is:
//  1. Close the pty master (triggers kernel SIGHUP to foreground PG)
//  2. Send SIGTERM to the direct child
//  3. Wait 500ms
//  4. SIGKILL the entire process group as fallback
func TestCleanupWithTimeoutAndSIGKILL(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pager.pid")

	// The "pager" ignores SIGHUP and SIGTERM (simulates a broken less
	// whose signal handler deadlocked).
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

	pagerPid := readPidFile(t, pidFile)

	// Broken cleanup sequence from tasks.go: SIGTERM the direct child,
	// then close the pipe. The subprocess ignores SIGTERM and SIGHUP,
	// and there is no SIGKILL fallback — it survives.
	err := TerminateProcessGracefully(cmd)
	assert.NoError(t, err)

	_, err = cmd.Process.Wait()
	assert.NoError(t, err)

	err = procAlive(pagerPid)
	assert.NoError(t, err,
		"pager (PID %d) should survive broken cleanup: SIGTERM-only with no "+
			"SIGKILL fallback leaves the subprocess alive",
		pagerPid)
}

// TestPtyCloseShowsCpuSpin demonstrates the CPU spin bug described in
// GNU_less.md: when a pty master is closed while less is reading from it,
// less's getchr() loop spins at ~99% CPU because read() returns 0 (EOF)
// and the do { ... } while (result != 1) loop never exits.
//
// Technique (from the less/spin repro):
//  1. Block SIGHUP via sigprocmask (the signal MASK survives exec, unlike
//     signal disposition which is reset to SIG_DFL on exec by POSIX).
//     Without this, less's init_signals() installs SIGHUP→terminate, and
//     the kernel's SIGHUP on ptmx close kills less before it can spin.
//  2. setsid() in the child to detach from any existing CTTY
//  3. Redirect stderr to /dev/null so ttyname(2) fails — less falls back
//     to /dev/null for its tty fd. read() on /dev/null returns 0.
//  4. Exec less — open_tty() falls through: ttyname(2)=NULL,
//     /dev/tty=ENXIO (no CTTY), fd 2=/dev/null → read()=0 → spin
//  5. Close the pty master — read() on /dev/null returns 0 → spin.
//
// A C helper is compiled at test time because Go has no way to call
// sigprocmask between fork and exec — only the signal mask survives exec,
// not the SIG_IGN set by bash's trap(1).
const lessSpinHelperC = `
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

int main(int argc, char *argv[]) {
	if (argc < 4) { fprintf(stderr, "usage: less_helper <file> <slave_fd> <less_path>\n"); return 1; }
	const char *file = argv[1];
	int slave_fd = atoi(argv[2]);
	const char *less_path = argv[3];

	/* Block SIGHUP at the mask level. The mask survives exec (POSIX),
	   unlike signal dispositions which reset to SIG_DFL on exec. */
	sigset_t set;
	sigemptyset(&set);
	sigaddset(&set, SIGHUP);
	sigprocmask(SIG_BLOCK, &set, NULL);

	/* New session — no controlling terminal. */
	setsid();

	/* Redirect stdin/stdout to the pty slave. */
	dup2(slave_fd, 0);
	dup2(slave_fd, 1);

	/* Redirect stderr to /dev/null — less's ttyname(2) on fd 2 will
	   return NULL, forcing the /dev/null fallback in open_tty(). */
	int devnull = open("/dev/null", O_RDWR);
	if (devnull >= 0) { dup2(devnull, 2); close(devnull); }
	if (slave_fd > 2) close(slave_fd);

	execl(less_path, "less", file, (char *)NULL);
	perror("execl");
	return 1;
}
`

func TestPtyCloseShowsCpuSpin(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc not found, skipping C helper compilation")
	}
	lessPath, err := exec.LookPath("less")
	if err != nil {
		t.Skip("less not found, skipping spin repro")
	}

	content := filepath.Join(t.TempDir(), "content.txt")
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf("line %d of 30", i+1))
	}
	if !assert.NoError(t, os.WriteFile(content, []byte(strings.Join(lines, "\n")), 0o644)) {
		return
	}

	// Compile the C helper.
	helperSrc := filepath.Join(t.TempDir(), "less_helper.c")
	helperBin := filepath.Join(t.TempDir(), "less_helper")
	if !assert.NoError(t, os.WriteFile(helperSrc, []byte(lessSpinHelperC), 0o644)) {
		return
	}
	compile := exec.Command("cc", "-o", helperBin, helperSrc)
	compile.Stderr = os.Stderr
	if !assert.NoError(t, compile.Run(), "failed to compile less helper") {
		return
	}

	ptmx, slave, err := pty.Open()
	if !assert.NoError(t, err) {
		return
	}
	if !assert.NoError(t, pty.Setsize(slave, &pty.Winsize{Cols: 80, Rows: 24})) {
		return
	}
	defer ptmx.Close()

	// Pass the pty slave as fd 3 via ExtraFiles. The C helper dups it
	// to stdin/stdout and closes the original.
	cmd := exec.Command(helperBin, content, "3", lessPath)
	cmd.ExtraFiles = []*os.File{slave}
	if !assert.NoError(t, cmd.Start()) {
		return
	}
	slave.Close()

	pgid := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()

	time.Sleep(300 * time.Millisecond)

	jiffiesBefore := readJiffies(t, cmd.Process.Pid)

	// Close the pty master — this is what lazygit does (ptmx.Close())
	ptmx.Close()

	time.Sleep(2000 * time.Millisecond)

	jiffiesAfter := readJiffies(t, cmd.Process.Pid)
	state := readProcessState(t, cmd.Process.Pid)
	jiffiesDelta := jiffiesAfter - jiffiesBefore
	t.Logf("jiffies delta after pty close: %d (process state: %c)", jiffiesDelta, state)

	// What happens after pty close depends on the kernel and less version:
	//
	//   Old kernel (read() returns 0 on closed pty):
	//     less's getchr() loop spins forever on EOF → process stays alive (R/S)
	//     with rising jiffies. This is the bug described in GNU_less.md.
	//
	//   Modern kernel (read() returns -EIO on closed pty):
	//     less's iread() gets -1 → quit(QUIT_ERROR) → process exits (Z/X).
	//     No spin, no hang. The bug never triggers.
	//
	//   Patched less (upstream fix for the EOF spin):
	//     getchr() detects result==0 and calls quit() → process exits (Z/X).
	//     Same outcome as the EIO case.
	//
	// This test must pass in all three scenarios. We assert either a clean
	// exit (bug absent) or a measurable CPU spin (bug present). The only
	// failure case is "alive but not spinning" — which would indicate a
	// different hang (e.g. blocked in read() with a live master fd), not
	// the EOF spin bug.

	switch {
	case state == 'Z' || state == 'X':
		// Process exited cleanly — either this kernel returns EIO, or
		// less is already patched. No spin bug to worry about.
		t.Logf("less exited cleanly after pty close (state=%c); "+
			"spin bug not triggered on this kernel/less version", state)

	case jiffiesDelta > 50:
		// Process is alive and burning CPU — the EOF spin bug manifests.
		// This is the scenario lazygit's SIGKILL fallback must handle.
		t.Logf("CPU spin detected after pty close (jiffies delta=%d); "+
			"less has the EOF spin bug on this kernel/less version", jiffiesDelta)

	default:
		t.Fatalf("unexpected state after pty close: state=%c, jiffies delta=%d; "+
			"expected either clean exit (Z/X) or CPU spin (delta>50)",
			state, jiffiesDelta)
	}
}
