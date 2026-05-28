//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package draft

// fsDevice on unsupported platforms returns 0; sameFilesystem will then
// report "same" for any two paths. This is the conservative direction —
// the drafts-under-live-dir invariant means a false positive is harmless.
func fsDevice(path string) uint64 { return 0 }
