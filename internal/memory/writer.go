package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"
)

// ErrMemoryTooLarge is returned when a write would exceed the character limit.
type ErrMemoryTooLarge struct {
	Limit  int
	Actual int
}

func (e ErrMemoryTooLarge) Error() string {
	return fmt.Sprintf("memory: content exceeds char limit (limit=%d, actual=%d)", e.Limit, e.Actual)
}

// writeMu serializes concurrent goroutine writes within the same process.
// flock/LockFileEx are per-process, so two goroutines in the same process
// would both "hold" the exclusive lock simultaneously.
var writeMu sync.Mutex

// Writer handles atomic writes to memory files with character-limit enforcement
// and exclusive file locking.
type Writer struct {
	ProfileDir  string
	MemoryLimit int
	UserLimit   int
}

// Kind enumerates the valid memory file types.
type Kind string

const (
	KindMemory Kind = "memory"
	KindUser   Kind = "user"
)

// WriteMode controls how content is written.
type WriteMode string

const (
	ModeReplace WriteMode = "replace"
	ModeAppend  WriteMode = "append"
)

// ValidateKind returns an error if k is not a valid Kind.
func ValidateKind(k string) (Kind, error) {
	switch Kind(k) {
	case KindMemory:
		return KindMemory, nil
	case KindUser:
		return KindUser, nil
	default:
		return "", fmt.Errorf("memory: invalid kind %q", k)
	}
}

// FileName returns the fixed filename for a given kind.
func (k Kind) FileName() string {
	switch k {
	case KindMemory:
		return "MEMORY.md"
	case KindUser:
		return "USER.md"
	default:
		return ""
	}
}

// CharLimit returns the configured character limit for a given kind.
func (w *Writer) CharLimit(kind Kind) int {
	switch kind {
	case KindMemory:
		return w.MemoryLimit
	case KindUser:
		return w.UserLimit
	default:
		return 0
	}
}

// Write writes content to the memory file for the given kind.
// It enforces character limits, acquires an exclusive lock, and uses
// atomic temp-file + rename to prevent partial writes.
func (w *Writer) Write(kind Kind, content string, mode WriteMode) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	memDir := memoriesDir(w.ProfileDir)
	lockPath := filepath.Join(memDir, ".write.lock")
	filePath := filepath.Join(memDir, kind.FileName())

	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return fmt.Errorf("memory: create dir: %w", err)
	}

	limit := w.CharLimit(kind)

	release, err := acquireLock(lockPath)
	if err != nil {
		return fmt.Errorf("memory: acquire lock: %w", err)
	}
	defer release()

	finalContent := content
	if mode == ModeAppend {
		existing, err := readFile(filePath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: read existing for append: %w", err)
		}
		if existing != "" && content != "" {
			finalContent = existing + "\n" + content
		} else if existing != "" {
			finalContent = existing
		}
	}

	count := utf8.RuneCountInString(finalContent)
	if count > limit {
		return ErrMemoryTooLarge{Limit: limit, Actual: count}
	}

	return atomicWriteFile(filePath, []byte(finalContent))
}

func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".memtmp-*")
	if err != nil {
		return fmt.Errorf("memory: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("memory: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("memory: close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("memory: rename: %w", err)
	}
	return nil
}

// ReadFile reads the current content of a memory file from disk.
func ReadFile(profileDir, kind string) (string, error) {
	k, err := ValidateKind(kind)
	if err != nil {
		return "", err
	}
	memDir := memoriesDir(profileDir)
	return readFile(filepath.Join(memDir, k.FileName()))
}

// EnsureValidMode returns the effective write mode, defaulting to replace.
func EnsureValidMode(mode string) WriteMode {
	if mode == string(ModeAppend) {
		return ModeAppend
	}
	return ModeReplace
}

// ValidKinds for iteration.
var ValidKinds = []Kind{KindMemory, KindUser}
