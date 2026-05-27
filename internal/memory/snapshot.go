package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// Snapshot holds the immutable memory content loaded at session start.
type Snapshot struct {
	MemoryMD string
	UserMD   string
	LoadedAt time.Time
}

// Loader reads memory files from disk.
type Loader struct {
	ProfileDir string
}

// Load reads both memory files, treating missing files as empty strings.
// Code points are counted via utf8.RuneCountInString.
func (l *Loader) Load() (*Snapshot, error) {
	memDir := memoriesDir(l.ProfileDir)
	memContent, err := readFile(filepath.Join(memDir, "MEMORY.md"))
	if err != nil {
		return nil, err
	}
	userContent, err := readFile(filepath.Join(memDir, "USER.md"))
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		MemoryMD: memContent,
		UserMD:   userContent,
		LoadedAt: time.Now().UTC(),
	}, nil
}

// CharCount returns the number of Unicode code points in s.
func CharCount(s string) int {
	return utf8.RuneCountInString(s)
}

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read %s: %w", path, err)
	}
	return string(data), nil
}

func memoriesDir(profileDir string) string {
	if profileDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		return filepath.Join(home, ".workhorse-agent", "memories")
	}
	return filepath.Join(profileDir, "memories")
}
