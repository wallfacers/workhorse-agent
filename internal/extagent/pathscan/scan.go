package pathscan

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Entry represents one detected binary on PATH.
type Entry struct {
	Name    string
	Path    string
	Version string
}

// Scan runs exec.LookPath for each name in the allowlist in parallel,
// then probes versions. Returns sorted by name.
func Scan(logger *slog.Logger, allowlist []string) []Entry {
	if len(allowlist) == 0 {
		return nil
	}
	if len(allowlist) > 80 && logger != nil {
		logger.Warn("pathscan.large", "name_count", len(allowlist))
	}

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers > len(allowlist) {
		workers = len(allowlist)
	}

	// Phase 1: LookPath in parallel.
	found := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan string, len(allowlist))

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				p, err := exec.LookPath(name)
				if err != nil {
					continue
				}
				mu.Lock()
				found[name] = p
				mu.Unlock()
			}
		}()
	}

	for _, name := range allowlist {
		jobs <- name
	}
	close(jobs)
	wg.Wait()

	// Phase 2: Version probe in parallel.
	entries := make([]Entry, 0, len(found))
	var vWg sync.WaitGroup
	var vMu sync.Mutex
	vJobs := make(chan Entry, len(found))

	vWorkers := workers
	if vWorkers > len(found) {
		vWorkers = len(found)
	}

	for i := 0; i < vWorkers; i++ {
		vWg.Add(1)
		go func() {
			defer vWg.Done()
			for e := range vJobs {
				e.Version = Version(e.Path, logger)
				vMu.Lock()
				entries = append(entries, e)
				vMu.Unlock()
			}
		}()
	}

	for name, path := range found {
		vJobs <- Entry{Name: name, Path: path}
	}
	close(vJobs)
	vWg.Wait()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// Version probes a binary for its version string with a 2s timeout.
func Version(bin string, logger *slog.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--version")
	var buf bytes.Buffer
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		if logger != nil {
			logger.Debug("pathscan: version probe failed", "bin", bin, "err", err)
		}
		return ""
	}

	output := bytes.TrimSpace(buf.Bytes())
	if idx := bytes.IndexByte(output, '\n'); idx >= 0 {
		output = output[:idx]
	}
	return string(output)
}
