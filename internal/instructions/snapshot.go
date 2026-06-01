package instructions

import "time"

// File holds the path and content of a single discovered instruction file.
type File struct {
	Path    string
	Content string
}

// Snapshot holds the immutable instruction files loaded at session start.
type Snapshot struct {
	Files    []File
	LoadedAt time.Time
}
