package beads

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// WorkLeaseExpiresMetadataKey is the durable deadline used by SQLite work claims.
	WorkLeaseExpiresMetadataKey = "gc.work.lease_expires_at"
	// WorkLeaseDuration is renewed by a successful owner heartbeat.
	WorkLeaseDuration = 30 * time.Minute
)

// SQLiteWorkStore adds owner-checked work leases to the native SQLite store.
// Coordination bindings continue to use SQLiteStore without work lease policy.
type SQLiteWorkStore struct {
	*SQLiteStore
}

// OpenSQLiteWorkStore opens a scope-local work ledger without an external service.
func OpenSQLiteWorkStore(dir string, opts ...SQLiteStoreOption) (*SQLiteWorkStore, error) {
	store, err := OpenSQLiteStore(dir, opts...)
	if err != nil {
		return nil, err
	}
	return &SQLiteWorkStore{SQLiteStore: store.(*SQLiteStore)}, nil
}

// Claim commits the assignment and its first lease in the same transaction.
func (s *SQLiteWorkStore) Claim(id, assignee string) (Bead, bool, error) {
	return s.claim(id, assignee, true)
}

// Heartbeat renews a work lease only while the caller still owns the live claim.
func (s *SQLiteWorkStore) Heartbeat(id, actor string) (Bead, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return Bead{}, fmt.Errorf("heartbeat %q: empty actor", id)
	}
	err := s.workLeaseWrite(id, func(b *Bead) (bool, error) {
		if b.Status != "in_progress" || b.Assignee != actor {
			return false, fmt.Errorf("heartbeat %q: %q does not own the active claim", id, actor)
		}
		setWorkLease(b, time.Now())
		return true, nil
	})
	if err != nil {
		return Bead{}, err
	}
	return s.Get(id)
}

// ReclaimStale releases exactly one expired work lease. Missing lease evidence
// never authorizes a release, including assignments imported from another store.
func (s *SQLiteWorkStore) ReclaimStale(id string) (bool, string, error) {
	var reclaimed bool
	var previous string
	err := s.workLeaseWrite(id, func(b *Bead) (bool, error) {
		reclaimed, previous = false, ""
		if b.Status != "in_progress" || b.Assignee == "" {
			return false, nil
		}
		deadline := b.Metadata[WorkLeaseExpiresMetadataKey]
		if deadline == "" {
			return false, nil
		}
		expires, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return false, fmt.Errorf("reclaim %q: invalid lease deadline: %w", id, err)
		}
		if time.Now().Before(expires) {
			return false, nil
		}
		previous = b.Assignee
		b.Assignee = ""
		setBeadStatus(b, "open")
		delete(b.Metadata, WorkLeaseExpiresMetadataKey)
		reclaimed = true
		return true, nil
	})
	return reclaimed, previous, err
}

// TransferIfCurrent changes only an in-progress claim held by the expected owner.
func (s *SQLiteWorkStore) TransferIfCurrent(id, from, to string) (bool, error) {
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return false, fmt.Errorf("claim transfer requires both owners")
	}
	changed := false
	err := s.workLeaseWrite(id, func(b *Bead) (bool, error) {
		changed = false
		if b.Status != "in_progress" || b.Assignee != from {
			return false, nil
		}
		b.Assignee = to
		setWorkLease(b, time.Now())
		changed = true
		return true, nil
	})
	return changed, err
}

func setWorkLease(b *Bead, now time.Time) {
	if b.Metadata == nil {
		b.Metadata = make(StringMap)
	}
	b.Metadata[WorkLeaseExpiresMetadataKey] = now.Add(WorkLeaseDuration).UTC().Format(time.RFC3339Nano)
}

func (s *SQLiteWorkStore) workLeaseWrite(id string, change func(*Bead) (bool, error)) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	return retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite work lease: begin: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		before, err := s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		next := cloneBead(before)
		changed, err := change(&next)
		if err != nil {
			return err
		}
		if changed {
			next.UpdatedAt = time.Now()
			if err := s.upsertBeadTx(ctx, tx, next); err != nil {
				return err
			}
			if err := s.bumpClaimFenceIfOwnershipTransitionTx(ctx, tx, before, &next); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
