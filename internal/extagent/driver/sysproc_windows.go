//go:build windows

package driver

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{}
}

func killProcessGroup(cmd *exec.Cmd, _ syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
