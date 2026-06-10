package session

import (
	"context"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

// Spec scenario: 水合后 instructions 仍生效 / metadata 跨水合保留.
func TestManager_InstructionsAndMetadataSurviveHydration(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runner := &fakeRunner{}
	m1 := NewManager(ManagerOptions{Store: st, RunnerFactory: func(*Session) Runner { return runner }})
	sess, err := m1.CreateSession(ctx, Options{
		Workdir:      t.TempDir(),
		Instructions: "当前页面上下文：taskId=T-1024",
		Metadata:     map[string]string{"dataweave_conversation_id": "conv-7f3a"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.Instructions == "" || sess.Metadata["dataweave_conversation_id"] != "conv-7f3a" {
		t.Fatalf("live session lost customization: %+v", sess.Metadata)
	}

	// A fresh manager over the same store simulates a restart.
	m2 := NewManager(ManagerOptions{Store: st, RunnerFactory: func(*Session) Runner { return runner }})
	hyd, err := m2.GetOrHydrate(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetOrHydrate: %v", err)
	}
	if hyd.Instructions != "当前页面上下文：taskId=T-1024" {
		t.Errorf("instructions lost on hydration: %q", hyd.Instructions)
	}
	if hyd.Metadata["dataweave_conversation_id"] != "conv-7f3a" {
		t.Errorf("metadata lost on hydration: %v", hyd.Metadata)
	}
}
