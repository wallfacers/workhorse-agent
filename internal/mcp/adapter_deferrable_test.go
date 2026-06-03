package mcp

import "testing"

func TestAdapter_ShouldDefer_Default(t *testing.T) {
	st := ServerTool{Server: "slack", Def: ToolDef{Name: "send"}, inst: &serverInstance{config: ServerConfig{Name: "slack"}}}
	a := NewAdapter(st)
	if !a.ShouldDefer() {
		t.Error("MCP tool without always_load should default to deferred")
	}
}

func TestAdapter_ShouldDefer_AlwaysLoad(t *testing.T) {
	st := ServerTool{Server: "core", Def: ToolDef{Name: "ping"}, inst: &serverInstance{config: ServerConfig{Name: "core", AlwaysLoad: true}}}
	a := NewAdapter(st)
	if a.ShouldDefer() {
		t.Error("server with always_load:true should opt out of deferral")
	}
}

func TestAdapter_ShouldDefer_NilInst(t *testing.T) {
	a := NewAdapter(ServerTool{Server: "x", Def: ToolDef{Name: "y"}})
	if !a.ShouldDefer() {
		t.Error("nil inst should default to deferred (always_load false)")
	}
}
