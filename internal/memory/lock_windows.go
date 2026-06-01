//go:build windows

package memory

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func acquireLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, 0xFFFFFFFF, 0xFFFFFFFF,
		&overlapped,
	)
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 0xFFFFFFFF, 0xFFFFFFFF, &overlapped)
		f.Close()
	}, nil
}

var _ = unsafe.Sizeof(byte(0))
