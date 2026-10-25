package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// sqliteWorkHookScope accepts only the city or a configured rig. BEADS_DIR is
// retained as a scope selector, never as authorization to open an arbitrary DB.
func (h *hookCity) sqliteWorkHookScope(dir string, env []string) (string, error) {
	selected := dir
	values := hookClaimEnvMap(env, dir, "")
	if root := strings.TrimSpace(values["BEADS_DIR"]); root != "" {
		selected = filepath.Dir(root)
	}
	selected = resolveStoreScopeRoot(h.path, selected)
	if selected == resolveStoreScopeRoot(h.path, h.path) {
		return selected, nil
	}
	if h.cfg != nil {
		for _, rig := range h.cfg.Rigs {
			if selected == resolveStoreScopeRoot(h.path, rig.Path) {
				return selected, nil
			}
		}
	}
	return "", fmt.Errorf("sqlite-work: hook scope %s is not the city or a configured rig", selected)
}

func (h *hookCity) sqliteWorkHookStore(ctx context.Context, dir string, env []string) (*beads.SQLiteWorkStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := h.sqliteWorkHookScope(dir, env)
	if err != nil {
		return nil, err
	}
	return openSQLiteWorkScope(scope, h.path, false)
}

func (h *hookCity) withSQLiteWorkClaimOps(ops hookClaimOps) hookClaimOps {
	ops.Claim = func(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return beads.Bead{}, false, err
		}
		defer store.CloseStore() //nolint:errcheck
		return hookClaimThroughStore(id, actor, func() (beads.Bead, bool, error) { return store.Claim(id, actor) }, store.Get)
	}
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, root, group string) ([]beads.Bead, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return nil, err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.List(beads.ListQuery{Status: "open", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root, beadmeta.ContinuationGroupMetadataKey: group}, TierMode: beads.TierBoth})
	}
	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, id, actor string) error {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.Update(id, beads.UpdateOpts{Assignee: &actor})
	}
	ops.InputDone = func(ctx context.Context, dir string, env []string, candidate beads.Bead, identities []string) (string, bool, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return "", false, err
		}
		defer store.CloseStore() //nolint:errcheck
		return workflowInputDone(store, candidate, identities)
	}
	ops.SkipDoneWorkflow = func(ctx context.Context, dir string, env []string, root string) error {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return err
		}
		defer store.CloseStore() //nolint:errcheck
		return skipDoneWorkflow(store, root)
	}
	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, id, _ string, patch map[string]string) error {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.Update(id, beads.UpdateOpts{Metadata: patch})
	}
	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, id, _ string) (beads.Bead, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return beads.Bead{}, err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.Get(id)
	}
	ops.ConfirmBlocked = func(ctx context.Context, dir string, env []string, id, _ string) (bool, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return false, err
		}
		defer store.CloseStore() //nolint:errcheck
		return beadHasUnmetPlainBlocksDep(store, id)
	}
	ops.Release = func(ctx context.Context, dir string, env []string, id, actor string) (bool, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return false, err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.ReleaseIfCurrent(id, actor)
	}
	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, id string) (bool, string, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return false, "", err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.ReclaimStale(id)
	}
	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, id, from, to string) (bool, error) {
		store, err := h.sqliteWorkHookStore(ctx, dir, env)
		if err != nil {
			return false, err
		}
		defer store.CloseStore() //nolint:errcheck
		return store.TransferIfCurrent(id, from, to)
	}
	ops.WorkflowStore = func(ctx context.Context, dir string, env []string, _, _ string) beads.Store {
		if err := ctx.Err(); err != nil {
			return refusedClassStore{err: err}
		}
		scope, err := h.sqliteWorkHookScope(dir, env)
		if err != nil {
			return refusedClassStore{err: err}
		}
		store, err := openStoreAtForCityWithConfig(scope, h.path, h.cfg)
		if err != nil {
			return refusedClassStore{err: err}
		}
		return store
	}
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, _ string) {
		store, err := h.sqliteWorkHookStore(context.Background(), dir, env)
		if err != nil {
			return
		}
		defer store.CloseStore() //nolint:errcheck
		rec := h.recorder()
		if closer, ok := rec.(interface{ Close() error }); ok {
			defer closer.Close()
		} //nolint:errcheck
		_ = executionevent.EmitLifecycle(rec, store, events.ExecutionStepStarted, step, eventActor())
	}
	return ops
}
