//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package draft

import (
	"os"
	"syscall"
)

// fsDevice returns the device id underlying path, or 0 if Stat fails. The
// device id is used by sameFilesystem to verify drafts and live dirs share a
// filesystem so os.Rename is atomic.
func fsDevice(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(st.Dev)
}
