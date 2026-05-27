package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandPath expands a leading "~" to the current user's home directory and
// returns an absolute path. Empty input is returned unchanged so the caller
// can keep a sentinel meaning of "feature disabled".
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// ResolvePaths runs ExpandPath over every file/dir field on Config so the rest
// of the program can treat them as resolved absolute paths.
func ResolvePaths(c *Config) error {
	for _, slot := range []*string{
		&c.Store.Path,
		&c.MCP.ConfigPath,
		&c.Skills.Dir,
		&c.Agents.Dir,
		&c.Memory.Dir,
	} {
		v, err := ExpandPath(*slot)
		if err != nil {
			return err
		}
		*slot = v
	}
	return nil
}
