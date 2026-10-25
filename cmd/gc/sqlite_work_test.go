package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func sqliteWorkTestCity(t *testing.T) (string, string, *config.City) {
	t.Helper()
	clearGCEnv(t)
	resetCLIStorageRoutes(t)
	city := t.TempDir()
	rig := filepath.Join(city, "rig")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`[workspace]
name = "sqlite-work-test"
prefix = "hq"
[beads]
provider = "sqlite-work"
[[rigs]]
name = "rig"
path = %q
prefix = "wrk"
`, rig)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", city)
	t.Setenv("BEADS_ACTOR", "worker-one")
	t.Setenv("BD_BIN", filepath.Join(city, "no-bd-installed"))
	cfg, err := loadCityConfigForBdPassthrough(city, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return city, rig, cfg
}

func TestSQLiteWorkRigAddUnconfiguredScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefix    string
		marker    string
		contents  string
		wantError string
	}{
		{name: "derived prefix"},
		{name: "explicit prefix", prefix: "nwr"},
		{name: "route rollback", wantError: "writing routes"},
		{name: "prefix collision", prefix: "wrk", wantError: "collides"},
		{name: "legacy bd", marker: ".beads/metadata.json", contents: "{}", wantError: "beads store"},
		{name: "legacy file", marker: ".gc/beads.json", contents: "{}", wantError: "refusing implicit migration"},
		{name: "orphan ledger", marker: ".gc/store/work/beads.sqlite", wantError: "refusing implicit migration"},
		{name: "identity mismatch", marker: ".gc/store/work/identity.json", contents: `{"provider":"sqlite-work","prefix":"other"}`, wantError: "identity mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, _, _ := sqliteWorkTestCity(t)
			t.Setenv("HOME", t.TempDir())
			rig := filepath.Join(city, "newrig")
			if err := os.MkdirAll(rig, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.marker != "" {
				path := filepath.Join(rig, tc.marker)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "route rollback" {
				if err := os.MkdirAll(filepath.Join(city, "rig", ".beads", "routes.jsonl"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := openSQLiteWorkScope(rig, city, false); err == nil || !strings.Contains(err.Error(), "unconfigured scope") {
				t.Fatalf("ordinary read accepted unconfigured rig: %v", err)
			}
			if err := initSQLiteWorkScope(rig, city); err == nil || !strings.Contains(err.Error(), "unconfigured scope") {
				t.Fatalf("ordinary initialization accepted unconfigured rig: %v", err)
			}
			var out, stderr bytes.Buffer
			code := doRigAdd(fsys.OSFS{}, city, rig, nil, "", tc.prefix, "", false, false, &out, &stderr)
			if tc.wantError != "" {
				if code != 1 || !strings.Contains(stderr.String(), tc.wantError) {
					t.Fatalf("rig add = %d, want %q: %s", code, tc.wantError, stderr.String())
				}
				after, err := os.ReadFile(filepath.Join(city, "city.toml"))
				if err != nil || !bytes.Equal(original, after) {
					t.Fatalf("failed provisioning changed city config: %v", err)
				}
				if _, err := openSQLiteWorkScope(rig, city, false); err == nil || !strings.Contains(err.Error(), "unconfigured scope") {
					t.Fatalf("failed provisioning authorized the scope: %v", err)
				}
				return
			}
			if code != 0 {
				t.Fatalf("rig add failed before registration: %s", stderr.String())
			}
			store, err := openSQLiteWorkScope(rig, city, false)
			if err != nil {
				t.Fatal(err)
			}
			defer store.CloseStore() //nolint:errcheck
			ticket, err := store.Create(beads.Bead{Title: "new rig work"})
			prefix := tc.prefix
			if prefix == "" {
				prefix = config.DeriveBeadsPrefix("newrig")
			}
			if err != nil || !strings.HasPrefix(ticket.ID, prefix+"-") {
				t.Fatalf("new rig cannot mint its own work: %+v, %v", ticket, err)
			}
			if _, err := os.Stat(filepath.Join(rig, ".beads", "metadata.json")); !os.IsNotExist(err) {
				t.Fatalf("native provisioning created legacy store: %v", err)
			}
		})
	}
}

func TestSQLiteWorkHookClaimsBeyondWorkflowLatches(t *testing.T) {
	for _, routeKey := range []string{"gc.routed_to", "gc.run_target"} {
		t.Run(routeKey, func(t *testing.T) {
			city, rig, _ := sqliteWorkTestCity(t)
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(city, "city.toml")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = append(body, []byte("\n[[agent]]\nname = \"worker\"\n")...)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{city, rig} {
				if err := initSQLiteWorkScope(scope, city); err != nil {
					t.Fatal(err)
				}
			}
			store, err := openSQLiteWorkScope(city, city, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.CloseStore(); err != nil {
					t.Error(err)
				}
			})
			for i := 0; i < 20; i++ {
				priority := 1
				_, err := store.Create(beads.Bead{
					Title: fmt.Sprintf("expanded workflow %d", i), Priority: &priority,
					Metadata: map[string]string{routeKey: "worker", "gc.kind": "workflow", "gc.workflow_expanded": "true"},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			metadata := map[string]string{routeKey: "worker"}
			if routeKey == "gc.run_target" {
				metadata["gc.kind"] = "workflow"
			}
			next, err := store.Create(beads.Bead{Title: "next executable work", Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			previous, err := store.Create(beads.Bead{Title: "previous step", Assignee: "worker", Status: "in_progress"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(previous.ID); err != nil {
				t.Fatal(err)
			}
			gc := reexecGCTestBinaryForTests(t)
			t.Setenv("PATH", filepath.Dir(gc)+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_AGENT", "worker")
			t.Setenv("GC_BEADS", "sqlite-work")
			t.Setenv("GC_ALIAS", "worker")
			t.Setenv("GC_SESSION_ORIGIN", "ephemeral")
			t.Setenv("GC_NO_API", "1")
			cmd := exec.Command(gc, "hook", "--claim", "--json")
			cmd.Dir = city
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("claim after closing previous step: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode hook result: %v: %s", err, stdout.String())
			}
			if result.BeadID != next.ID {
				t.Fatalf("claimed %q, want executable work %q: %+v", result.BeadID, next.ID, result)
			}
			claimed, err := store.Get(next.ID)
			if err != nil || claimed.Status != "in_progress" || claimed.Assignee != "worker" {
				t.Fatalf("claim was not persisted: %+v, %v", claimed, err)
			}
		})
	}
}

func TestSQLiteWorkCLIAndHookRoundTrip(t *testing.T) {
	city, rig, cfg := sqliteWorkTestCity(t)
	run := func(args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		code := doBd(append([]string{"--rig", "rig"}, args...), &out, &stderr)
		if code != 0 {
			t.Fatalf("gc bd %v: exit %d: %s", args, code, stderr.String())
		}
		return out.String()
	}
	run("init")
	if err := initSQLiteWorkScope(city, city); err != nil {
		t.Fatal(err)
	}
	var created sqliteWorkView
	if err := json.Unmarshal([]byte(run("create", "work ticket", "--notes", "first note", "--external-ref", "https://example.test/ticket/1", "--json")), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.ID, "wrk-") {
		t.Fatalf("wrong scope prefix: %s", created.ID)
	}
	run("update", created.ID, "--append-notes", "second note")
	run("comments", "add", created.ID, "review evidence")
	ops := newHookCity(city, cfg).claimOps()
	env := []string{"BEADS_DIR=" + filepath.Join(rig, ".beads")}
	got, won, err := ops.Claim(context.Background(), rig, env, created.ID, "worker-one")
	if err != nil || !won || got.Assignee != "worker-one" {
		t.Fatalf("native hook claim: %+v %v %v", got, won, err)
	}
	run("heartbeat", created.ID)
	var rootOut, rootErr bytes.Buffer
	if code := doBd([]string{"update", created.ID, "--if-assignee", "another-worker", "--if-status", "in_progress", "--status", "open"}, &rootOut, &rootErr); code != 0 {
		t.Fatalf("root native ID routing: %s", rootErr.String())
	}
	var guarded []sqliteWorkView
	if err := json.Unmarshal([]byte(run("show", created.ID, "--json")), &guarded); err != nil {
		t.Fatal(err)
	}
	if len(guarded) != 1 || guarded[0].Status != "in_progress" || guarded[0].Assignee != "worker-one" {
		t.Fatalf("stale recovery changed current claim: %+v", guarded)
	}
	if err := ops.StampWorkMeta(context.Background(), rig, env, created.ID, "worker-one", map[string]string{"evidence": "verified"}); err != nil {
		t.Fatal(err)
	}
	run("close", created.ID, "--reason", "verified delivery")
	var rows []sqliteWorkView
	if err := json.Unmarshal([]byte(run("show", created.ID, "--json")), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "closed" || rows[0].Notes != "first note\nsecond note" || len(rows[0].Comments) != 1 || rows[0].Comments[0].Text != "review evidence" || rows[0].Metadata["evidence"] != "verified" {
		t.Fatalf("round trip lost fields: %+v", rows)
	}
	root, err := openSQLiteWorkScope(city, city, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	if _, err := root.Get(created.ID); err == nil {
		t.Fatal("rig work leaked into city work store")
	}
	for _, args := range [][]string{{"update", created.ID, "--unsupported"}, {"sql", "delete from beads"}, {"update", created.ID, "--claim", "--title", "changed"}} {
		var out, stderr bytes.Buffer
		if code := doBd(append([]string{"--rig", "rig"}, args...), &out, &stderr); code == 0 {
			t.Fatalf("unsupported mutation accepted: %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(rig, ".beads", "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("native work created bd metadata: %v", err)
	}
}

func TestSQLiteWorkControllerRigLifecycle(t *testing.T) {
	city, rig, cfg := sqliteWorkTestCity(t)
	if err := startBeadsLifecycleResolvingCityDatabase(city, cfg, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	work, err := openSQLiteWorkScope(rig, city, false)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := work.Create(beads.Bead{Title: "controller native work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.CloseStore(); err != nil {
		t.Fatal(err)
	}
	cs := &controllerState{cityPath: city, cfg: cfg, cacheCtx: context.Background()}
	stores := cs.buildStores(cfg)
	store := stores["rig"]
	if store == nil {
		t.Fatal("controller omitted the native rig store")
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck
	if got, err := store.Get(ticket.ID); err != nil || got.Title != ticket.Title {
		t.Fatalf("controller cannot read native work: %+v, %v", got, err)
	}
	claimer, ok := store.(interface {
		Claim(string, string) (beads.Bead, bool, error)
	})
	if !ok {
		t.Fatal("controller store does not expose atomic claims")
	}
	claimed, won, err := claimer.Claim(ticket.ID, "controller-worker")
	if err != nil || !won || claimed.Assignee != "controller-worker" {
		t.Fatalf("controller cannot claim native work: %+v, %t, %v", claimed, won, err)
	}
	if err := closeBeadStoreHandle(store); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sqliteWorkDir(rig), "beads.sqlite")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	unavailable := cs.openRigStore(sqliteWorkProvider, "rig", rig, "wrk", cfg)
	if _, err := unavailable.List(beads.ListQuery{}); err == nil || !strings.Contains(err.Error(), "sqlite-work") {
		t.Fatalf("missing native ledger did not fail closed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("controller recreated missing native ledger: %v", err)
	}
}

func TestSQLiteWorkKeepsInfrastructureBindingAcrossRestart(t *testing.T) {
	city, rig, cfg := sqliteWorkTestCity(t)
	if err := startBeadsLifecycleResolvingCityDatabase(city, cfg, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, "city.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(`
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"
[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store/graph"
`)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadCityConfigForBdPassthrough(city, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var out, log bytes.Buffer
	request := storageOperatorRequest{CityPath: city, Cfg: cfg, FleetStopped: true}
	if code := doStorageMigrate(context.Background(), request, &out, &log); code != 0 {
		t.Fatalf("native infrastructure genesis: %s", log.String())
	}
	routes, err := storageBootGate(city, cfg, "test native restart", nil, &log)
	if err != nil {
		t.Fatalf("boot split native city: %v: %s", err, log.String())
	}
	work, err := openSQLiteWorkScope(rig, city, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := work.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	ticket, err := work.Create(beads.Bead{Title: "native work remains separate"})
	if err != nil {
		t.Fatal(err)
	}
	infra := resolveSessionStore(routes, work, cfg, city, nil)
	session, err := infra.Create(beads.Bead{Title: "durable session", Type: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := work.Get(session.ID); err == nil {
		t.Fatal("session written into native work store")
	}
	t.Setenv("BEADS_ACTOR", "old-worker")
	if code := doBd([]string{"update", session.ID, "--claim"}, &out, &log); code != 0 {
		t.Fatalf("claim infrastructure session: %s", log.String())
	}
	for _, owner := range []string{"new-worker", "old-worker"} {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"update", session.ID, "--if-assignee", owner, "--if-status", "in_progress", "--assignee", "", "--status", "open"}, &stdout, &stderr); code != 0 {
			t.Fatalf("guarded infrastructure update: %s", stderr.String())
		}
		row, err := infra.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if owner == "new-worker" && row.Assignee != "old-worker" || owner == "old-worker" && (row.Assignee != "" || row.Status != "open") {
			t.Fatalf("guarded infrastructure update by %q: %+v", owner, row)
		}
	}
	if err := routes.close(); err != nil {
		t.Fatal(err)
	}
	if err := startBeadsLifecycleResolvingCityDatabase(city, cfg, &log, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := storageBootGate(city, cfg, "test native restart", nil, &log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.close(); err != nil {
			t.Error(err)
		}
	})
	infra = resolveSessionStore(reopened, work, cfg, city, nil)
	if _, err := infra.Get(session.ID); err != nil {
		t.Fatalf("restart lost infrastructure session: %v", err)
	}
	if _, err := work.Get(ticket.ID); err != nil {
		t.Fatalf("restart lost work: %v", err)
	}
}

func TestSQLiteWorkDependenciesAndIdentityRefusals(t *testing.T) {
	city, rig, _ := sqliteWorkTestCity(t)
	if _, err := openSQLiteWorkScope(rig, city, false); err == nil {
		t.Fatal("ordinary read created missing ledger")
	}
	if err := initSQLiteWorkScope(rig, city); err != nil {
		t.Fatal(err)
	}
	store, err := openSQLiteWorkScope(rig, city, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	before, err := store.Create(beads.Bead{Title: "prerequisite"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Create(beads.Bead{Title: "dependent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(after.ID, before.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	ready, err := store.Ready()
	if err != nil || len(ready) != 1 || ready[0].ID != before.ID {
		t.Fatalf("blocked readiness: %+v %v", ready, err)
	}
	if err := store.Close(before.ID); err != nil {
		t.Fatal(err)
	}
	ready, err = store.Ready()
	if err != nil || len(ready) != 1 || ready[0].ID != after.ID {
		t.Fatalf("released readiness: %+v %v", ready, err)
	}
	identity := filepath.Join(sqliteWorkDir(rig), "identity.json")
	if err := os.WriteFile(identity, []byte(`{"provider":"sqlite-work","prefix":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSQLiteWorkScope(rig, city, false); err == nil {
		t.Fatal("mismatched scope identity accepted")
	}
}

// Control dispatch must read native work stores in-process. A BdStore here shells
// the disabled external bd and the dispatcher exits at startup (exit 78).
func TestSQLiteWorkControlStoreOpensNatively(t *testing.T) {
	city, rig, cfg := sqliteWorkTestCity(t)
	for _, scope := range []string{city, rig} {
		if err := initSQLiteWorkScope(scope, city); err != nil {
			t.Fatal(err)
		}
		store, err := openControlStoreAtForCity(scope, city, cfg)
		if err != nil {
			t.Fatalf("open control store for %s: %v", scope, err)
		}
		native, ok := store.(*beads.SQLiteWorkStore)
		if !ok {
			t.Fatalf("control store for %s is %T, want native sqlite-work", scope, store)
		}
		created, err := native.Create(beads.Bead{Title: "control ready"})
		if err != nil {
			t.Fatal(err)
		}
		ready, err := native.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil || len(ready) != 1 || ready[0].ID != created.ID {
			t.Fatalf("native ready for %s: %+v %v", scope, ready, err)
		}
		if err := native.CloseStore(); err != nil {
			t.Fatal(err)
		}
	}
}
