package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/wallfacers/workhorse-agent/internal/config"
	"github.com/wallfacers/workhorse-agent/internal/store"
	"github.com/wallfacers/workhorse-agent/internal/store/sqlite"
)

func openMemStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), sqlite.Options{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func permRows(t *testing.T, st *sqlite.Store) []*store.Permission {
	t.Helper()
	rows, err := st.ListPermissions(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestApplyPresetRules_RetightenReplacesInPlace(t *testing.T) {
	st := openMemStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// First pass: allow.
	if err := applyPresetRules(ctx, st, []config.PresetRule{
		{Tool: "Bash", Pattern: "ls *", Decision: "allow_permanent"},
	}, logger); err != nil {
		t.Fatal(err)
	}
	// Second pass: same (tool, pattern) retightened to deny.
	if err := applyPresetRules(ctx, st, []config.PresetRule{
		{Tool: "Bash", Pattern: "ls *", Decision: "deny_permanent"},
	}, logger); err != nil {
		t.Fatal(err)
	}

	rows := permRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("retighten should replace in place, got %d rows: %+v", len(rows), rows)
	}
	if rows[0].Decision != store.DecisionDenyPermanent {
		t.Errorf("decision should be deny after retighten, got %q", rows[0].Decision)
	}
}

func TestApplyPresetRules_RemovesStaleOnDrop(t *testing.T) {
	st := openMemStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// A manually-created rule must survive reconciliation.
	if err := st.SavePermission(ctx, &store.Permission{
		ID: "perm-manual", Tool: "Read", Pattern: "*",
		Decision: store.DecisionAllowPermanent, Scope: store.ScopePermanent,
	}); err != nil {
		t.Fatal(err)
	}

	if err := applyPresetRules(ctx, st, []config.PresetRule{
		{Tool: "Bash", Pattern: "git *", Decision: "allow_permanent"},
	}, logger); err != nil {
		t.Fatal(err)
	}
	if len(permRows(t, st)) != 2 {
		t.Fatalf("expected manual + preset, got %d", len(permRows(t, st)))
	}

	// Drop the preset from config entirely; reconciliation must delete it but
	// leave the manual rule untouched.
	if err := applyPresetRules(ctx, st, nil, logger); err != nil {
		t.Fatal(err)
	}
	rows := permRows(t, st)
	if len(rows) != 1 || rows[0].ID != "perm-manual" {
		t.Fatalf("stale preset not reconciled, rows=%+v", rows)
	}
}
