package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
)

// holdingACPAgent answers the ACP handshake and never answers a
// session/prompt, so the first prompt leaves the session busy.
const holdingACPAgent = `exec python3 -u -c '
import sys, json
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method", "")
    if method == "initialize":
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"serverInfo": {"name": "holding"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "held-1"}}), flush=True)
'`

// TestPollerSessionIdleEnoughUsesACPIdleWait pins the ACP behavior of the
// queued-nudge quiescence check when no activity stamp is observed: the ACP
// provider now answers WaitForIdle, so an idle session it owns accepts
// delivery, a session with an outstanding prompt does not, and a provider
// that does not own the connection reports not-idle.
func TestPollerSessionIdleEnoughUsesACPIdleWait(t *testing.T) {
	dir := filepath.Join(testutil.ShortTempDir(t, "gc-acp-"), "acp")
	sp := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{HandshakeTimeout: 10 * time.Second})
	name := "gc-acp-poller-idle"
	if err := sp.Start(context.Background(), name, runtime.Config{Command: holdingACPAgent, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(name) })
	target := nudgeTarget{sessionName: name}
	noActivity := worker.LiveObservation{}

	if !pollerSessionIdleEnough(target, sp, time.Second, noActivity) {
		t.Fatal("pollerSessionIdleEnough = false, want true for an idle ACP session")
	}

	other := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{})
	if pollerSessionIdleEnough(target, other, time.Second, noActivity) {
		t.Fatal("pollerSessionIdleEnough = true through a provider that does not own the ACP connection")
	}

	if err := sp.Nudge(name, runtime.TextContent("hold")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if pollerSessionIdleEnough(target, sp, 20*time.Millisecond, noActivity) {
		t.Fatal("pollerSessionIdleEnough = true while an ACP prompt is outstanding")
	}
}

// TestACPNoWakeDrainRequiresIdleTurn exercises the drain transaction through
// the real ACP control socket, without promoting ACP's sleep capability.
func TestACPNoWakeDrainRequiresIdleTurn(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, signal := range []bool{false, true} {
			dir, err := os.MkdirTemp("/tmp", "gc-acp-drain-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			owner := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{HandshakeTimeout: 10 * time.Second})
			reader := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{})
			var meta []string
			wantKind := intentDrainBeginFresh
			if signal {
				meta = []string{session.DrainIntentReasonKey, reasonNoWake, session.DrainIntentAtKey, time.Now().Add(-time.Minute).Format(time.RFC3339), session.DrainIntentIncarnationKey, "3"}
				wantKind = intentSignalFresh
			}
			p, it, store, _ := admittedBeginOn(t, meta, func(e *selectionEntry) { e.Desired = desireSleep }, func(w *World) {
				w.Env.SP = reader
				for id, row := range w.Census.Rows {
					name := row.Info.SessionName
					if err := owner.Start(context.Background(), name, runtime.Config{
						Command: holdingACPAgent, WorkDir: t.TempDir(),
						Env: map[string]string{"GC_SESSION_ID": id.ID, "GC_INSTANCE_TOKEN": row.Info.InstanceToken},
					}); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = owner.Stop(name) })
					w.SleepPolicies[id.ID] = resolveSessionSleepPolicyInfo(row.Info, w.Env.Cfg, reader)
					w.Now = time.Now()
					w.Obs = observedOwn(w, id.ID)
				}
			})
			if it.Kind != wantKind {
				t.Fatalf("busy=%v signal=%v: decision %s (%s), want %s", busy, signal, it.Kind, it.Reason, wantKind)
			}
			if busy {
				name := p.World.Census.Rows[it.Key].Info.SessionName
				if err := owner.Nudge(name, runtime.TextContent("hold")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Get(it.Key.ID)
			if err != nil {
				t.Fatal(err)
			}
			s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
			after, err := store.Get(it.Key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if busy {
				if s.Outcome != settledNoop || s.Cause != causeNotIdle || after.Revision != before.Revision {
					t.Fatalf("busy signal=%v: %+v, revision %v -> %v", signal, s, before.Revision, after.Revision)
				}
			} else if s.Outcome != settledLanded {
				t.Fatalf("idle signal=%v: %+v", signal, s)
			}
			assertNoIdleProbeTarget(t, censusRowOf(t, after).Info, p.World.SleepPolicies[it.Key.ID])
		}
	}
}
