//go:build linux

package tasks

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jesseduffield/lazygit/pkg/gocui"
	"github.com/jesseduffield/lazygit/pkg/utils"
	"github.com/stretchr/testify/assert"
)

func readPositivePid(t *testing.T, path string) int {
	t.Helper()

	for i := 0; i < 100; i++ {
		data, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(data), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("PID file %s never contained a valid PID", path)
	return 0
}

func processState(pid int) byte {
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

func assertProcessNotRunning(t *testing.T, pid int) {
	t.Helper()

	for i := 0; i < 100; i++ {
		err := syscall.Kill(pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) || processState(pid) == 'Z' || processState(pid) == 'X' {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	assert.Failf(t, "process is still running", "pid %d state %c", pid, processState(pid))
}

func assertProcessRunning(t *testing.T, pid int) {
	t.Helper()

	for i := 0; i < 100; i++ {
		err := syscall.Kill(pid, syscall.Signal(0))
		if err == nil && processState(pid) != 'Z' && processState(pid) != 'X' {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	assert.Failf(t, "process is not running", "pid %d state %c", pid, processState(pid))
}

func TestNewCmdTaskKillsProcessGroupWhenDirectChildExits(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	pidFile := filepath.Join(t.TempDir(), "pager.pid")
	pipeReader, pipeWriter := io.Pipe()

	manager := NewViewBufferManager(
		utils.NewDummyLog(),
		io.Discard,
		func() {},
		func() {},
		func() {},
		func() {},
		func() gocui.Task { return gocui.NewFakeTask() },
	)

	cmd := exec.Command("bash", "-c", fmt.Sprintf(`
		set -e
		trap 'exit 0' SIGTERM

		(
			trap '' SIGHUP SIGTERM SIGINT
			echo $BASHPID > %s
			while true; do sleep 1; done
		) &
		wait
	`, pidFile))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := func() (*exec.Cmd, io.Reader) {
		assert.NoError(t, cmd.Start())
		return cmd, pipeReader
	}
	onDone := func() {
		_ = pipeWriter.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	}

	stop := make(chan struct{})
	taskDone := make(chan struct{})

	go func() {
		defer close(taskDone)
		_ = manager.NewCmdTask(start, "", LinesToRead{Total: -1, InitialRefreshAfter: -1}, onDone)(TaskOpts{
			Stop:                 stop,
			InitialContentLoaded: func() {},
		})
	}()

	pagerPid := readPositivePid(t, pidFile)
	close(stop)

	select {
	case <-taskDone:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not stop")
	}

	assertProcessNotRunning(t, pagerPid)
}
