package config

import (
	"reflect"
	"testing"
)

func TestDiffReloadable_PermissionSubset(t *testing.T) {
	old := Default()
	nw := Default()
	nw.Tools.PresetRules = []PresetRule{{Tool: "Bash", Pattern: "git *", Decision: "allow_permanent"}}
	nw.Tools.DefaultPermission = "deny_permanent"
	nw.Agent.PermissionRequestTimeoutSeconds = 120

	d := DiffReloadable(old, nw)
	if !d.PresetRulesChanged || !d.DefaultPermissionChanged || !d.TimeoutChanged {
		t.Fatalf("expected all permission fields changed, got %+v", d)
	}
	if len(d.NonReloadable) != 0 {
		t.Errorf("expected no non-reloadable changes, got %v", d.NonReloadable)
	}
	if !d.HasReloadable() {
		t.Error("HasReloadable should be true")
	}
}

func TestDiffReloadable_NonReloadablePort(t *testing.T) {
	old := Default()
	nw := Default()
	nw.Server.Port = 9000

	d := DiffReloadable(old, nw)
	if d.HasReloadable() {
		t.Errorf("port change must not be a reloadable change, got %+v", d)
	}
	if !reflect.DeepEqual(d.NonReloadable, []string{"server.port"}) {
		t.Errorf("NonReloadable = %v, want [server.port]", d.NonReloadable)
	}
}

func TestDiffReloadable_StorePathNonReloadable(t *testing.T) {
	old := Default()
	nw := Default()
	nw.Store.Path = "/tmp/other.db"
	d := DiffReloadable(old, nw)
	found := false
	for _, n := range d.NonReloadable {
		if n == "store.path" {
			found = true
		}
	}
	if !found {
		t.Errorf("store.path change should be reported, got %v", d.NonReloadable)
	}
}

func TestDiffReloadable_NoChange(t *testing.T) {
	d := DiffReloadable(Default(), Default())
	if d.HasReloadable() || len(d.NonReloadable) != 0 {
		t.Errorf("identical configs should report no diff, got %+v", d)
	}
}
