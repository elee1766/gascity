package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

// hookClaimStoreOpener opens one configured work leg, preserving the work
// query environment and the actor of this operation. The caller releases its
// handle through closeHookClaimStore; invocation-memo handles remain borrowed.
type hookClaimStoreOpener func(context.Context, string, []string, string) (beads.Store, error)

// hookOpenConfiguredStore serves callers without an already resolved hookCity.
// The production hook binds its resolved city and config in claimOps instead.
func hookOpenConfiguredStore(ctx context.Context, dir string, env []string, actor string) (beads.Store, error) {
	return newHookCity(cityForStoreDir(dir), nil).openClaimStore(ctx, dir, env, actor)
}

// openClaimStore selects the configured provider for the physical query scope.
// Only the bd leaf constructs a command-backed adapter, bound to the original
// directory, exact query environment, deadline and actor.
func (h *hookCity) openClaimStore(ctx context.Context, dir string, env []string, actor string) (beads.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := h.claimStoreScope(dir, env)
	if err != nil {
		return nil, err
	}
	store, err := openStoreAtForCityWithConfigAndBdOpener(ctx, scope, h.path, h.cfg, func() (beads.Store, error) {
		return hookClaimBdStoreContext(ctx, dir, env, actor), nil
	})
	if err != nil {
		return nil, err
	}
	return beads.WithClaimActor(store, actor), nil
}

// claimStoreScope treats BEADS_DIR as a scope selector, never authorization to
// open an arbitrary SQLite ledger. Bd commands retain the original selector.
func (h *hookCity) claimStoreScope(dir string, env []string) (string, error) {
	selected := dir
	values := hookClaimEnvMap(env, dir, "")
	if root := strings.TrimSpace(values["BEADS_DIR"]); root != "" {
		if !filepath.IsAbs(root) {
			root = filepath.Join(dir, root)
		}
		selected = filepath.Dir(root)
	}
	scope := resolveStoreScopeRoot(h.path, selected)
	provider := rawBeadsProviderForScope(scope, h.path)
	if provider != sqliteWorkProvider {
		return scope, nil
	}
	if scope == resolveStoreScopeRoot(h.path, h.path) {
		return scope, nil
	}
	cfg := h.cfg
	if cfg == nil {
		var err error
		cfg, err = loadCityConfigForStoreOpen(h.path, provider, nil)
		if err != nil {
			return "", err
		}
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if scope == resolveStoreScopeRoot(h.path, rig.Path) {
				return scope, nil
			}
		}
	}
	return "", fmt.Errorf("sqlite-work: hook scope %s is not the city or a configured rig", scope)
}

// hookClaimStoreOps is the single operation set for every configured provider.
// Provider selection and command environment belong to its opener, not to the
// individual hook operations or the deferred announcement pass.
type hookClaimStoreOps struct {
	open hookClaimStoreOpener
}

func closeHookClaimStore(store beads.Store) {
	_ = closeBeadStoreHandle(store)
}

func (s hookClaimStoreOps) claim(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, bool, error) {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return beads.Bead{}, false, err
	}
	defer closeHookClaimStore(store)
	claimer, ok := store.(beads.AssignmentClaimer)
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("claiming through configured store %T: %w", store, beads.ErrClaimUnsupported)
	}
	return hookClaimThroughStore(id, actor,
		func() (beads.Bead, bool, error) { return claimer.Claim(id) }, store.Get)
}

func (s hookClaimStoreOps) listContinuation(ctx context.Context, dir string, env []string, root, group string) ([]beads.Bead, error) {
	store, err := s.open(ctx, dir, env, "")
	if err != nil {
		return nil, err
	}
	defer closeHookClaimStore(store)
	return store.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        root,
			beadmeta.ContinuationGroupMetadataKey: group,
		},
		TierMode: beads.TierBoth,
	})
}

func (s hookClaimStoreOps) assignContinuation(ctx context.Context, dir string, env []string, id, actor string) error {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return err
	}
	defer closeHookClaimStore(store)
	return store.Update(id, beads.UpdateOpts{Assignee: &actor})
}

func (s hookClaimStoreOps) inputDone(ctx context.Context, dir string, env []string, candidate beads.Bead, identities []string) (string, bool, error) {
	store, err := s.open(ctx, dir, env, "")
	if err != nil {
		return "", false, err
	}
	defer closeHookClaimStore(store)
	return workflowInputDone(store, candidate, identities)
}

func (s hookClaimStoreOps) skipDoneWorkflow(ctx context.Context, dir string, env []string, root string) error {
	store, err := s.open(ctx, dir, env, "")
	if err != nil {
		return err
	}
	defer closeHookClaimStore(store)
	return skipDoneWorkflow(store, root)
}

func (s hookClaimStoreOps) stampWorkMeta(ctx context.Context, dir string, env []string, id, actor string, patch map[string]string) error {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return err
	}
	defer closeHookClaimStore(store)
	return store.Update(id, beads.UpdateOpts{Metadata: patch})
}

func (s hookClaimStoreOps) readWorkMeta(ctx context.Context, dir string, env []string, id, actor string) (beads.Bead, error) {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return beads.Bead{}, err
	}
	defer closeHookClaimStore(store)
	return store.Get(id)
}

func (s hookClaimStoreOps) confirmBlocked(ctx context.Context, dir string, env []string, id, actor string) (bool, error) {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return false, err
	}
	defer closeHookClaimStore(store)
	return beadHasUnmetPlainBlocksDep(store, id)
}

func (s hookClaimStoreOps) release(ctx context.Context, dir string, env []string, id, actor string) (bool, error) {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return false, err
	}
	defer closeHookClaimStore(store)
	releaser, ok := store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, fmt.Errorf("releasing through configured store %T: %w", store, beads.ErrConditionalReleaseUnsupported)
	}
	return releaser.ReleaseIfCurrent(id, actor)
}

func (s hookClaimStoreOps) reclaimStale(ctx context.Context, dir string, env []string, id string) (bool, string, error) {
	store, err := s.open(ctx, dir, env, "")
	if err != nil {
		return false, "", err
	}
	defer closeHookClaimStore(store)
	reclaimer, ok := store.(beads.StaleAssignmentReclaimer)
	if !ok {
		return false, "", nil
	}
	reclaimed, owner, err := reclaimer.ReclaimStale(id)
	if errors.Is(err, beads.ErrReclaimUnsupported) {
		return false, "", nil
	}
	return reclaimed, owner, err
}

func (s hookClaimStoreOps) restampAdopted(ctx context.Context, dir string, env []string, id, from, to string) (bool, error) {
	store, err := s.open(ctx, dir, env, to)
	if err != nil {
		return false, err
	}
	defer closeHookClaimStore(store)
	transferStore, ok := store.(beads.ConditionalAssigneeTransferer)
	if !ok {
		return false, fmt.Errorf("transferring through configured store %T: %w", store, beads.ErrConditionalTransferUnsupported)
	}
	return transferStore.TransferIfCurrent(id, from, to)
}

// hookWorkflowStoreHandle owns only handles opened by workflowStore. A
// class-routed WorkflowStore returns a borrowed class handle instead, which
// closeHookWorkflowStore must leave open for the rest of the invocation.
type hookWorkflowStoreHandle struct {
	beads.Store
}

func closeHookWorkflowStore(store beads.Store) {
	if owned, ok := store.(*hookWorkflowStoreHandle); ok {
		closeHookClaimStore(owned.Store)
	}
}

func (s hookClaimStoreOps) workflowStore(ctx context.Context, dir string, env []string, _, actor string) beads.Store {
	store, err := s.open(ctx, dir, env, actor)
	if err != nil {
		return refusedClassStore{err: err}
	}
	return &hookWorkflowStoreHandle{Store: store}
}

func (s hookClaimStoreOps) emitExecutionStepStarted(step beads.Bead, dir string, env []string, actor string) {
	s.emitExecutionStepStartedTo(openCityRecorder(io.Discard), step, dir, env, actor)
}

func (s hookClaimStoreOps) emitExecutionStepStartedTo(rec events.Recorder, step beads.Bead, dir string, env []string, actor string) {
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // lifecycle events are best-effort
	}
	store, err := s.open(context.Background(), dir, env, actor)
	if err != nil {
		return
	}
	defer closeHookClaimStore(store)
	_ = executionevent.EmitLifecycle(rec, store, events.ExecutionStepStarted, step, eventActor())
}
