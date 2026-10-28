package main

import (
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Reclaim provenance stamped on a source bead whose dead owner was released.
const (
	sourceReclaimedFromMetadataKey = "gc.reclaimed_from"
	sourceReclaimedAtMetadataKey   = "gc.reclaimed_at"
	sourceReclaimedByMetadataKey   = "gc.reclaimed_with_step"
	// workLeaseExpiresMetadataKey is the SQLite work provider's claim lease deadline.
	workLeaseExpiresMetadataKey = "gc.work.lease_expires_at"
)

// releaseDeadOwnerSourceClaims releases the source work a reclaimed workflow
// step's dead assignee still holds. The step is released by the caller only
// after its assignee was confirmed dead, so that same identity holding the
// workflow's input-convoy source is equally dead. Without this, the next
// worker to claim the step finds the source owned by the dead session and
// must refuse it.
//
// Only members still assigned to deadAssignee are touched. Status, labels
// (including dispatch holds) and every other field stay as they are, so a
// held source stays held but ownerless. Each write is revision-fenced where
// the store supports it, so a worker that claimed the source in between
// keeps it.
func releaseDeadOwnerSourceClaims(cityStore beads.Store, rigStores map[string]beads.Store, stepStore beads.Store, step beads.Bead, now time.Time) []string {
	deadAssignee := strings.TrimSpace(step.Assignee)
	rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	if deadAssignee == "" || stepStore == nil {
		return nil
	}
	root := step
	if strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey]) == "" {
		if rootID == "" || rootID == step.ID {
			return nil
		}
		var err error
		if root, err = stepStore.Get(rootID); err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				log.Printf("releaseDeadOwnerSourceClaims: loading root %s of %s: %v", rootID, step.ID, err)
			}
			return nil
		}
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return nil
	}
	sourceStore := sourceStoreForRoot(cityStore, rigStores, stepStore, root)
	if sourceStore == nil {
		return nil
	}
	members, err := convoy.Members(sourceStore, convoyID, false, sourceStore)
	if err != nil {
		log.Printf("releaseDeadOwnerSourceClaims: listing input convoy %s of %s: %v", convoyID, step.ID, err)
		return nil
	}
	var released []string
	for _, m := range members {
		if convoy.IsUnresolvedTrackedItem(m) || convoy.IsTerminalStatus(m.Status) {
			continue
		}
		if strings.TrimSpace(m.Assignee) != deadAssignee {
			continue
		}
		ok, err := releaseDeadOwnerSource(sourceStore, m, deadAssignee, step.ID, now)
		if err != nil {
			log.Printf("releaseDeadOwnerSourceClaims: releasing source %s of dead %q: %v", m.ID, deadAssignee, err)
			continue
		}
		if ok {
			released = append(released, m.ID)
		}
	}
	return released
}

// stepTracksSource reports whether sourceID is a member of the input convoy
// of step's workflow. A bead outside any workflow tracks nothing.
func stepTracksSource(cityStore beads.Store, rigStores map[string]beads.Store, stepStore beads.Store, step beads.Bead, sourceID string) (bool, error) {
	root := step
	if strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey]) == "" {
		rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" || rootID == step.ID {
			return false, nil
		}
		var err error
		if root, err = stepStore.Get(rootID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return false, nil
	}
	if convoyID == sourceID {
		return true, nil
	}
	sourceStore := sourceStoreForRoot(cityStore, rigStores, stepStore, root)
	if sourceStore == nil {
		return false, nil
	}
	members, err := convoy.Members(sourceStore, convoyID, false, sourceStore)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(members, func(m beads.Bead) bool { return m.ID == sourceID }), nil
}

// sourceStoreForRoot is the store holding the workflow's input convoy and
// source work: the root's gc.root_store_ref scope, else the step's store.
func sourceStoreForRoot(cityStore beads.Store, rigStores map[string]beads.Store, stepStore beads.Store, root beads.Bead) beads.Store {
	ref := strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey])
	rig, ok := storeref.ScopeRigContext(ref)
	switch {
	case !ok:
		return stepStore
	case rig == "" && !storeref.IsClassRef(ref):
		return cityStore
	case rig == "":
		return stepStore
	default:
		return rigStores[rig]
	}
}

// releaseDeadOwnerSource clears the assignee and work lease of src while it
// is still held by deadAssignee. It is idempotent: a source already released
// or claimed by someone else is left alone.
func releaseDeadOwnerSource(store beads.Store, src beads.Bead, deadAssignee, stepID string, now time.Time) (bool, error) {
	update := beads.UpdateOpts{
		Assignee: stringPtr(""),
		Metadata: map[string]string{
			workLeaseExpiresMetadataKey:    "",
			sourceReclaimedFromMetadataKey: deadAssignee,
			sourceReclaimedAtMetadataKey:   now.UTC().Format(time.RFC3339),
			sourceReclaimedByMetadataKey:   stepID,
		},
	}
	current, err := store.Get(src.ID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if strings.TrimSpace(current.Assignee) != deadAssignee || convoy.IsTerminalStatus(current.Status) {
		return false, nil
	}
	if writer, ok := beads.ConditionalWriterForTarget(store); ok && current.Revision != 0 {
		if err := writer.UpdateIfMatch(current.ID, current.Revision, update); err != nil {
			var stale *beads.PreconditionFailedError
			if errors.As(err, &stale) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err := store.Update(current.ID, update); err != nil {
		return false, err
	}
	return true, nil
}
