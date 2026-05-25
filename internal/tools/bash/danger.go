package bash

import (
	"regexp"
	"strings"
)

// Inspect classifies a shell command against the eight pattern families the
// permission-control spec calls out. Whenever the result is Dangerous, the
// permission layer MUST force a prompt regardless of any prior allow rule.
//
// MVP intentionally does NOT catch certain bypasses; they're documented in
// CLAUDE.md and tested as such:
//
//   - absolute paths:        /bin/rm -rf /
//   - bash -c indirection:   bash -c "rm -rf /"
//   - alias indirection:     alias rm='custom-rm'
//   - hex / unicode escapes: \x72m -rf /
//   - base64-encoded:        echo "<b64>" | base64 -d  (only the |sh tail is caught)
//
// Closing those holes requires a real shell parser plus alias resolution,
// which is out of scope for MVP. The known-bypass tests guarantee we don't
// silently fix one of these and let the doc drift.
type DangerLevel int

const (
	NotDangerous DangerLevel = iota
	Dangerous
)

// dangerPattern bundles a compiled regexp with a human-readable label that
// shows up in the permission_request event.
type dangerPattern struct {
	label string
	re    *regexp.Regexp
}

// (?i) makes the regexes case-insensitive so -RF / -Fr / RM all hit.
// The "command boundary" prefix `(?:^|[;&|()` "+"`\s])` allows the dangerous
// command to start at the beginning of the input or after a shell metachar.
var dangerPatterns = []dangerPattern{
	// 1. rm -rf /, rm -rf ~, rm -rf $HOME, rm -rf /usr, etc.
	// Flag order is normalised by requiring both r and f anywhere in the
	// flag string. Trailing target may be /, ~, $HOME — anything after the
	// root marker is part of the same path so we don't pin the right edge.
	{
		label: "destructive_rm",
		re:    regexp.MustCompile(`(?i)\brm\s+-(?:[a-z]*r[a-z]*f|[a-z]*f[a-z]*r)[a-z]*\s+(?:/|~|\$HOME)`),
	},
	// 2. dd writing to a block device.
	{
		label: "dd_to_device",
		re:    regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/`),
	},
	// 3. mkfs.* — formatting a filesystem. Match at start of a command only
	// to avoid hitting "mkfs" inside a code comment or string.
	{
		label: "mkfs",
		re:    regexp.MustCompile(`(?m)(?:^|[;&|()]\s*)\s*mkfs(?:\.[a-z0-9]+)?\b`),
	},
	// 4. Redirecting to a raw block device.
	{
		label: "redirect_to_block_device",
		re:    regexp.MustCompile(`>\s*/dev/(?:sd[a-z]+\d*|nvme\d+n\d+(?:p\d+)?|hd[a-z]+\d*|mmcblk\d+(?:p\d+)?|vd[a-z]+\d*)`),
	},
	// 5. Fork bomb.
	{
		label: "fork_bomb",
		re:    regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:\s*&[^}]*\}\s*;\s*:`),
	},
	// 6. chmod -R 777 / or ~. Recursive world-writeable on system roots.
	{
		label: "chmod_world_root",
		re:    regexp.MustCompile(`\bchmod\s+-[A-Za-z]*R[A-Za-z]*\s+777\s+(?:/|~|\$HOME)`),
	},
	// 7. shutdown / reboot / halt / poweroff *as a command*. Must appear at
	// the start of input or directly after a shell command separator so a
	// comment ("# foo about shutdown") doesn't trip it.
	{
		label: "system_power",
		re:    regexp.MustCompile(`(?m)(?:^|[;&|()]\s*|&\s*|\|\s*)\s*(?:shutdown|reboot|halt|poweroff)\b`),
	},
	// 8. piping curl/wget/base64 output into a shell.
	{
		label: "pipe_to_shell",
		re:    regexp.MustCompile(`\b(?:curl|wget|base64\s+-[dD])\b[^|]*\|\s*(?:sh|bash|zsh|ksh)\b`),
	},
}

// Inspect returns (Dangerous, label) on the first pattern match, otherwise
// (NotDangerous, ""). cmd is the *exact* command string the model produced —
// no normalisation, because the spec scenarios use literal strings.
func Inspect(cmd string) (DangerLevel, string) {
	// Bound the input so a pathological multi-MB command can't burn regexp
	// engines for too long.
	if len(cmd) > 1<<20 {
		cmd = cmd[:1<<20]
	}
	// Cheap pre-screen: strip trailing whitespace + collapse internal CRs.
	cmd = strings.ReplaceAll(cmd, "\r", "")
	for _, p := range dangerPatterns {
		if p.re.MatchString(cmd) {
			return Dangerous, p.label
		}
	}
	return NotDangerous, ""
}
