# GNU less: 100% CPU spin on pty close

## Problem

When a pty master is closed (e.g., lazygit navigates away from a git log pane, fzf closes, or SSH disconnects), the child `less` process spins at ~99% CPU indefinitely. Each orphaned less also has a defunct `[highlight-less-]` zombie child (truncated from `highlight-less-wrapper`).

## Root cause

In `ttyin.c`, `getchr()` reads one character at a time from `/dev/tty`:

```c
public int getchr(void)
{
    char c;
    ssize_t result;
    do
    {
        flush();
        // ...
        {
            unsigned char uc;
            result = iread(tty, &uc, sizeof(char));
            c = (char) uc;
        }
        if (result == READ_INTR)
            return (READ_INTR);
        if (result < 0)
        {
            quit(QUIT_ERROR);
        }
        // result == 0 falls through — no handler!
    } while (result != 1);
    return (unsigned char) c;
}
```

When the pty master closes, `read()` returns 0 (EOF). The loop handles `READ_INTR` and `result < 0` but **not `result == 0`**. The `do { ... } while (result != 1)` loop spins forever calling `read()` → 0 → loop → 99% CPU.

`iread()` (in `os.c`) is a thin wrapper around `read()` that handles EINTR/EAGAIN retries and signal-based longjmp. It passes `read()`'s return value straight through — `n == 0` is returned as-is to `getchr()`.

## Why `result == 0` is unambiguous

`getchr()` reads from the `tty` file descriptor, which is opened from `/dev/tty` (via `open_tty()` in `ttyin.c`). On a terminal in raw mode (which less enables):

- **Normal input:** `read()` blocks, then returns 1. Never 0.
- **Ctrl+D:** In raw mode, Ctrl+D is literal `0x04`, `read()` returns 1.
- **Signal interruption:** `read()` returns -1 with `errno=EINTR`, handled by `iread()` retrying.
- **Nonblocking mode:** `read()` returns -1 with `errno=EAGAIN`, handled by `iread()` retrying.
- **Pty master closed / terminal destroyed:** `read()` returns 0. **This is the only case.**

There is no scenario where `read()` on `/dev/tty` returns 0 and the terminal is still usable.

However, if `open_tty()` falls back to fd 2 (stderr) and stderr is redirected, `read()` returning 0 is normal EOF. The `isatty()` guard distinguishes these cases.

## The patch

File: `ttyin.c` in [gwsw/less](https://github.com/gwsw/less)

```diff
--- a/ttyin.c
+++ b/ttyin.c
@@ -193,6 +193,11 @@ public int getchr(void)
 		result = iread(tty, &uc, sizeof(char));
 		c = (char) uc;
 		if (result == READ_INTR)
 			return (READ_INTR);
+		if (result == 0)
+		{
+			if (isatty(tty))
+				return (READ_INTR);
+			quit(QUIT_ERROR);
+		}
 		if (result < 0)
 		{
```

Two cases handled:

- **Terminal fd + result==0:** Terminal is gone (pty master closed). `READ_INTR` causes `getchr()` to return to the caller, which proceeds to cleanup (including `pclose()`/`waitpid()` for the highlight wrapper child).
- **Non-terminal fd + result==0:** Normal EOF on a pipe/file (e.g., `echo foo | less` with stdin redirected). `quit(QUIT_ERROR)` exits cleanly. Without this, the loop would spin forever.

### Why `isatty()` is needed

The `tty` file descriptor comes from `open_tty()` which tries `ttyname(2)` → `/dev/tty` → fd 2 (stderr). If stderr is redirected (e.g., `less 2>/dev/null`), fd 2 is a regular file or pipe, not a terminal. On such a fd, `read()` returning 0 is normal EOF — the pipe writer closed or the file ended. Treating that as `READ_INTR` would be wrong; `quit(QUIT_ERROR)` is correct.

### What was considered and discarded

The `quit()` function does full cleanup regardless of status code — `term_deinit()`, `flush()`, `edit(NULL)`, `save_cmdhist()`, `raw_mode(0)`, `close_getchr()`, `exit(status)`. The status code only affects the process exit code.

Quit codes from `less.h`:

```c
#define QUIT_OK         EXIT_SUCCESS      // 0
#define QUIT_ERROR      EXIT_FAILURE      // 1
#define QUIT_INTERRUPT  (EXIT_FAILURE+1)  // 2
#define QUIT_SAVED_STATUS (-1)
```

| Option | Exit code | Verdict | Reason |
|---|---|---|---|
| `quit(QUIT_ERROR)` | 1 | **Used for non-tty case** | Consistent with existing `result < 0` handler in same function. Input EOF on non-tty is genuinely unexpected. |
| `quit(QUIT_INTERRUPT)` | 2 | Discarded | Semantically wrong — implies user action (Ctrl-C) that didn't happen. Same cleanup path, different exit code. |
| `return READ_INTR` | caller decides | **Used for tty case** | Correct for terminal-gone: simulates interrupt, lets normal command-loop cleanup run. |
| `return READ_INTR` for non-tty | caller decides | **Dangerous** | `getchr()` returns to caller (`commands()` loop), which calls `getchr()` again, `read()` → 0 again → infinite spin one level up the call stack. |
| `return '\0'` | caller decides | Discarded | Converted to `\340` by existing null-guard, treated as valid keypress. Loop continues normally. |

The `return READ_INTR` trap for the non-tty case: looks clean (reuse existing interrupt path) but moves the spin from inside `getchr()` to the caller's loop. The existing `result < 0` handler already calls `quit()` directly with the comment "Don't call error() here, because error calls getchr!" — same reasoning applies: when `getchr()` detects a fatal input condition, it must terminate directly rather than return to a caller that will re-enter the broken code path.

For the tty case, `return READ_INTR` is correct because the caller (`commands()`) will check `sigs` in `psignals()` and exit cleanly. The terminal is gone but the program state is consistent — cleanup can run normally.

## Why the kernel's SIGHUP doesn't save you

The Linux kernel **does** send SIGHUP when the pty master closes. In `drivers/tty/pty.c`, `pty_close()` calls `tty_vhangup(tty->link)` on the slave side, which sends SIGHUP to the foreground process group of the session. This is not a missing-kernel-feature problem.

less handles SIGHUP in `signal.c`:

```c
static RETSIGTYPE terminate(int type)
{
    (void) type;
    quit(15);
}

// In init_signals():
(void) LSIGNAL(SIGHUP, terminate);
```

`terminate()` calls `quit()` which does cleanup (closing files, pipes) then `_exit()`. So SIGHUP **should** kill less. But it doesn't, for two reasons:

### 1. `quit()` called from a signal handler may deadlock

`quit()` calls `pclose()` to close pipe file descriptors (from LESSOPEN preprocessor), closes open files, and does other cleanup. None of these are async-signal-safe. If any of them block on a mutex held by the main thread (e.g., stdio locks, memory allocator locks), `quit()` never reaches `_exit()`. The signal is delivered, the handler starts, but the handler hangs.

### 2. Race between SIGHUP delivery and `read()` returning 0

When the pty master closes, two things happen nearly simultaneously:
- `tty_vhangup()` marks the slave as hung up and sends SIGHUP
- Pending `read()` on the slave returns 0 (EOF)

If SIGHUP arrives while less is blocked in `read()`, the kernel interrupts the syscall. But less uses `signal()` (not `sigaction()`), and on Linux `signal()` sets `SA_RESTART` — meaning `read()` restarts after the handler returns instead of returning -1 with EINTR. If the handler completes (calls `_exit()`), fine. But if the handler deadlocks in `quit()`, `read()` restarts and returns 0, entering the spin.

If SIGHUP arrives between `flush()` and `read()` in `getchr()`, the handler runs first. If it deadlocks, `read()` then returns 0 and the loop spins.

### Why the `result == 0` patch is still correct

The patch handles the **observable symptom** (EOF on the tty fd) regardless of signal delivery:

- If SIGHUP is delivered and `quit()` succeeds → less exits via `_exit()` (patch not needed for this case)
- If SIGHUP is not delivered, or `quit()` deadlocks → `read()` returns 0 → patch catches it → less exits cleanly

The patch is defense against the case where the signal path fails. It's the correct fix because it handles the terminal-gone condition at the point where it's observable: `read()` returning 0 on a terminal fd that can never produce valid input again.

### SIGHUP exceptions (when the kernel does NOT send it)

| Condition | SIGHUP sent? |
|-----------|-------------|
| Slave is a controlling terminal, master closed | **Yes** — `tty_vhangup()` sends to foreground pgrp |
| Slave opened with `O_NOCTTY`, no controlling terminal | **No** — no session/pgrp to signal |
| Process is in a background process group | **No** — only foreground pgrp receives SIGHUP |
| Process has SIGHUP set to `SIG_IGN` | **No** — ignored |
| Process has SIGHUP blocked | **No** — queued but not delivered until unblocked |

## How lazygit triggers this

### Process creation

lazygit wraps git commands in a pty so git detects a terminal and invokes the pager (`pkg/gui/pty.go`):

```go
ptmx, err = pty.StartWithSize(cmd, gui.desiredPtySize(view))
cmd.Env = append(cmd.Env, "TERM=dumb")
cmd.Env = append(cmd.Env, "GIT_PAGER="+pager)
```

`git` → `less` (via `GIT_PAGER`), both inside the same pty session.

### Cleanup on navigation

When the user navigates away, `opts.Stop` channel closes. A goroutine in `pkg/tasks/tasks.go` runs:

```go
case <-opts.Stop:
    // sends SIGTERM to the process
    if err := oscommands.TerminateProcessGracefully(cmd); err != nil {
        // ...
    }
    // closes pty master — this is where less gets stuck
    onDone()
```

`onDone()` calls `onClose()` which calls `ptmx.Close()`:

```go
onClose := func() {
    gui.Mutexes.PtyMutex.Lock()
    ptmx.Close()
    delete(gui.viewPtmxMap, view.Name())
    gui.Mutexes.PtyMutex.Unlock()
}
```

### The failure chain

1. `TerminateProcessGracefully` sends SIGTERM to git
2. `ptmx.Close()` closes the pty master
3. The kernel sends SIGHUP to the foreground process group on the pty slave
4. less's signal handler returns without breaking the `getchr()` loop
5. `read()` on the pty slave returns 0 (EOF)
6. `getchr()` spins forever in `do { ... } while (result != 1)`
7. less never reaches `pclose()`/`waitpid()` → zombie `highlight-less-wrapper`

### PR that introduced the behavior

lazygit PR [#4782](https://github.com/jesseduffield/lazygit/pull/4782) (merged Aug 1, 2025): commit `8d7740a` "Don't kill tasks when we no longer need them". Previously lazygit explicitly killed child processes. The change to just closing the pty master was done to fix stale `index.lock` files from git.

## Related upstream issues

| Issue | Status | Relevance |
|-------|--------|-----------|
| [gwsw/less#558](https://github.com/gwsw/less/issues/558) | Fixed | `poll()` returns POLLIN on `/dev/null` stdin, causing `read(0)` returns 0 loop in `check_poll`. Fix added `is_tty` guard, but doesn't cover terminal-death-after-start. |
| [gwsw/less#368](https://github.com/gwsw/less/issues/368) | — | `poll()` behavior changed for ^X interrupt support, introduced tight loop bugs. |
| [gwsw/less#719](https://github.com/gwsw/less/issues/719) | Closed Jan 31, 2026 | "Infinite loop at 100% CPU when tty unavailable and binary file prompts for input." Fixed in `0f41a20b` — but only for the binary-file-prompt-when-stdout-not-tty case. Does NOT fix pty-close-after-start. |
| [junegunn/fzf#4667](https://github.com/junegunn/fzf/issues/4667) | Open | Same bug from fzf: multiple `less` at 100% CPU after fzf closes. Confirmed by fzf maintainer. |
| [ServerFault #1168529](https://serverfault.com/questions/1168529) | — | Exact same symptom: `less` 100% CPU after SSH disconnect when started via `sudo su -`. |

## Why lazygit is the right place to fix this

The user's instinct is correct: lazygit's cleanup is broken, not less. Here's why.

### What every other pty manager does

| Tool | Cleanup action | Explicit signal? | Wait? |
|------|---------------|-----------------|-------|
| **tmux** | `close(wp->fd)` (pty master) | No — relies on kernel SIGHUP | `waitpid(WNOHANG)` in SIGCHLD handler |
| **sshd** | `close(s->ptymaster)` | No — relies on kernel SIGHUP | `waitpid()` in SIGCHLD handler |
| **screen** | `close(pty_fd)` | No — relies on kernel SIGHUP | SIGCHLD handler |
| **xterm** | `close(pty_fd)` | No — relies on kernel SIGHUP | SIGCHLD handler |
| **lazygit** | `cmd.Process.Signal(SIGTERM)` → `ptmx.Close()` | **Yes — but only to git, not the process group** | `cmd.Wait()` async, errors ignored |

Every established terminal emulator does the same thing: close the pty master, let the kernel's `tty_vhangup()` send SIGHUP to the foreground process group, and reap children via SIGCHLD/waitpid. None of them send explicit signals first.

### What lazygit does wrong

```go
// pkg/tasks/tasks.go:176 — sends SIGTERM to git ONLY
oscommands.TerminateProcessGracefully(cmd)
// pkg/gui/pty.go:94 — closes pty master
ptmx.Close()
```

Three problems:

1. **SIGTERM goes to the direct child (git) only, not the process group.** `cmd.Process.Signal()` calls `kill(pid, SIGTERM)`, not `kill(-pid, SIGTERM)`. When git receives SIGTERM and exits, less is orphaned — it's in the same process group but never receives the signal.

2. **git exits before the pty master closes.** By the time `ptmx.Close()` triggers the kernel's SIGHUP, git is already dead. The foreground process group leader is gone. The kernel still sends SIGHUP to remaining processes in the group, but the timing is adversarial.

3. **No wait for the process group to actually exit.** `cmd.Wait()` only waits for the direct child (git). It doesn't wait for less. And it's async (`go func() { _ = cmd.Wait() }()`), so errors are silently ignored.

### Why the kernel's SIGHUP isn't enough

When `ptmx.Close()` runs, the kernel calls `pty_close()` → `tty_vhangup(slave)` → sends SIGHUP to the foreground process group. This DOES reach less. But:

1. less's SIGHUP handler calls `quit(15)` which does async-signal-unsafe cleanup (`pclose()`, `close()`, etc.)
2. If `quit()` deadlocks on a mutex (e.g., stdio lock held by the main thread), the handler never reaches `_exit()`
3. less uses `signal()` not `sigaction()` — on Linux, `signal()` sets `SA_RESTART`, so `read()` restarts after the handler returns
4. If the handler deadlocks, `read()` restarts and returns 0 (EOF from hung-up tty), entering the spin

This is a less bug (async-signal-unsafe signal handler), but it's a **known limitation** of signal handlers in general. POSIX only guarantees async-signal-safe functions in signal handlers. `quit()` violates this. Every complex program (vim, emacs, htop, etc.) has the same risk in their SIGHUP handlers.

### The proper fix for lazygit

**Close the pty master (triggers kernel SIGHUP to the foreground process group), then wait with a timeout, then SIGKILL.**

In `pkg/tasks/tasks.go`, replace the current cleanup:

```go
case <-opts.Stop:
    // ...

    // Close the pty master first. This triggers the kernel's tty_vhangup()
    // which sends SIGHUP to the foreground process group (git + less + highlight).
    // This is what tmux/sshd/screen do — close the pty, let the kernel signal
    // the group atomically.
    onDone()

    // Send SIGTERM to the direct child as well, for commands started without a PTY.
    if err := oscommands.TerminateProcessGracefully(cmd); err != nil {
        self.Log.Errorf("error when trying to terminate cmd task: %v; Command: %v %v", err, cmd.Path, cmd.Args)
    }

    // Wait for the process group to exit, with a timeout.
    done := make(chan struct{})
    go func() {
        cmd.Wait()
        close(done)
    }()
    select {
    case <-done:
        // Clean exit
    case <-time.After(500 * time.Millisecond):
        // SIGHUP didn't work (e.g., less's handler deadlocked).
        // Force-kill the entire process group.
        if cmd.Process != nil {
            _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
        }
        <-done // Wait for cmd.Wait() to reap the zombie
    }
```

Key changes:
1. **`onDone()` first (closes pty master) — no explicit `kill(-pid, SIGHUP)`** — the kernel's `tty_vhangup()` sends SIGHUP to the foreground process group when the pty master is closed. We don't need to send it explicitly; this is what tmux, sshd, screen, and xterm all do (see comparison table above).
2. **SIGTERM second** — sent to the direct child for non-PTY commands where closing stdout is the only way to signal termination. The pty close already handles the process group via kernel SIGHUP.
3. **Timeout + SIGKILL fallback** — if SIGHUP doesn't work (e.g., less's handler deadlocked, process ignoring it), escalate to SIGKILL after 500ms. SIGKILL can't be caught or ignored.
4. **`cmd.Wait()` blocks until the process exits** — reaps the zombie properly.

### Why `kill(-pid, SIGHUP)` is NOT needed

The kernel's SIGHUP (from `tty_vhangup()` triggered by `ptmx.Close()`) sends SIGHUP to the entire foreground process group — git, less, highlight-less-wrapper, and any other processes in the group. This is the same target as an explicit `kill(-pid, SIGHUP)`.

Sending SIGHUP explicitly before closing the pty is redundant. It also creates the timing problem described above: if git exits before the pty master closes, the pty close happens without a group leader, making the kernel's SIGHUP delivery less reliable.

The simpler approach — close pty, let kernel SIGHUP, wait, SIGKILL — is what every established terminal emulator does and has worked reliably for decades.

### Why tmux doesn't have this problem

tmux doesn't send explicit signals — it just closes the pty master. This works because:

1. tmux's child is typically a shell (bash/zsh/fish), not git
2. The shell is the session leader and receives SIGHUP from the kernel
3. The shell's SIGHUP handler forwards signals to its children (or the shell exits, and the kernel sends SIGHUP to the orphaned process group)
4. less, as a child of the shell, receives SIGHUP through the shell's forwarding

In lazygit's case, git is the session leader. git's SIGHUP handler (from the kernel) just exits. git doesn't forward SIGHUP to its children. And git is already dead from SIGTERM by the time the kernel sends SIGHUP.

### The less patch is still correct

Even with lazygit's fix, the less patch (isatty-guarded `result == 0` handler) is still the right defense. It handles the case where:
- SIGHUP is delivered but `quit()` deadlocks
- The pty master is closed and `read()` returns 0
- Without the patch, less spins forever
- With the patch, less exits cleanly

For the tty case: `return READ_INTR` → clean exit, cleanup runs.
For the non-tty case: `quit(QUIT_ERROR)` → consistent with existing error handling.

The patch fixes the observable symptom. lazygit's fix prevents the symptom from occurring in the first place. Both are needed.

## Version info

- **less:** 692 (PCRE2), Arch `less` package, `/usr/bin/less`
- **highlight:** 4.20, `/usr/bin/highlight`
- **lazygit:** 0.61.1
- **System:** Linux (Arch)
