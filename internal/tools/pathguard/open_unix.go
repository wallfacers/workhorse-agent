//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package pathguard

import (
	"os"
	"syscall"
)

// OpenRead opens path for reading using O_NOFOLLOW so a symlink swapped in
// during the race window between Resolve and Open is rejected with ELOOP.
func OpenRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// OpenWrite opens path for create/truncate write with O_NOFOLLOW. The leaf
// won't follow a symlink even if one is planted between Resolve and Open.
func OpenWrite(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
}

// OpenExclusive is OpenWrite + O_EXCL — fails if the leaf already exists. Used
// by atomic writes (write-temp then rename).
func OpenExclusive(path string, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
}
