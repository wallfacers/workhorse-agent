package main

import (
	"bytes"
	"testing"
)

func TestRunApprove_MissingApprovalID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runApprove([]string{"--session", "sess1"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for missing approval_id")
	}
}

func TestRunApprove_MissingSession(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runApprove([]string{"my-approval"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for missing --session")
	}
}

func TestRunApprove_EditWithoutFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runApprove([]string{"--session", "sess1", "--decision", "edit", "my-approval"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("--decision edit without --file should error")
	}
}

func TestRunApprove_UnknownDecision(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runApprove([]string{"--session", "sess1", "--decision", "maybe", "my-approval"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("unknown --decision should error")
	}
}

func TestRunSetupAgent_MissingBinary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runSetupAgent(nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}
