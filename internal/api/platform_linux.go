//go:build linux

package api

import (
	"os"
	"strings"
)

// isWSL reports whether the process is running inside Windows Subsystem for
// Linux by checking /proc/version for Microsoft/WSL markers.
func isWSL() bool {
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	s := strings.ToLower(string(b))
	return strings.Contains(s, "microsoft") || strings.Contains(s, "wsl")
}
