//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package bash

import "os/exec"

// configureProcessGroup is a no-op on platforms without Setpgid; the
// CommandContext kill fallback handles teardown.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup falls back to killing the top-level process. Grandchildren
// may linger; the spec marks Bash process-group teardown as a Unix-only
// guarantee.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
