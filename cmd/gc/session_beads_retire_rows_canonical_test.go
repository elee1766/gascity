package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestRetireDuplicateRows_ClearsLoserCanonicalIdentity pins that the typed
// duplicate-retire path (retireDuplicateConfiguredNamedSessionRows) persists
// RetireNamedSessionPatch's canonical-identity clears on the losing row and
// leaves the winner's canonical identity intact, so the retired duplicate no
// longer claims the named session's canonical instance name or pool slot.
func TestRetireDuplicateRows_ClearsLoserCanonicalIdentity(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	cityName := config.EffectiveCityName(cfg, "")
	spec, ok := session.FindNamedSessionSpec(cfg, cityName, "mayor")
	if !ok {
		t.Fatalf("named spec for mayor not found; fixture cfg no longer resolves it")
	}
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	store := beads.NewMemStore()
	mkSession := func(gen, sessName string, canonical string) string {
		b, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Status: "open",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                            "mayor",
				"configured_named_session":            "true",
				"configured_named_identity":           "mayor",
				"generation":                          gen,
				"session_name":                        sessName,
				session.CanonicalInstanceNameMetadata: canonical,
				session.CanonicalPoolSlotMetadata:     "1",
			},
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return b.ID
	}
	winner := mkSession("5", spec.SessionName, spec.SessionName)         // canonical name + higher generation → wins
	loser := mkSession("3", spec.SessionName, spec.SessionName+"-stale") // retired duplicate

	rows := []session.ReconcileSession{
		{Info: sessiontest.SeedBead(t, mustGet(t, store, winner))},
		{Info: sessiontest.SeedBead(t, mustGet(t, store, loser))},
	}

	retireDuplicateConfiguredNamedSessionRows("", store, testSeatWork("", cfg, store, nil), runtime.NewFake(), cfg, cityName, rows, now, nil)

	loserBead := mustGet(t, store, loser)
	for _, key := range []string{session.CanonicalInstanceNameMetadata, session.CanonicalPoolSlotMetadata} {
		if got := loserBead.Metadata[key]; got != "" {
			t.Errorf("loser %s = %q after retirement, want cleared", key, got)
		}
	}
	winnerBead := mustGet(t, store, winner)
	if got := winnerBead.Metadata[session.CanonicalInstanceNameMetadata]; got != spec.SessionName {
		t.Errorf("winner %s = %q, want %q (winner is not retired)", session.CanonicalInstanceNameMetadata, got, spec.SessionName)
	}
	if got := winnerBead.Metadata[session.CanonicalPoolSlotMetadata]; got != "1" {
		t.Errorf("winner %s = %q, want %q (winner is not retired)", session.CanonicalPoolSlotMetadata, got, "1")
	}
}

func TestRetirementPreservesLinkedAndBlockedWork(t *testing.T) {
	for _, mode := range []string{"beads", "rows", "close"} {
		t.Run(mode, func(t *testing.T) {
			store := stampedSQLite(t, gate.Require)
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}, NamedSessions: []config.NamedSession{{Template: "worker"}}}
			cityName := config.EffectiveCityName(cfg, "")
			spec, ok := session.FindNamedSessionSpec(cfg, cityName, "worker")
			if !ok {
				t.Fatal("missing named worker")
			}
			makeSession := func(generation, name string) beads.Bead {
				b, err := store.Create(beads.Bead{Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{
					"template": "worker", "configured_named_session": "true", "configured_named_identity": "worker", "generation": generation, "session_name": name,
				}})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			winner := makeSession("5", spec.SessionName)
			loser := makeSession("3", spec.SessionName+"-old")
			source := createWork(t, store, "open", loser.ID, map[string]string{"gc.execution_routed_to": "worker"})
			step := createWork(t, store, "blocked", loser.ID, map[string]string{"gc.root_bead_id": "root", "gc.session_id": loser.ID})
			if err := store.Update(step.ID, beads.UpdateOpts{Labels: []string{"hold:mayor"}}); err != nil {
				t.Fatal(err)
			}
			step = mustGet(t, store, step.ID)
			sw := testSeatWork("", cfg, store, nil)
			now := time.Now()
			switch mode {
			case "beads":
				retireDuplicateConfiguredNamedSessionBeads("", store, sw, runtime.NewFake(), cfg, cityName, []beads.Bead{winner, loser}, map[string]beads.Bead{}, map[string]int{}, now, io.Discard)
			case "rows":
				rows := []session.ReconcileSession{{Info: sessiontest.SeedBead(t, winner)}, {Info: sessiontest.SeedBead(t, loser)}}
				retireDuplicateConfiguredNamedSessionRows("", store, sw, runtime.NewFake(), cfg, cityName, rows, now, io.Discard)
			case "close":
				if err := store.Close(source.ID); err != nil {
					t.Fatal(err)
				}
				if closeSeat(store, sw, sessiontest.SeedBead(t, loser), closeWhy{reason: "drained", gate: closeGated}, now, io.Discard) {
					t.Fatal("closed a seat that owns blocked work")
				}
			}
			if got := mustGet(t, store, loser.ID); got.Revision != loser.Revision {
				t.Fatal("retirement mutated the old session")
			}
			if mode != "close" {
				if got := mustGet(t, store, source.ID); got.Revision != source.Revision {
					t.Fatal("retirement moved the source without its held step")
				}
			}
			if got := mustGet(t, store, step.ID); got.Revision != step.Revision {
				t.Fatal("held step or its ownership changed")
			}
		})
	}
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

type stopHookProvider struct {
	*runtime.Fake
	onStop func()
}

func (p *stopHookProvider) Stop(name string) error {
	if p.onStop != nil {
		p.onStop()
		p.onStop = nil
	}
	return p.Fake.Stop(name)
}

// Work that becomes held while the old runtime is being stopped must not be
// split from its session by the retirement that follows the stop.
func TestRetirementRefusesWorkHeldDuringRuntimeStop(t *testing.T) {
	for _, mode := range []string{"beads", "rows"} {
		t.Run(mode, func(t *testing.T) {
			cityPath := t.TempDir()
			store := stampedSQLite(t, gate.Require)
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}, NamedSessions: []config.NamedSession{{Template: "worker"}}}
			cityName := config.EffectiveCityName(cfg, "")
			spec, ok := session.FindNamedSessionSpec(cfg, cityName, "worker")
			if !ok {
				t.Fatal("missing named worker")
			}
			makeSession := func(generation, name string) beads.Bead {
				b, err := store.Create(beads.Bead{Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{
					"template": "worker", "configured_named_session": "true", "configured_named_identity": "worker", "generation": generation, "session_name": name,
				}})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			winner := makeSession("5", spec.SessionName)
			loser := makeSession("3", spec.SessionName+"-old")
			work := createWork(t, store, "in_progress", loser.ID, nil)
			sp := &stopHookProvider{Fake: runtime.NewFake()}
			if err := sp.Start(context.Background(), spec.SessionName+"-old", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			sp.onStop = func() {
				if err := store.Update(work.ID, beads.UpdateOpts{Labels: []string{"hold:mayor"}}); err != nil {
					t.Fatal(err)
				}
			}
			sw := testSeatWork("", cfg, store, nil)
			now := time.Now()
			switch mode {
			case "beads":
				retireDuplicateConfiguredNamedSessionBeads(cityPath, store, sw, sp, cfg, cityName, []beads.Bead{winner, loser}, map[string]beads.Bead{}, map[string]int{}, now, io.Discard)
			case "rows":
				rows := []session.ReconcileSession{{Info: sessiontest.SeedBead(t, winner)}, {Info: sessiontest.SeedBead(t, loser)}}
				retireDuplicateConfiguredNamedSessionRows(cityPath, store, sw, sp, cfg, cityName, rows, now, io.Discard)
			}
			if sp.onStop != nil {
				t.Fatal("retirement did not stop the old runtime")
			}
			if got := mustGet(t, store, work.ID); got.Assignee != loser.ID {
				t.Fatalf("held work assignee = %q, want %q", got.Assignee, loser.ID)
			}
			if got := mustGet(t, store, loser.ID); got.Metadata["retired_named_identity"] != "" {
				t.Fatal("retired the old session while it owns held work")
			}
		})
	}
}

// reread hooks the live re-read so a hold can land between it and the write.
type rereadHookStore struct {
	beads.Store
	afterGet func()
}

func (s *rereadHookStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if s.afterGet != nil {
		s.afterGet()
		s.afterGet = nil
	}
	return b, err
}

// rereadRevisionStore keeps MemStore's revision fence and hooks its re-read.
type rereadRevisionStore struct {
	*beads.MemStore
	hook *rereadHookStore
}

func (s rereadRevisionStore) Get(id string) (beads.Bead, error) { return s.hook.Get(id) }

// rereadAssignmentStore guards only status and assignee, like BdStore.
type rereadAssignmentStore struct{ *rereadHookStore }

func (s rereadAssignmentStore) UpdateIfAssignment(id, status, assignee string, opts beads.UpdateOpts) (bool, error) {
	cur, err := s.Store.Get(id)
	if err != nil || cur.Status != status || cur.Assignee != assignee {
		return false, err
	}
	return true, s.Update(id, opts)
}

// A hold landing between the live re-read and the write must not let the
// retirement move the work, whatever fence the store offers.
func TestReassignRetiredWorkRefusesHoldAfterReread(t *testing.T) {
	for _, mode := range []string{"revision", "assignment-only"} {
		t.Run(mode, func(t *testing.T) {
			base := beads.NewMemStore()
			work := createWork(t, base, "in_progress", "old", nil)
			hook := &rereadHookStore{Store: base}
			var store beads.Store = rereadRevisionStore{MemStore: base, hook: hook}
			if mode == "assignment-only" {
				store = rereadAssignmentStore{hook}
			}
			wa := workAssignmentForStore(beads.WorkStore{Store: store})
			hook.afterGet = func() {
				if err := base.Update(work.ID, beads.UpdateOpts{Labels: []string{"hold:mayor"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := wa.ReassignRetiredWorkBead(work, "new", retirementWorkHeld); err == nil {
				t.Fatal("reassignment of work held after the re-read reported success")
			}
			if got := mustGet(t, base, work.ID); got.Assignee != "old" {
				t.Fatalf("held work assignee = %q, want old", got.Assignee)
			}
			if mode == "revision" {
				free := createWork(t, base, "in_progress", "old", nil)
				if err := wa.ReassignRetiredWorkBead(free, "new", retirementWorkHeld); err != nil {
					t.Fatalf("standalone reassignment: %v", err)
				}
				if got := mustGet(t, base, free.ID); got.Assignee != "new" {
					t.Fatalf("standalone work assignee = %q, want new", got.Assignee)
				}
			}
		})
	}
}
