package pathscan

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CacheFile struct {
	ScannedAt          time.Time `json:"scanned_at"`
	ExtraFingerprint   string    `json:"extra_fingerprint"`
	DisabledFingerprint string   `json:"disabled_fingerprint"`
	Entries            []Entry   `json:"entries"`
}

// fingerprint computes a stable hash for a sorted string slice.
func fingerprint(names []string) string {
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// LoadCache reads the pathscan cache. Returns nil without error if missing.
func LoadCache(path string) (*CacheFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, nil // treat read errors as cache miss
	}
	var cf CacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, nil // malformed = cache miss
	}
	return &cf, nil
}

// WriteCache writes the cache atomically using temp-file + rename.
func WriteCache(path string, cf *CacheFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CachedScan returns cached entries if valid, or runs a fresh scan.
// Cache is valid when: file exists, scanned_at within ttl, fingerprints match.
func CachedScan(logger *slog.Logger, cachePath string, ttl time.Duration, extra, disabled []string) []Entry {
	cf, err := LoadCache(cachePath)
	if err != nil {
		logger.Debug("pathscan: cache read error", "err", err)
	}

	fp := fingerprint(extra)
	dp := fingerprint(disabled)

	if cf != nil {
		age := time.Since(cf.ScannedAt)
		if age <= ttl && cf.ExtraFingerprint == fp && cf.DisabledFingerprint == dp {
			logger.Debug("pathscan: cache hit", "age", age, "entries", len(cf.Entries))
			return cf.Entries
		}
		logger.Debug("pathscan: cache invalid",
			"age", age, "ttl", ttl,
			"fp_match", cf.ExtraFingerprint == fp,
			"dp_match", cf.DisabledFingerprint == dp)
	}

	allowlist := Allowlist(extra, disabled)
	entries := Scan(logger, allowlist)

	newCf := &CacheFile{
		ScannedAt:           time.Now(),
		ExtraFingerprint:    fp,
		DisabledFingerprint: dp,
		Entries:             entries,
	}
	if err := WriteCache(cachePath, newCf); err != nil {
		logger.Warn("pathscan: failed to write cache", "path", cachePath, "err", err)
	}

	return entries
}

// ResolveCachePath returns the full cache file path given the profile directory.
func ResolveCachePath(profileDir string) string {
	return filepath.Join(profileDir, "cache", "pathscan.json")
}

// EntriesForEnvironment converts entries to the format needed by the EnvironmentBlock.
// This is a convenience to avoid importing pathscan in the prompt package.
type CLITool struct {
	Name    string
	Path    string
	Version string
}

func ToCLITools(entries []Entry) []CLITool {
	out := make([]CLITool, len(entries))
	for i, e := range entries {
		out[i] = CLITool(e)
	}
	return out
}

// FormatVersion returns a human-readable version string, stripping common prefixes.
func FormatVersion(v string) string {
	// Strip common "name version " prefix (e.g. "git version 2.43.0" → "2.43.0")
	for {
		if idx := strings.Index(v, " "); idx >= 0 {
			rest := v[idx+1:]
			if looksLikeVersion(rest) {
				return rest
			}
			v = rest
		} else {
			break
		}
	}
	if looksLikeVersion(v) {
		return v
	}
	return ""
}

func looksLikeVersion(s string) bool {
	if len(s) == 0 {
		return false
	}
	// Digits like "2.43.0"
	if s[0] >= '0' && s[0] <= '9' {
		return true
	}
	// "v" followed by digit like "v14.21.0"
	if s[0] == 'v' && len(s) > 1 && s[1] >= '0' && s[1] <= '9' {
		return true
	}
	return false
}

// FormatEntry formats a single CLI tool for the environment block.
func FormatEntry(e Entry) string {
	v := FormatVersion(e.Version)
	if v != "" {
		return fmt.Sprintf("- %s @ %s (%s)", e.Name, e.Path, v)
	}
	return fmt.Sprintf("- %s @ %s", e.Name, e.Path)
}
