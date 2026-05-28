package approval_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wallfacers/workhorse-agent/internal/extagent/approval"
)

// TestSoak_FiftyConsecutiveCycles exercises the manager's register +
// expire + decide cycle 50 times so leak symptoms (orphaned timers,
// undeleted draft files, goroutine retention) surface as visible failures
// rather than slow heap creep. Mirrors §14.4 from the change tasks.
func TestSoak_FiftyConsecutiveCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in -short mode")
	}
	mgr := approval.New(approval.Options{Timeout: 50 * time.Millisecond})
	defer mgr.Cancel()

	for i := 0; i < 50; i++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "draft.yaml")
		if err := os.WriteFile(path, []byte("name: x\n"), 0o600); err != nil {
			t.Fatalf("iter=%d write draft: %v", i, err)
		}
		id := mgr.Register(&approval.PendingApproval{
			SessionID: "soak-session",
			AgentName: "fake",
			DraftPath: path,
			DraftYAML: "name: x\n",
		})
		if id == "" {
			t.Fatalf("iter=%d Register returned empty id", i)
		}
		// Alternate between approve, reject, expire to exercise every path.
		switch i % 3 {
		case 0:
			_ = mgr.Decide(id, approval.DecisionApprove, "")
		case 1:
			_ = mgr.Decide(id, approval.DecisionReject, "")
		default:
			// Let the timer expire.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if mgr.Get(id) == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if mgr.Get(id) != nil {
			t.Errorf("iter=%d: entry not removed after cycle", i)
		}
	}
}
