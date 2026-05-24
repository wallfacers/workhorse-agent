//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package bash

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup tells the kernel to start the child in its own
// process group. After Start(), cmd.Process.Pid == process group id.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the whole group: SIGTERM first so well-behaved
// children get to flush, then SIGKILL 1.5 s later as a hard backstop.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	// Negative pid == process group. Errors are ignored: if the group is
	// already gone the syscall returns ESRCH and we don't care.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	go func(pid int) {
		time.Sleep(1500 * time.Millisecond)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}(pid)
}
