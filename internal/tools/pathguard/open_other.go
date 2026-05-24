//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package pathguard

import (
	"fmt"
	"os"
)

// OpenRead opens path for reading. Platforms without O_NOFOLLOW (Windows,
// Plan9) cannot prevent symlink races at open time, so we re-check with
// os.Lstat afterwards and reject if the leaf is a symlink.
func OpenRead(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	if err := assertNotSymlink(path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func OpenWrite(path string, perm os.FileMode) (*os.File, error) {
	if err := assertNotSymlink(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

func OpenExclusive(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

func assertNotSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pathguard: leaf is a symlink: %s", path)
	}
	return nil
}
