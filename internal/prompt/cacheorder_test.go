package prompt_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/prompt"
)

// These tests lock the optimize-prompt-cache-order contract: the static base段
// (Base + CancelledNote) is always the prefix, with the dynamic environment and
// memory blocks appended after, in that order. They map directly to the
// agent-loop "System prompt 组装顺序优先静态前缀" and prompt-memory
// "System prompt injection" spec scenarios.

const (
	envBlock = "<environment>\nos: linux\ncwd: /tmp\n</environment>"
	memBlock = "<memory>\n# MEMORY\nremember this\n</memory>"
)

func baseSeg(base string) string {
	if base == "" {
		return prompt.CancelledNote
	}
	return base + "\n\n" + prompt.CancelledNote
}

// Scenario: 静态 base 作为缓存前缀 — base, environment, memory all non-empty.
func TestSystemPrompt_OrderBaseEnvMemory(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Base:        "BASE PROMPT",
		Environment: envBlock,
		Memory:      memBlock,
	})
	want := baseSeg("BASE PROMPT") + "\n\n" + envBlock + "\n\n" + memBlock
	if got != want {
		t.Errorf("order mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if !strings.HasPrefix(got, baseSeg("BASE PROMPT")) {
		t.Error("system prompt must start with the static base段")
	}
	if strings.Index(got, envBlock) > strings.Index(got, memBlock) {
		t.Error("environment must precede memory")
	}
}

// Task 3.2 — static base+environment is a stable cache prefix: two inputs that
// differ only in memory share the entire base段+environment as a common prefix.
func TestSystemPrompt_StaticPrefixAcrossMemoryChange(t *testing.T) {
	in := prompt.SystemPromptInput{Base: "BASE PROMPT", Environment: envBlock}
	a := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: in.Base, Environment: in.Environment, Memory: "<memory>first</memory>"})
	b := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: in.Base, Environment: in.Environment, Memory: "<memory>second</memory>"})

	staticPrefix := baseSeg(in.Base) + "\n\n" + in.Environment
	if !strings.HasPrefix(a, staticPrefix) || !strings.HasPrefix(b, staticPrefix) {
		t.Fatal("both renders must start with the static base段+environment prefix")
	}
	lcp := longestCommonPrefix(a, b)
	if !strings.HasPrefix(lcp, staticPrefix) {
		t.Errorf("longest common prefix must cover the entire base+environment段:\nlcp:    %q\nstatic: %q", lcp, staticPrefix)
	}
}

// Task 3.3 / Scenario 仅有 base 段 — environment and memory empty.
func TestSystemPrompt_OnlyBase(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: "BASE PROMPT"})
	want := baseSeg("BASE PROMPT")
	if got != want {
		t.Errorf("only-base mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "<environment>") || strings.Contains(got, "<memory>") {
		t.Error("only-base render must not contain environment/memory framing")
	}
}

// Scenario: Empty memory produces no memory section (prompt-memory spec) — base
// and environment present, memory empty.
func TestSystemPrompt_EmptyMemoryNoSection(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: "BASE PROMPT", Environment: envBlock})
	want := baseSeg("BASE PROMPT") + "\n\n" + envBlock
	if got != want {
		t.Errorf("empty-memory mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "<memory>") {
		t.Error("empty memory must produce no memory section")
	}
	if strings.HasSuffix(got, "\n\n") {
		t.Error("empty memory must not leave a dangling delimiter")
	}
}

// Task 3.4 — the memory block is byte-stable: it renders verbatim regardless of
// whether the environment block is present, so the same-session memory bytes
// never drift.
func TestSystemPrompt_MemoryBlockByteStable(t *testing.T) {
	withEnv := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: "BASE", Environment: envBlock, Memory: memBlock})
	noEnv := prompt.BuildSystemPrompt(prompt.SystemPromptInput{Base: "BASE", Memory: memBlock})
	if !strings.HasSuffix(withEnv, "\n\n"+memBlock) || !strings.HasSuffix(noEnv, "\n\n"+memBlock) {
		t.Error("memory block must appear verbatim as the suffix, joined by a stable delimiter")
	}
}

// --- Instructions segment tests (add-agents-md-support) ---

const instrBlock = "<instructions>\nInstructions from: /project/AGENTS.md\nuse Go\n</instructions>"

// Scenario: Full assembly order B → Cancel → Env → Instructions → Memory.
func TestSystemPrompt_FullOrderWithInstructions(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Base:         "BASE PROMPT",
		Environment:  envBlock,
		Instructions: instrBlock,
		Memory:       memBlock,
	})
	want := baseSeg("BASE PROMPT") + "\n\n" + envBlock + "\n\n" + instrBlock + "\n\n" + memBlock
	if got != want {
		t.Errorf("full order mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// Scenario: Instructions only (no environment, no memory).
func TestSystemPrompt_InstructionsOnly(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Instructions: instrBlock,
	})
	want := prompt.CancelledNote + "\n\n" + instrBlock
	if got != want {
		t.Errorf("instructions-only mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// Scenario: Instructions positioned between environment and memory.
func TestSystemPrompt_InstructionsBetweenEnvAndMemory(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Base:         "BASE",
		Environment:  envBlock,
		Instructions: instrBlock,
		Memory:       memBlock,
	})
	envIdx := strings.Index(got, envBlock)
	instrIdx := strings.Index(got, instrBlock)
	memIdx := strings.Index(got, memBlock)
	if !(envIdx < instrIdx && instrIdx < memIdx) {
		t.Errorf("order must be env < instructions < memory, got env@%d instr@%d mem@%d", envIdx, instrIdx, memIdx)
	}
}

// Scenario: Empty instructions produce no framing.
func TestSystemPrompt_EmptyInstructionsNoSection(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Base:         "BASE PROMPT",
		Environment:  envBlock,
		Instructions: "",
		Memory:       memBlock,
	})
	if strings.Contains(got, "<instructions>") {
		t.Error("empty instructions must produce no instructions section")
	}
	want := baseSeg("BASE PROMPT") + "\n\n" + envBlock + "\n\n" + memBlock
	if got != want {
		t.Errorf("empty-instructions mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// Scenario: 顺序变更不改变内容集合 — the set of "\n\n"-joined segments equals the
// fixed multiset {base, CancelledNote, environment, instructions, memory} regardless of order.
func TestSystemPrompt_SameContentSet(t *testing.T) {
	got := prompt.BuildSystemPrompt(prompt.SystemPromptInput{
		Base:        "BASE PROMPT",
		Environment: envBlock,
		Memory:      memBlock,
	})
	gotSegs := strings.Split(got, "\n\n")
	wantSegs := []string{"BASE PROMPT", prompt.CancelledNote, envBlock, memBlock}
	sort.Strings(gotSegs)
	sort.Strings(wantSegs)
	if strings.Join(gotSegs, "\x00") != strings.Join(wantSegs, "\x00") {
		t.Errorf("content set drifted:\ngot:  %q\nwant: %q", gotSegs, wantSegs)
	}
}

func longestCommonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
