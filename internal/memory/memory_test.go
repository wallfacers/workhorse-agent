package memory_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

func TestCharCount_CJK(t *testing.T) {
	got := memory.CharCount("你好世界")
	if got != 4 {
		t.Errorf("CharCount CJK: got %d, want 4", got)
	}
	got = memory.CharCount("hello你好")
	if got != 7 {
		t.Errorf("CharCount mixed: got %d, want 7", got)
	}
}

func TestSnapshot_MissingFilesYieldEmpty(t *testing.T) {
	dir := t.TempDir()
	loader := &memory.Loader{ProfileDir: dir}
	snap, err := loader.Load()
	if err != nil {
		t.Fatalf("load from empty dir: %v", err)
	}
	if snap.MemoryMD != "" {
		t.Errorf("memory should be empty, got %q", snap.MemoryMD)
	}
	if snap.UserMD != "" {
		t.Errorf("user should be empty, got %q", snap.UserMD)
	}

	// Files should NOT be created
	memDir := filepath.Join(dir, "memories")
	if _, err := os.Stat(memDir); !os.IsNotExist(err) {
		t.Error("memories dir should not be created by Load")
	}
}

func TestSnapshot_LoadsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, "memories")
	os.MkdirAll(memDir, 0o700)
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("agent facts"), 0o600)
	os.WriteFile(filepath.Join(memDir, "USER.md"), []byte("user prefs"), 0o600)

	loader := &memory.Loader{ProfileDir: dir}
	snap, err := loader.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if snap.MemoryMD != "agent facts" {
		t.Errorf("memory: got %q", snap.MemoryMD)
	}
	if snap.UserMD != "user prefs" {
		t.Errorf("user: got %q", snap.UserMD)
	}
}

func TestWriter_OverLimitRejected(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 10,
		UserLimit:   10,
	}

	err := w.Write(memory.KindMemory, "this is way too long for the limit", memory.ModeReplace)
	if err == nil {
		t.Fatal("expected over-limit error")
	}
	var tooLarge memory.ErrMemoryTooLarge
	if !errorAs(err, &tooLarge) {
		t.Errorf("expected ErrMemoryTooLarge, got %T: %v", err, err)
	}

	// Disk should remain untouched
	content, _ := memory.ReadFile(dir, "memory")
	if content != "" {
		t.Errorf("file should not exist after rejected write, got %q", content)
	}
}

func TestWriter_ReplaceOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 100,
		UserLimit:   100,
	}

	w.Write(memory.KindMemory, "first", memory.ModeReplace)
	w.Write(memory.KindMemory, "second", memory.ModeReplace)

	content, err := memory.ReadFile(dir, "memory")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if content != "second" {
		t.Errorf("got %q, want %q", content, "second")
	}
}

func TestWriter_AppendMode(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 1000,
		UserLimit:   1000,
	}

	w.Write(memory.KindMemory, "line1", memory.ModeReplace)
	w.Write(memory.KindMemory, "line2", memory.ModeAppend)

	content, err := memory.ReadFile(dir, "memory")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if content != "line1\nline2" {
		t.Errorf("got %q, want %q", content, "line1\nline2")
	}
}

func TestWriter_ConcurrentAppendsSameFile(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 10000,
		UserLimit:   10000,
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			w.Write(memory.KindMemory, "append", memory.ModeAppend)
		}(i)
	}
	wg.Wait()

	content, err := memory.ReadFile(dir, "memory")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	count := 0
	for _, line := range splitLines(content) {
		if line == "append" {
			count++
		}
	}
	if count != goroutines {
		t.Errorf("expected %d appends, found %d (content: %q)", goroutines, count, content)
	}
}

func TestWriter_ConcurrentAppendsBothFiles(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 10000,
		UserLimit:   10000,
	}

	const each = 25
	var wg sync.WaitGroup
	wg.Add(each * 2)
	for i := 0; i < each; i++ {
		go func() {
			defer wg.Done()
			w.Write(memory.KindMemory, "m", memory.ModeAppend)
		}()
		go func() {
			defer wg.Done()
			w.Write(memory.KindUser, "u", memory.ModeAppend)
		}()
	}
	wg.Wait()

	memContent, _ := memory.ReadFile(dir, "memory")
	userContent, _ := memory.ReadFile(dir, "user")

	memCount := countOccurrences(memContent, "m")
	userCount := countOccurrences(userContent, "u")

	if memCount != each {
		t.Errorf("memory: expected %d, got %d", each, memCount)
	}
	if userCount != each {
		t.Errorf("user: expected %d, got %d", each, userCount)
	}
}

func TestWriter_InvalidKind(t *testing.T) {
	_, err := memory.ReadFile(t.TempDir(), "badkind")
	if err == nil {
		t.Error("expected error for invalid kind")
	}
}

func TestWriter_CJKCharLimit(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 5,
		UserLimit:   5,
	}

	// 3 CJK chars = 3 code points, should fit
	err := w.Write(memory.KindMemory, "你好世", memory.ModeReplace)
	if err != nil {
		t.Fatalf("3 CJK chars should fit in limit 5: %v", err)
	}

	// 6 CJK chars = 6 code points, should not fit
	err = w.Write(memory.KindMemory, "你好世界再见", memory.ModeReplace)
	if err == nil {
		t.Fatal("6 CJK chars should exceed limit 5")
	}
}

func TestBlock_BothEmpty(t *testing.T) {
	got := memory.Block(&memory.Snapshot{})
	if got != "" {
		t.Errorf("both empty should yield empty string, got %q", got)
	}
}

func TestBlock_OnlyUser(t *testing.T) {
	got := memory.Block(&memory.Snapshot{UserMD: "user content"})
	if got == "" {
		t.Fatal("should not be empty")
	}
	if !contains(got, "USER:") {
		t.Error("should contain USER section")
	}
	if contains(got, "MEMORY:") {
		t.Error("should not contain MEMORY section")
	}
	if contains(got, "---") {
		t.Error("should not contain separator")
	}
}

func TestBlock_OnlyMemory(t *testing.T) {
	got := memory.Block(&memory.Snapshot{MemoryMD: "memory content"})
	if !contains(got, "MEMORY:") {
		t.Error("should contain MEMORY section")
	}
	if contains(got, "USER:") {
		t.Error("should not contain USER section")
	}
	if contains(got, "---") {
		t.Error("should not contain separator")
	}
}

func TestBlock_BothPresent(t *testing.T) {
	snap := &memory.Snapshot{UserMD: "user data", MemoryMD: "memory data"}
	got := memory.Block(snap)
	want := "<memory>\nUSER:\nuser data\n---\nMEMORY:\nmemory data\n</memory>"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBlock_Idempotent(t *testing.T) {
	snap := &memory.Snapshot{UserMD: "u", MemoryMD: "m"}
	a := memory.Block(snap)
	b := memory.Block(snap)
	if a != b {
		t.Error("Block should return byte-identical strings for the same snapshot")
	}
}

func TestBlock_NilSnapshot(t *testing.T) {
	got := memory.Block(nil)
	if got != "" {
		t.Errorf("nil snapshot should yield empty string, got %q", got)
	}
}

// helpers

func errorAs(err error, target interface{}) bool {
	e, ok := err.(memory.ErrMemoryTooLarge)
	if !ok {
		return false
	}
	*target.(*memory.ErrMemoryTooLarge) = e
	return true
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func countOccurrences(s, sub string) int {
	count := 0
	for {
		i := containsAt(s, sub)
		if i < 0 {
			break
		}
		count++
		s = s[i+len(sub):]
	}
	return count
}

func containsAt(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func contains(s, sub string) bool {
	return containsAt(s, sub) >= 0
}

var _ = utf8.RuneCountInString
