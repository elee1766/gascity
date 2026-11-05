package main

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A step reclaimed from a dead session must also release that session's claim
// on the workflow's source ticket in the rig store, keeping its status and
// hold, while a source held by another owner is untouched (trd-80897).
func TestReleaseOrphanedPoolAssignments_ReleasesDeadOwnerSource(t *testing.T) {
	graph := beads.NewMemStore()
	rig := beads.NewMemStore()
	const dead = "worker-dead"

	held, err := rig.Create(beads.Bead{
		Title: "source", Assignee: dead, Labels: []string{beadmeta.HoldExternalLabel},
		Metadata: map[string]string{workLeaseExpiresMetadataKey: "2026-10-25T07:00:00Z"},
	})
	requireNoError(t, err)
	requireNoError(t, rig.Update(held.ID, beads.UpdateOpts{Status: stringPtr("blocked")}))
	other, err := rig.Create(beads.Bead{Title: "other owner", Assignee: "worker-live"})
	requireNoError(t, err)
	requireNoError(t, rig.Update(other.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}))
	conv, err := rig.Create(beads.Bead{Title: "input convoy", Type: "convoy"})
	requireNoError(t, err)
	requireNoError(t, rig.DepAdd(conv.ID, held.ID, "tracks"))
	requireNoError(t, rig.DepAdd(conv.ID, other.ID, "tracks"))

	root, err := graph.Create(beads.Bead{Title: "workflow", Metadata: map[string]string{
		beadmeta.InputConvoyIDMetadataKey: conv.ID, beadmeta.RootStoreRefMetadataKey: "rig:trade",
	}})
	requireNoError(t, err)
	step, err := graph.Create(beads.Bead{Title: "deliver", Assignee: dead, Metadata: map[string]string{
		"gc.routed_to": "worker", beadmeta.RootBeadIDMetadataKey: root.ID,
	}})
	requireNoError(t, err)
	requireNoError(t, graph.Update(step.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}))
	step, err = graph.Get(step.ID)
	requireNoError(t, err)

	released := releaseOrphanedPoolAssignments(graph, beads.SessionStore{Store: graph}, testPoolReleaseConfig(), "", nil,
		[]beads.Bead{step}, []beads.Store{graph}, []string{""}, map[string]beads.Store{"trade": rig}, nil, nil, nil)
	if len(released) != 1 || released[0].ID != step.ID {
		t.Fatalf("released = %+v, want step %s", released, step.ID)
	}

	got, err := rig.Get(held.ID)
	requireNoError(t, err)
	if got.Assignee != "" || got.Metadata[workLeaseExpiresMetadataKey] != "" {
		t.Fatalf("source still owned: assignee=%q lease=%q", got.Assignee, got.Metadata[workLeaseExpiresMetadataKey])
	}
	if got.Status != "blocked" || !slices.Contains(got.Labels, beadmeta.HoldExternalLabel) {
		t.Fatalf("source status/hold changed: status=%q labels=%v", got.Status, got.Labels)
	}
	if got.Metadata[sourceReclaimedFromMetadataKey] != dead || got.Metadata[sourceReclaimedByMetadataKey] != step.ID {
		t.Fatalf("missing reclaim note: %v", got.Metadata)
	}
	if o, _ := rig.Get(other.ID); o.Assignee != "worker-live" || o.Status != "in_progress" {
		t.Fatalf("other owner's source changed: %+v", o)
	}
	// Idempotent: a second pass over the already-released source is a no-op.
	if again := releaseDeadOwnerSourceClaims(graph, map[string]beads.Store{"trade": rig}, graph, step, got.UpdatedAt); len(again) != 0 {
		t.Fatalf("second release = %v, want none", again)
	}
}
