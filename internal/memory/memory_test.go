package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallfacers/workhorse-agent/internal/memory"
)

// shared test helpers for the package.

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustNow() time.Time { return time.Now().UTC() }

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

func TestBudgets_CheckEntryContent_CJK(t *testing.T) {
	b := memory.Budgets{EntryContentChars: 2}
	// "你好" = 2 code points → within limit.
	if err := b.CheckEntryContent("你好"); err != nil {
		t.Fatalf("2 CJK code points should fit limit 2: %v", err)
	}
	// "你好世" = 3 code points → over limit.
	err := b.CheckEntryContent("你好世")
	if err == nil {
		t.Fatal("3 CJK code points should exceed limit 2")
	}
	var tooLarge memory.ErrMemoryTooLarge
	if !errorAs(err, &tooLarge) {
		t.Fatalf("expected ErrMemoryTooLarge, got %T: %v", err, err)
	}
	if tooLarge.Limit != 2 || tooLarge.Actual != 3 {
		t.Fatalf("expected limit=2 actual=3, got limit=%d actual=%d", tooLarge.Limit, tooLarge.Actual)
	}
}

func TestBudgets_CheckTrigger(t *testing.T) {
	b := memory.Budgets{TriggerChars: 5}
	if err := b.CheckTrigger("hi"); err != nil {
		t.Fatalf("short trigger should pass: %v", err)
	}
	// Over length.
	var ti memory.ErrTriggerInvalid
	err := b.CheckTrigger("toolong")
	if err == nil {
		t.Fatal("over-length trigger should be rejected")
	}
	if !errorAsTrigger(err, &ti) {
		t.Fatalf("expected ErrTriggerInvalid, got %T", err)
	}
	// Newline.
	err = b.CheckTrigger("a\nb")
	if err == nil {
		t.Fatal("trigger with newline should be rejected")
	}
	if !errorAsTrigger(err, &ti) {
		t.Fatalf("expected ErrTriggerInvalid for newline, got %T", err)
	}
}

func TestPinnedCharTotal(t *testing.T) {
	es, _ := newEntryStore(t)
	ctx := context.Background()
	must(t, es.Upsert(ctx, &memory.Entry{Name: "p1", Content: "abc", Pinned: true, CharCount: 3}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "p2", Content: "de", Pinned: true, CharCount: 2}))
	must(t, es.Upsert(ctx, &memory.Entry{Name: "n1", Content: "ignored", Pinned: false, CharCount: 7}))

	total, err := es.PinnedCharTotal(ctx, "")
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != 5 {
		t.Fatalf("PinnedCharTotal(all) = %d, want 5", total)
	}
	// Excluding p1 (the upsert-increment case).
	total, err = es.PinnedCharTotal(ctx, "p1")
	if err != nil {
		t.Fatalf("total exclude: %v", err)
	}
	if total != 2 {
		t.Fatalf("PinnedCharTotal(exclude p1) = %d, want 2", total)
	}
	// Empty store edge: exclude everything still 0 on a fresh store handled by COALESCE.
	es2, _ := newEntryStore(t)
	if got, err := es2.PinnedCharTotal(ctx, ""); err != nil || got != 0 {
		t.Fatalf("empty store PinnedCharTotal = %d, err %v; want 0", got, err)
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

func TestWriter_AppendToNonexistentFile(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 1000,
		UserLimit:   1000,
	}

	err := w.Write(memory.KindMemory, "first append", memory.ModeAppend)
	if err != nil {
		t.Fatalf("append to nonexistent file: %v", err)
	}

	content, err := memory.ReadFile(dir, "memory")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if content != "first append" {
		t.Errorf("got %q, want %q", content, "first append")
	}
}

func TestWriter_WriteEmptyContent(t *testing.T) {
	dir := t.TempDir()
	w := &memory.Writer{
		ProfileDir:  dir,
		MemoryLimit: 100,
		UserLimit:   100,
	}

	err := w.Write(memory.KindMemory, "", memory.ModeReplace)
	if err != nil {
		t.Fatalf("write empty: %v", err)
	}

	content, _ := memory.ReadFile(dir, "memory")
	if content != "" {
		t.Errorf("got %q, want empty", content)
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

func TestBlock_OnlyPinned(t *testing.T) {
	got := memory.Block(&memory.Snapshot{Pinned: "pinned content"})
	if got == "" {
		t.Fatal("should not be empty")
	}
	if !contains(got, "PINNED:") {
		t.Error("should contain PINNED section")
	}
	if contains(got, "INDEX:") {
		t.Error("should not contain INDEX section")
	}
	if contains(got, "---") {
		t.Error("should not contain separator")
	}
}

func TestBlock_OnlyIndex(t *testing.T) {
	got := memory.Block(&memory.Snapshot{Index: "- a — t"})
	if !contains(got, "INDEX:") {
		t.Error("should contain INDEX section")
	}
	if contains(got, "PINNED:") {
		t.Error("should not contain PINNED section")
	}
	if contains(got, "---") {
		t.Error("should not contain separator")
	}
}

func TestBlock_BothPresent(t *testing.T) {
	snap := &memory.Snapshot{Pinned: "pinned data", Index: "- a — t"}
	got := memory.Block(snap)
	want := "<memory>\nPINNED:\npinned data\n---\nINDEX:\n- a — t\n</memory>"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBlock_Idempotent(t *testing.T) {
	snap := &memory.Snapshot{Pinned: "p", Index: "- a — t"}
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

func errorAsTrigger(err error, target *memory.ErrTriggerInvalid) bool {
	e, ok := err.(memory.ErrTriggerInvalid)
	if !ok {
		return false
	}
	*target = e
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
