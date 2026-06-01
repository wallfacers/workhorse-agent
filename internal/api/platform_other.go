//go:build !linux

package api

// isWSL always returns false on non-Linux platforms.
func isWSL() bool { return false }
