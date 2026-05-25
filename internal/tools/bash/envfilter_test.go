package bash_test

import (
	"slices"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/tools/bash"
)

func TestFilter_ExactMatchDeny(t *testing.T) {
	cases := []string{
		"LD_PRELOAD=/tmp/evil.so",
		"LD_LIBRARY_PATH=/tmp",
		"LD_AUDIT=audit.so",
		"DYLD_INSERT_LIBRARIES=evil.dylib",
		"DYLD_LIBRARY_PATH=/tmp",
		"DYLD_FALLBACK_LIBRARY_PATH=/tmp",
		"DYLD_FORCE_FLAT_NAMESPACE=1",
		"PYTHONPATH=/tmp",
		"PYTHONSTARTUP=/tmp/startup.py",
	}
	kept, dropped := bash.Filter(cases)
	if len(kept) != 0 {
		t.Errorf("none of the exact-match entries should pass, got kept=%v", kept)
	}
	if len(dropped) != len(cases) {
		t.Errorf("dropped count %d, want %d", len(dropped), len(cases))
	}
}

func TestFilter_DYLDPrefix(t *testing.T) {
	// future / undocumented DYLD_* must still be dropped.
	in := []string{"DYLD_VERSIONED_FRAMEWORK_PATH=/tmp", "DYLD_PRINT_LIBRARIES=1"}
	kept, dropped := bash.Filter(in)
	if len(kept) != 0 {
		t.Errorf("DYLD_* prefix must always drop, kept=%v", kept)
	}
	if !slices.Contains(dropped, "DYLD_VERSIONED_FRAMEWORK_PATH") ||
		!slices.Contains(dropped, "DYLD_PRINT_LIBRARIES") {
		t.Errorf("missing entries in dropped: %v", dropped)
	}
}

func TestFilter_NodeOptionsSafe(t *testing.T) {
	in := []string{
		"NODE_OPTIONS=--max-old-space-size=8192 --enable-source-maps",
	}
	kept, dropped := bash.Filter(in)
	if len(kept) != 1 {
		t.Errorf("safe NODE_OPTIONS should pass through, kept=%v dropped=%v", kept, dropped)
	}
}

func TestFilter_NodeOptionsDangerous(t *testing.T) {
	cases := []string{
		"NODE_OPTIONS=--require ./evil.js",
		"NODE_OPTIONS=--import=evil",
		"NODE_OPTIONS=--experimental-loader=foo",
		"NODE_OPTIONS=--inspect=0.0.0.0:9229",
		"NODE_OPTIONS=--inspect-brk",
		"NODE_OPTIONS=--max-old-space-size=8192 --require=./taint.js", // mixed
	}
	for _, e := range cases {
		kept, dropped := bash.Filter([]string{e})
		if len(kept) != 0 {
			t.Errorf("expected dropped for %q, kept=%v", e, kept)
		}
		if !slices.Contains(dropped, "NODE_OPTIONS") {
			t.Errorf("expected NODE_OPTIONS in dropped for %q, got %v", e, dropped)
		}
	}
}

func TestFilter_PreservesUnrelated(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/home/foo", "LANG=en_US.UTF-8"}
	kept, dropped := bash.Filter(in)
	if len(kept) != 3 {
		t.Errorf("ordinary env should pass, kept=%v dropped=%v", kept, dropped)
	}
}

func TestFilterMap(t *testing.T) {
	in := map[string]string{
		"LD_PRELOAD":   "/tmp/x",
		"NODE_OPTIONS": "--require=evil",
		"PATH":         "/usr/bin",
		"DYLD_VERSION": "1",
		"HOME":         "/home/foo",
	}
	out, dropped := bash.FilterMap(in)
	if _, ok := out["PATH"]; !ok {
		t.Error("PATH should survive")
	}
	if _, ok := out["LD_PRELOAD"]; ok {
		t.Error("LD_PRELOAD must be dropped")
	}
	if !slices.Contains(dropped, "LD_PRELOAD") ||
		!slices.Contains(dropped, "NODE_OPTIONS") ||
		!slices.Contains(dropped, "DYLD_VERSION") {
		t.Errorf("dropped set wrong: %v", dropped)
	}
}
