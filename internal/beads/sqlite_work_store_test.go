package beads

import (
	"sync"
	"testing"
	"time"
)

// This exercises the production SQLite constructor through two independent
// connections, so a process-local mutex cannot make the ownership test pass.
func TestSQLiteWorkOwnershipSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenSQLiteWorkStore(dir, WithSQLiteStoreIDPrefix("wrk"), WithSQLiteStoreRetention(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	second, err := OpenSQLiteWorkStore(dir, WithSQLiteStoreIDPrefix("wrk"), WithSQLiteStoreRetention(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	b, err := first.Create(Bead{Title: "claim across independent connections"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := make(chan string, 2)
	failures := make(chan error, 2)
	for actor, store := range map[string]*SQLiteWorkStore{"one": first, "two": second} {
		bound := WithClaimActor(store, actor).(AssignmentClaimer)
		wg.Add(1)
		go func(actor string, bound AssignmentClaimer) {
			defer wg.Done()
			_, won, err := bound.Claim(b.ID)
			if err != nil {
				failures <- err
			} else if won {
				wins <- actor
			}
		}(actor, bound)
	}
	wg.Wait()
	close(wins)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	owner := ""
	count := 0
	for actor := range wins {
		owner = actor
		count++
	}
	if count != 1 {
		t.Fatalf("claim winners = %d, want one", count)
	}
	held, err := second.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Assignee != owner || held.Status != "in_progress" || held.Metadata[WorkLeaseExpiresMetadataKey] == "" {
		t.Fatalf("claim not durable: %+v", held)
	}
	if _, err := second.Heartbeat(b.ID, "other"); err == nil {
		t.Fatal("wrong actor renewed claim")
	}
	if changed, _, err := second.ReclaimStale(b.ID); err != nil || changed {
		t.Fatalf("fresh claim reclaimed: %v %v", changed, err)
	}
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := first.Update(b.ID, UpdateOpts{Metadata: map[string]string{WorkLeaseExpiresMetadataKey: expired}}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Heartbeat(b.ID, owner); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := first.ReclaimStale(b.ID); err != nil || changed {
		t.Fatalf("renewed claim reclaimed: %v %v", changed, err)
	}
	if err := first.Update(b.ID, UpdateOpts{Metadata: map[string]string{WorkLeaseExpiresMetadataKey: expired}}); err != nil {
		t.Fatal(err)
	}
	if changed, previous, err := second.ReclaimStale(b.ID); err != nil || !changed || previous != owner {
		t.Fatalf("expired claim not reclaimed: %v %q %v", changed, previous, err)
	}
	_, won, err := first.Claim(b.ID, "next")
	if err != nil || !won {
		t.Fatalf("new claim: %v %v", won, err)
	}
	if moved, err := second.TransferIfCurrent(b.ID, owner, "intruder"); err != nil || moved {
		t.Fatalf("stale transfer changed claim: %v %v", moved, err)
	}
	if moved, err := second.TransferIfCurrent(b.ID, "next", "canonical"); err != nil || !moved {
		t.Fatalf("owner transfer: %v %v", moved, err)
	}
	if changed, err := first.ReleaseIfCurrent(b.ID, "next"); err != nil || changed {
		t.Fatalf("stale release changed claim: %v %v", changed, err)
	}
	if err := second.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteWorkStore(dir, WithSQLiteStoreIDPrefix("wrk"), WithSQLiteStoreRetention(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	done, err := reopened.Get(b.ID)
	if err != nil || done.Status != "closed" || done.Assignee != "canonical" {
		t.Fatalf("reopen lost final state: %+v %v", done, err)
	}
}
