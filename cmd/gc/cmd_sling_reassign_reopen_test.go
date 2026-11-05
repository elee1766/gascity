package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestOnFormulaReassignReopensOrderClaimedBead checks that a formula-backed
// handoff is refused without clearing the order's claim or publishing routing.
// A source and its workflow cannot be handed off through separate writes.
func TestOnFormulaReassignReopensOrderClaimedBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(2)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "hotspot work", Type: "task", Status: "in_progress", Assignee: "order:mol-dog-jsonl"},
	}, nil)

	opts := testOpts(a, "BL-42")
	opts.OnFormula = "mol-polecat-work"
	opts.NoConvoy = true
	opts.Reassign = true

	code := doSling(opts, deps, deps.Store, stdout, stderr)
	if code == 0 {
		t.Fatalf("formula reassignment succeeded unexpectedly; stdout: %s", stdout.String())
	}

	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if got := source.Metadata["gc.routed_to"]; got != "" {
		t.Errorf("gc.routed_to = %q, want no published route", got)
	}
	if source.Assignee != "order:mol-dog-jsonl" {
		t.Errorf("Assignee = %q, want original order owner", source.Assignee)
	}
	if source.Status != "in_progress" {
		t.Errorf("Status = %q, want original in_progress state", source.Status)
	}
}
