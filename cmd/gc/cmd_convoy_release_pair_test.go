package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// releasePairCity is a born-split city: sqlite-work work ledger plus a
// sqlite-beads binding for every infrastructure class, with a routed pool, a
// one-step graph.v2 formula and a source/step pair owned by a retired session.
type releasePairCity struct {
	t         *testing.T
	dir       string
	gc        string
	env       []string
	source    string
	step      string
	root      string
	finalizer string
	convoy    string
	session   string
	owner     string
}

const releasePairDeadOwner = "rig/gastown.retired"

func newReleasePairCity(t *testing.T, sourceLabels ...string) *releasePairCity {
	t.Helper()
	clearGCEnv(t)
	resetCLIStorageRoutes(t)
	dir := t.TempDir()
	for _, sub := range []string{"home", "formulas", ".gc"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	city := `[workspace]
name = "release-pair"
prefix = "qa"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"

[beads]
provider = "sqlite-work"

[[agent]]
name = "worker"

[[agent]]
name = "control-dispatcher"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(city), 0o600); err != nil {
		t.Fatal(err)
	}
	formula := "formula = \"pair\"\n[requires]\nformula_compiler = \">=2.0.0\"\n[[steps]]\nid = \"deliver\"\ntitle = \"deliver\"\n"
	if err := os.WriteFile(filepath.Join(dir, "formulas", "pair.toml"), []byte(formula), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &releasePairCity{t: t, dir: dir, gc: reexecGCTestBinaryForTests(t), owner: releasePairDeadOwner}
	c.env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"), "GC_CITY="+dir, "GC_HOME="+filepath.Join(dir, "supervisor"),
		"GC_BEADS=sqlite-work", "GC_NO_API=1", "GC_ISOLATED=1", "GC_AGENT=worker", "GC_ALIAS=worker",
		"BEADS_ACTOR=worker", "GC_SESSION_ORIGIN=ephemeral", "BD_BIN="+filepath.Join(dir, "no-bd"),
	)
	c.run("bd", "init")
	t.Setenv("GC_CITY", dir)
	t.Setenv("GC_BEADS", "sqlite-work")
	t.Setenv("HOME", filepath.Join(dir, "home"))
	sessions := c.sessionStore()
	session, err := sessions.Create(beads.Bead{Title: "retired session", Type: "session", Labels: []string{"gc:session"},
		Metadata: map[string]string{"alias": releasePairDeadOwner, "session_name": "pool-retired"}})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	c.session = session.ID
	if err := sessions.Close(c.session); err != nil {
		t.Fatalf("closing session bead: %v", err)
	}
	c.source = c.create("source work")
	c.convoy = c.jsonMap("convoy", "create", "pair input", c.source, "--json")["convoy_id"].(string)
	mapping := c.jsonMap("formula", "cook", "pair", "--json")["id_mapping"].(map[string]any)
	c.step = mapping["pair.deliver"].(string)
	c.root = mapping["pair"].(string)
	c.finalizer = mapping["pair.workflow-finalize"].(string)
	c.run("bd", "update", c.root, "--set-metadata", "gc.input_convoy_id="+c.convoy, "--set-metadata", "gc.routed_to=worker",
		"--set-metadata", "gc.source_bead_id="+c.source, "--set-metadata", "gc.session_name=pool-retired")
	srcArgs := []string{"bd", "update", c.source, "--assignee", releasePairDeadOwner, "--status", "blocked", "--set-metadata", "gc.execution_routed_to=worker"}
	for _, label := range sourceLabels {
		srcArgs = append(srcArgs, "--add-label", label)
	}
	c.run(srcArgs...)
	c.run("bd", "update", c.step, "--assignee", releasePairDeadOwner, "--status", "blocked",
		"--set-metadata", "gc.session_name=pool-retired", "--set-metadata", "gc.routed_to=worker")
	return c
}

// sessionStore is the sessions-class binding, which bd create refuses to mint into.
func (c *releasePairCity) sessionStore() beads.Store {
	c.t.Helper()
	store, relocated := cliStorageRoutes(c.dir).storeFor(coordclass.ClassSessions)
	if !relocated || store == nil {
		c.t.Fatal("sessions class is not relocated; the fixture is not a split city")
	}
	return store
}

func (c *releasePairCity) exec(args ...string) (string, string, int) {
	c.t.Helper()
	cmd := exec.Command(c.gc, args...)
	cmd.Dir, cmd.Env = c.dir, c.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		c.t.Fatalf("gc %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

func (c *releasePairCity) run(args ...string) string {
	c.t.Helper()
	out, errOut, code := c.exec(args...)
	if code != 0 {
		c.t.Fatalf("gc %v: exit %d\nstdout=%s\nstderr=%s", args, code, out, errOut)
	}
	return out
}

func (c *releasePairCity) jsonMap(args ...string) map[string]any {
	c.t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(c.run(args...)), &out); err != nil {
		c.t.Fatalf("gc %v: decode: %v", args, err)
	}
	return out
}

func (c *releasePairCity) create(title string, args ...string) string {
	c.t.Helper()
	return c.jsonMap(append([]string{"bd", "create", title, "--json"}, args...)...)["id"].(string)
}

type releasePairRow struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Assignee string            `json:"assignee"`
	Labels   []string          `json:"labels"`
	Metadata map[string]string `json:"metadata"`
}

func (c *releasePairCity) show(id string) releasePairRow {
	c.t.Helper()
	var rows []releasePairRow
	if err := json.Unmarshal([]byte(c.run("bd", "show", id, "--json")), &rows); err != nil || len(rows) != 1 {
		c.t.Fatalf("bd show %s: %v %v", id, rows, err)
	}
	return rows[0]
}

func (c *releasePairCity) releasePair(extra ...string) (string, string, int) {
	c.t.Helper()
	args := append([]string{"convoy", "release-pair", c.source, "--step", c.step, "--from", c.owner,
		"--old-session", c.session, "--handover-id", "h1", "--manifest", filepath.Join(c.dir, "handover.json")}, extra...)
	return c.exec(args...)
}

func (c *releasePairCity) hookClaim() (string, int) {
	c.t.Helper()
	out, _, code := c.exec("hook", "--claim", "--json")
	return out, code
}

// Inventory includes unselected work scopes before and after fencing.
func TestReleasePairRefusesUnselectedStoreReferences(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("after_fencing_%t", late), func(t *testing.T) {
			c := newReleasePairCity(t)
			cfgPath := filepath.Join(c.dir, "city.toml")
			cfg, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(c.dir, "other")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg = append(cfg, []byte("\n[[rigs]]\nname = \"other\"\npath = \""+other+"\"\n")...)
			if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
				t.Fatal(err)
			}
			c.run("--rig", "other", "bd", "init")
			if late {
				if _, errOut, code := c.releasePair("--apply", "--stop-after", "fence-source"); code != 0 {
					t.Fatalf("fence: %s", errOut)
				}
			}
			ref := c.jsonMap("--rig", "other", "bd", "create", "other owned reference", "--assignee", releasePairDeadOwner, "--json")["id"].(string)
			before := c.show(c.source)
			extra := []string{}
			want := ref
			if late {
				extra = append(extra, "--apply")
				want = "references or waits changed"
			}
			out, errOut, code := c.releasePair(extra...)
			if code != 1 || !strings.Contains(errOut, want) {
				t.Fatalf("expected refusal for %s, got exit %d: %s %s", ref, code, out, errOut)
			}
			if after := c.show(c.source); after.Assignee != before.Assignee || after.Status != before.Status {
				t.Fatalf("source changed after incomplete inventory: %+v -> %+v", before, after)
			}
		})
	}
}

func TestReleasePairRefusesLiveReplacementIdentity(t *testing.T) {
	for _, key := range []string{"alias", "agent_name", "alias_history"} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/late=%t", key, late), func(t *testing.T) {
				c := newReleasePairCity(t)
				if late {
					if _, errOut, code := c.releasePair("--apply", "--stop-after", "fence-source"); code != 0 {
						t.Fatalf("fence: %s", errOut)
					}
				}
				replacement, err := c.sessionStore().Create(beads.Bead{
					Title: "replacement owner", Type: "session", Labels: []string{"gc:session"},
					Metadata: map[string]string{key: releasePairDeadOwner, "session_name": "replacement-owner", "state": "active"},
				})
				if err != nil {
					t.Fatal(err)
				}
				before := c.show(c.source)
				args := []string{}
				if late {
					args = append(args, "--apply")
				}
				out, errOut, code := c.releasePair(args...)
				if code != 1 || !strings.Contains(errOut, replacement.ID) {
					t.Fatalf("expected refusal for replacement %s, got exit %d: %s %s", replacement.ID, code, out, errOut)
				}
				if after := c.show(c.source); after.Assignee != before.Assignee || after.Status != before.Status {
					t.Fatalf("source changed after replacement refusal: %+v -> %+v", before, after)
				}
			})
		}
	}
}

// The staged source-only release cannot be claimed until the step is released.
func TestReleasePairStagedHandoverOnSplitStores(t *testing.T) {
	for _, sessionID := range []bool{false, true} {
		t.Run(fmt.Sprintf("session_id=%t", sessionID), func(t *testing.T) {
			testReleasePairStagedHandover(t, sessionID)
		})
	}
}

func testReleasePairStagedHandover(t *testing.T, sessionID bool) {
	t.Helper()
	c := newReleasePairCity(t, "delivery:demo")
	if sessionID {
		c.owner = c.session
		c.run("bd", "update", c.source, "--assignee", c.owner)
		c.run("bd", "update", c.step, "--assignee", c.owner)
	}

	out, errOut, code := c.releasePair("--json")
	if code != 0 {
		t.Fatalf("JSON dry run: exit %d out=%s err=%s", code, out, errOut)
	}
	var result struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Action        string `json:"action"`
		Result        string `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Result != "preflight-verified" || result.SchemaVersion != "1" || !result.OK || result.Command != "convoy.release-pair" || result.Action != "release-pair" {
		t.Fatalf("JSON dry run result: %s (%v)", out, err)
	}
	if row := c.show(c.source); row.Assignee != c.owner || row.Status != "blocked" {
		t.Fatalf("dry run mutated the source: %+v", row)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "handover.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote a manifest: %v", err)
	}

	if out, errOut, code := c.releasePair("--apply", "--stop-after", "release-source"); code != 0 || !strings.Contains(out, "result=stopped") {
		t.Fatalf("staged run: exit %d out=%s err=%s", code, out, errOut)
	}
	src, step := c.show(c.source), c.show(c.step)
	if src.Assignee != "" || src.Status != "open" || slices.Contains(src.Labels, "hold:mayor") {
		t.Fatalf("source after release-source: %+v", src)
	}
	if step.Assignee != "" || step.Status != "open" || !slices.Contains(step.Labels, "hold:mayor") {
		t.Fatalf("step must stay fenced after release-source: %+v", step)
	}
	if out, code := c.hookClaim(); code != 1 || !strings.Contains(out, "no_work") {
		t.Fatalf("half-released pair was served: exit %d %s", code, out)
	}
	if row := c.show(c.root); row.Status == "closed" || row.Metadata["gc.session_name"] != "" {
		t.Fatalf("root after half release: %+v", row)
	}

	if out, errOut, code := c.releasePair("--apply"); code != 0 || !strings.Contains(out, "result=released") {
		t.Fatalf("resume: exit %d out=%s err=%s", code, out, errOut)
	}
	if row := c.show(c.source); !slices.Equal(row.Labels, []string{"delivery:demo"}) || row.Metadata["gc.handover.phase"] != "release-source" {
		t.Fatalf("source labels or provenance lost: %+v", row)
	}
	out, code = c.hookClaim()
	if code != 0 || !strings.Contains(out, c.step) {
		t.Fatalf("worker did not claim the original step: exit %d %s", code, out)
	}
	c.run("bd", "update", c.source, "--claim")
	if s, st := c.show(c.source), c.show(c.step); s.Assignee != "worker" || st.Assignee != "worker" {
		t.Fatalf("pair not owned by one successor: source=%+v step=%+v", s, st)
	}
	if row := c.show(c.root); row.Status == "closed" || row.Metadata["gc.input_convoy_id"] != c.convoy {
		t.Fatalf("root provenance lost: %+v", row)
	}

	before := c.show(c.step)
	if out, errOut, code := c.releasePair("--apply"); code != 0 || !strings.Contains(out, "result=released") {
		t.Fatalf("repeat: exit %d out=%s err=%s", code, out, errOut)
	}
	if after := c.show(c.step); after.Assignee != before.Assignee || after.Status != before.Status {
		t.Fatalf("repeat changed the claimed step: %+v -> %+v", before, after)
	}
}

// A hold the operator did not name survives the handover, and a named one is
// cleared only at release.
func TestReleasePairPreservesUnnamedHolds(t *testing.T) {
	c := newReleasePairCity(t, "hold:external")
	if out, errOut, code := c.releasePair("--apply"); code != 0 {
		t.Fatalf("release: exit %d out=%s err=%s", code, out, errOut)
	}
	if row := c.show(c.source); !slices.Contains(row.Labels, "hold:external") {
		t.Fatalf("unnamed hold cleared: %+v", row)
	}

	c = newReleasePairCity(t, "hold:external")
	if out, errOut, code := c.releasePair("--apply", "--clear-hold", "hold:external"); code != 0 {
		t.Fatalf("release with authority: exit %d out=%s err=%s", code, out, errOut)
	}
	if row := c.show(c.source); slices.Contains(row.Labels, "hold:external") {
		t.Fatalf("named hold kept: %+v", row)
	}
}

// Concurrent changes between phases refuse before the next write: an owner
// change on a fenced record, and a new reference such as an approval gate.
func TestReleasePairRefusesDrift(t *testing.T) {
	c := newReleasePairCity(t)
	if _, errOut, code := c.releasePair("--apply", "--stop-after", "fence-source"); code != 0 {
		t.Fatalf("fence: %s", errOut)
	}
	c.run("bd", "update", c.step, "--set-metadata", "gc.claimed_at=late")
	srcBefore := c.show(c.source)
	_, errOut, code := c.releasePair("--apply")
	if code != 1 || !strings.Contains(errOut, "changed since the last verified phase") {
		t.Fatalf("owner drift not refused: exit %d %s", code, errOut)
	}
	if row := c.show(c.source); row.Assignee != srcBefore.Assignee || row.Status != srcBefore.Status {
		t.Fatalf("source written after refusal: %+v", row)
	}

	c = newReleasePairCity(t)
	if _, errOut, code := c.releasePair("--apply", "--stop-after", "prepare-step"); code != 0 {
		t.Fatalf("prepare: %s", errOut)
	}
	c.create("new approval gate", "--set-metadata", "gc.approval.source_id="+c.source)
	_, errOut, code = c.releasePair("--apply")
	if code != 1 || !strings.Contains(errOut, "references or waits changed") {
		t.Fatalf("new gate not refused: exit %d %s", code, errOut)
	}
	if row := c.show(c.source); row.Assignee != releasePairDeadOwner {
		t.Fatalf("source released despite a new gate: %+v", row)
	}
}

// Preflight refuses a pair whose owner session is still open.
func TestReleasePairRefusesLiveOwner(t *testing.T) {
	c := newReleasePairCity(t)
	if err := c.sessionStore().Reopen(c.session); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := c.releasePair("--apply")
	if code != 1 || !strings.Contains(errOut, "is not closed") {
		t.Fatalf("live owner not refused: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "handover.json")); !os.IsNotExist(err) {
		t.Fatalf("refused preflight wrote a manifest: %v", err)
	}
}

// An interrupted write whose postimage matches neither side exits 2 and is not
// retried; one that landed is recognized on resume.
func TestReleasePairReconcilesInterruptedWrites(t *testing.T) {
	c := newReleasePairCity(t)
	manifest := filepath.Join(c.dir, "handover.json")
	if _, errOut, code := c.releasePair("--apply", "--stop-after", "fence-step"); code != 0 {
		t.Fatalf("fence-step: %s", errOut)
	}
	var m releasePairManifest
	data, err := os.ReadFile(manifest)
	if err != nil || json.Unmarshal(data, &m) != nil {
		t.Fatalf("manifest: %v", err)
	}
	planned := m.Expected[c.source].clone()
	planned.Labels = append(planned.Labels, "hold:mayor")
	planned.Metadata["gc.handover.id"], planned.Metadata["gc.handover.phase"] = "h1", "fence-source"
	m.Pending = &releasePairPending{Phase: "fence-source", ID: c.source, ExpectedRevision: m.Expected[c.source].Revision, Planned: planned}
	if err := saveReleasePairManifest(manifest, &m); err != nil {
		t.Fatal(err)
	}
	c.run("bd", "update", c.source, "--add-label", "hold:mayor", "--set-metadata", "gc.handover.id=h1", "--set-metadata", "gc.handover.phase=fence-source")
	if out, errOut, code := c.releasePair("--apply", "--stop-after", "fence-source"); code != 0 || !strings.Contains(out, "completed=fence-step,fence-source") {
		t.Fatalf("landed write not recognized: exit %d out=%s err=%s", code, out, errOut)
	}

	data, _ = os.ReadFile(manifest)
	m = releasePairManifest{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Pending = &releasePairPending{Phase: "prepare-step", ID: c.step, ExpectedRevision: m.Expected[c.step].Revision, Planned: m.Expected[c.step]}
	if err := saveReleasePairManifest(manifest, &m); err != nil {
		t.Fatal(err)
	}
	c.run("bd", "update", c.step, "--set-metadata", "foreign=write")
	_, errOut, code := c.releasePair("--apply")
	if code != releasePairExitUnknown || !strings.Contains(errOut, "outcome unknown") {
		t.Fatalf("unknown outcome: exit %d %s", code, errOut)
	}
	if row := c.show(c.step); row.Assignee != releasePairDeadOwner {
		t.Fatalf("step retried after unknown outcome: %+v", row)
	}
}

// One retired owner holding two pairs hands over each pair by naming the
// other as a sibling; an unnamed or mismatched sibling is still refused.
func TestReleasePairHandsOverSiblingPairs(t *testing.T) {
	c := newReleasePairCity(t)
	src2 := c.create("second source")
	convoy2 := c.jsonMap("convoy", "create", "second input", src2, "--json")["convoy_id"].(string)
	mapping := c.jsonMap("formula", "cook", "pair", "--json")["id_mapping"].(map[string]any)
	step2, root2 := mapping["pair.deliver"].(string), mapping["pair"].(string)
	c.run("bd", "update", root2, "--set-metadata", "gc.input_convoy_id="+convoy2, "--set-metadata", "gc.routed_to=worker",
		"--set-metadata", "gc.source_bead_id="+src2)
	c.run("bd", "update", src2, "--assignee", releasePairDeadOwner, "--status", "blocked", "--set-metadata", "gc.execution_routed_to=worker")
	c.run("bd", "update", step2, "--assignee", releasePairDeadOwner, "--status", "blocked",
		"--set-metadata", "gc.session_name=pool-retired", "--set-metadata", "gc.routed_to=worker")

	if _, errOut, code := c.releasePair(); code != 1 || !strings.Contains(errOut, "hand it over first") {
		t.Fatalf("unnamed sibling not refused: exit %d %s", code, errOut)
	}
	if _, errOut, code := c.releasePair("--sibling-pair", src2+":"+c.step); code != 1 || !strings.Contains(errOut, "overlaps") {
		t.Fatalf("overlapping sibling not refused: exit %d %s", code, errOut)
	}
	if _, errOut, code := c.releasePair("--sibling-pair", c.convoy+":"+step2); code != 1 || !strings.Contains(errOut, "sibling") {
		t.Fatalf("mismatched sibling not refused: exit %d %s", code, errOut)
	}

	if out, errOut, code := c.releasePair("--apply", "--sibling-pair", src2+":"+step2); code != 0 || !strings.Contains(out, "result=released") {
		t.Fatalf("first pair: exit %d out=%s err=%s", code, out, errOut)
	}
	if s := c.show(src2); s.Assignee != releasePairDeadOwner || s.Status != "blocked" {
		t.Fatalf("sibling touched by the first handover: %+v", s)
	}
	out, errOut, code := c.exec("convoy", "release-pair", src2, "--step", step2, "--from", releasePairDeadOwner,
		"--old-session", c.session, "--handover-id", "h2", "--manifest", filepath.Join(c.dir, "handover2.json"), "--apply")
	if code != 0 || !strings.Contains(out, "result=released") {
		t.Fatalf("second pair: exit %d out=%s err=%s", code, out, errOut)
	}
	for _, id := range []string{c.source, c.step, src2, step2} {
		if row := c.show(id); row.Assignee != "" || row.Status != "open" || slices.Contains(row.Labels, "hold:mayor") {
			t.Fatalf("%s not released: %+v", id, row)
		}
	}
}
