//go:build windows

package memory

import (
	"os"
	"syscall"
	unsafe "unsafe"
)

func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	var overlapped syscall.Overlapped
	err = syscall.LockFileEx(
		syscall.Handle(f.Fd()),
		syscall.LOCKFILE_EXCLUSIVE_LOCK,
		0, 0xFFFFFFFF, 0xFFFFFFFF,
		&overlapped,
	)
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.UnlockFile(syscall.Handle(f.Fd()), 0, 0, 0xFFFFFFFF, 0xFFFFFFFF, &overlapped)
		f.Close()
	}, nil
}

var _ = unsafe.Sizeof(byte(0))
