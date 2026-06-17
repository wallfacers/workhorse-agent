package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// migrationMarker is the per-profile sentinel that records a completed flat-file
// migration. Its presence makes MigrateLegacyFiles a no-op.
const migrationMarker = ".migrated_to_entries"

// headingRe matches a markdown heading line (1–6 leading '#' followed by
// whitespace) at the start of a line, used to split MEMORY.md into sections.
var headingRe = regexp.MustCompile(`(?m)^#{1,6}\s`)

// hrSplitRe splits on a horizontal-rule separator line (`---`, optionally padded
// with surrounding whitespace) used as the fallback when MEMORY.md has no
// headings.
var hrSplitRe = regexp.MustCompile(`(?m)^\s*---\s*$`)

// nonSlugRe matches runs of characters that are not lowercase ASCII letters or
// digits, collapsed to a single '-' during slug generation.
var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// MigrateLegacyFiles performs a one-time, idempotent migration of the legacy
// flat-file memory (USER.md + MEMORY.md under <profile>/memories/) into the
// per-entry store (design D7). It is safe to call on every startup:
//
//   - If the marker file exists, it returns nil immediately.
//   - If neither legacy file has content, it returns nil WITHOUT writing the
//     marker (nothing to migrate; a cheap recheck on the next startup is fine).
//   - Otherwise it upserts entries, copies originals to memories/legacy/, and
//     writes the marker only after every step succeeds, so a partial failure is
//     retried on the next startup (best-effort; already-written entries are not
//     rolled back, but the upsert keys on name so a retry overwrites rather than
//     duplicates).
func MigrateLegacyFiles(ctx context.Context, store *EntryStore, profileDir string) error {
	dir := memoriesDir(profileDir)
	markerPath := filepath.Join(dir, migrationMarker)
	if _, err := os.Stat(markerPath); err == nil {
		return nil // already migrated
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("memory: stat migration marker: %w", err)
	}

	userPath := filepath.Join(dir, "USER.md")
	memoryPath := filepath.Join(dir, "MEMORY.md")

	userContent, err := readFile(userPath)
	if err != nil {
		return err
	}
	memoryContent, err := readFile(memoryPath)
	if err != nil {
		return err
	}

	userContent = strings.TrimRight(userContent, "\n")
	if strings.TrimSpace(userContent) == "" {
		userContent = ""
	}
	if strings.TrimSpace(memoryContent) == "" {
		memoryContent = ""
	}

	// Nothing to migrate: don't write the marker so we recheck cheaply next boot.
	if userContent == "" && memoryContent == "" {
		return nil
	}

	now := time.Now().UTC()

	// USER.md → one pinned, evergreen, category=user entry.
	if userContent != "" {
		e := &Entry{
			Name:       "user-profile",
			Category:   "user",
			Durability: "evergreen",
			Pinned:     true,
			Trigger:    "User identity and preferences",
			Content:    userContent,
			CharCount:  utf8.RuneCountInString(userContent),
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := store.Upsert(ctx, e); err != nil {
			return err
		}
	}

	// MEMORY.md → one volatile entry per markdown section.
	if memoryContent != "" {
		used := map[string]int{}
		for _, sec := range splitSections(memoryContent) {
			name := uniqueName(slugify(sec), used)
			content := strings.TrimSpace(sec)
			e := &Entry{
				Name:       name,
				Category:   "",
				Durability: "volatile",
				Pinned:     false,
				Trigger:    firstSentence(sectionBody(content)),
				Content:    content,
				CharCount:  utf8.RuneCountInString(content),
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if err := store.Upsert(ctx, e); err != nil {
				return err
			}
		}
	}

	// Back up originals to memories/legacy/ (do NOT delete the originals).
	legacyDir := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		return fmt.Errorf("memory: create legacy dir: %w", err)
	}
	if userContent != "" {
		if err := copyFile(userPath, filepath.Join(legacyDir, "USER.md")); err != nil {
			return err
		}
	}
	if memoryContent != "" {
		if err := copyFile(memoryPath, filepath.Join(legacyDir, "MEMORY.md")); err != nil {
			return err
		}
	}

	// Marker last: only a fully successful migration is recorded.
	if err := os.WriteFile(markerPath, []byte(now.Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return fmt.Errorf("memory: write migration marker: %w", err)
	}
	return nil
}

// splitSections splits MEMORY.md into sections. Primary rule: each markdown
// heading (and its following body up to the next heading) is one section; any
// non-empty preamble before the first heading is its own section. When the
// document has no heading at all, it falls back to splitting on horizontal-rule
// (`---`) separators; if that still yields a single block the whole document is
// one section. Empty/whitespace-only sections are dropped.
func splitSections(s string) []string {
	locs := headingRe.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return splitByHR(s)
	}

	var sections []string
	// Preamble before the first heading.
	if pre := strings.TrimSpace(s[:locs[0][0]]); pre != "" {
		sections = append(sections, s[:locs[0][0]])
	}
	for i, loc := range locs {
		start := loc[0]
		end := len(s)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if strings.TrimSpace(s[start:end]) != "" {
			sections = append(sections, s[start:end])
		}
	}
	return sections
}

// splitByHR splits on horizontal-rule separators; a single resulting block is
// returned as one section.
func splitByHR(s string) []string {
	parts := hrSplitRe.Split(s, -1)
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// slugify derives a slug name from a section: it uses the first heading text if
// present (otherwise the first non-empty line), lowercases it, replaces every
// run of non-[a-z0-9] with '-', and trims leading/trailing '-'. An empty result
// falls back to "memory-imported".
func slugify(section string) string {
	title := ""
	for _, line := range strings.Split(section, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		// Strip leading heading markers if this is a heading line.
		t = strings.TrimLeft(t, "#")
		t = strings.TrimSpace(t)
		if t != "" {
			title = t
			break
		}
	}
	slug := nonSlugRe.ReplaceAllString(strings.ToLower(title), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "memory-imported"
	}
	return slug
}

// uniqueName returns name unless it has been used; on collision it appends
// "-2", "-3", … until unique. used is updated in place.
func uniqueName(name string, used map[string]int) string {
	if _, ok := used[name]; !ok {
		used[name] = 1
		return name
	}
	for {
		used[name]++
		candidate := fmt.Sprintf("%s-%d", name, used[name])
		if _, ok := used[candidate]; !ok {
			used[candidate] = 1
			return candidate
		}
	}
}

// sectionBody returns the section content with a leading markdown heading line
// stripped, so the trigger is derived from the body rather than the title. A
// section without a heading line (HR-split fallback) is returned unchanged.
func sectionBody(section string) string {
	trimmed := strings.TrimLeft(section, "\n")
	if !strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
		return section
	}
	if i := strings.IndexByte(trimmed, '\n'); i >= 0 {
		return trimmed[i+1:]
	}
	return "" // heading-only section
}

// firstSentence returns the first sentence of content as the entry trigger:
// content up to the first '.', '。', or newline, trimmed and capped at 120 code
// points.
func firstSentence(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	end := len(content)
	for i, r := range content {
		if r == '.' || r == '。' || r == '\n' {
			end = i
			break
		}
	}
	sentence := strings.TrimSpace(content[:end])
	if utf8.RuneCountInString(sentence) > 120 {
		runes := []rune(sentence)
		sentence = string(runes[:120])
	}
	return sentence
}

// copyFile copies src to dst (0o600). A missing src is silently skipped so a
// migration importing only one of the two files does not fail.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("memory: read %s for backup: %w", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("memory: write backup %s: %w", dst, err)
	}
	return nil
}
