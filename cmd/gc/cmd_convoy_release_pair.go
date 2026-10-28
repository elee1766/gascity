package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// releasePairPhases is the reviewed staged order: fence both records, clear the
// dead owner, unbind the root, then release the source while the step is still
// fenced so no worker can claim the source without its step.
var releasePairPhases = []string{"fence-step", "fence-source", "prepare-step", "prepare-source", "prepare-root", "release-source", "release-step"}

const (
	releasePairFenceLabel     = beadmeta.HoldMayorLabel
	releasePairHandoverIDKey  = "gc.handover.id"
	releasePairHandoverKey    = "gc.handover.phase"
	releasePairInventoryLimit = 20000
	releasePairExitUnknown    = 2
)

// releasePairSessionKeys are the owner bindings cleared from source and step.
var releasePairSessionKeys = []string{beadmeta.SessionIDMetadataKey, beadmeta.SessionNameMetadataKey, beadmeta.ClaimedAtMetadataKey}

// releasePairProseKeys are metadata keys whose free text may mention ids without
// creating a structural reference.
var releasePairProseKeys = []string{"notes", "comments", "message", "body", "description", "subject", "evidence"}

type releasePairOptions struct {
	Source     string
	Step       string
	From       string
	OldSession string
	HandoverID string
	Manifest   string
	ClearHolds []string
	Siblings   []string
	StopAfter  string
	Apply      bool
	JSON       bool
}

type releasePairSnap struct {
	ID       string            `json:"id"`
	Revision int64             `json:"revision"`
	Status   string            `json:"status"`
	Assignee string            `json:"assignee,omitempty"`
	Type     string            `json:"type,omitempty"`
	ParentID string            `json:"parent,omitempty"`
	Labels   []string          `json:"labels,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type releasePairPending struct {
	Phase            string          `json:"phase"`
	ID               string          `json:"id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Planned          releasePairSnap `json:"planned"`
}

type releasePairInventory struct {
	Scopes     []string          `json:"scopes"`
	References []releasePairSnap `json:"references"`
	Deps       []beads.Dep       `json:"deps"`
	Waits      []string          `json:"waits"`
}

type releasePairManifest struct {
	HandoverID string                     `json:"handover_id"`
	Source     string                     `json:"source"`
	Step       string                     `json:"step"`
	Root       string                     `json:"root"`
	Convoy     string                     `json:"convoy"`
	Finalizer  string                     `json:"finalizer"`
	From       string                     `json:"from"`
	OldSession string                     `json:"old_session"`
	ClearHolds []string                   `json:"clear_holds,omitempty"`
	Siblings   []string                   `json:"sibling_pairs,omitempty"`
	CreatedAt  string                     `json:"created_at"`
	Preimage   map[string]releasePairSnap `json:"preimage"`
	Expected   map[string]releasePairSnap `json:"expected"`
	FenceAdded map[string]bool            `json:"fence_added"`
	Inventory  releasePairInventory       `json:"inventory"`
	Completed  []string                   `json:"completed"`
	Pending    *releasePairPending        `json:"pending,omitempty"`
}

type releasePairResult struct {
	SchemaVersion string   `json:"schema_version"`
	OK            bool     `json:"ok"`
	Command       string   `json:"command"`
	Action        string   `json:"action"`
	HandoverID    string   `json:"handover_id"`
	Manifest      string   `json:"manifest"`
	Result        string   `json:"result"`
	Completed     []string `json:"completed"`
	Next          string   `json:"next,omitempty"`
	Detail        string   `json:"detail,omitempty"`
}

// errReleasePairUnknown marks an interrupted write whose outcome cannot be
// proven either way; the operator reconciles it before any retry.
var errReleasePairUnknown = errors.New("outcome unknown")

func newConvoyReleasePairCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts releasePairOptions
	cmd := &cobra.Command{
		Use:   "release-pair <source-bead-id>",
		Short: "Release a dead owner's workflow source and step to the pool together",
		Long: `Return a workflow source and its execution step, both owned by a
retired session, to their routed pool without creating a new workflow.

The source, input convoy, workflow root, step and finalizer are preserved.
The command fences both records with hold:mayor, clears the dead owner and
session bindings, unbinds the root, then releases the source while the step
is still fenced and finally releases the step. Every phase is a revision
compare-and-swap with exact read-back, and every phase first re-checks all
five records, open structural references and session waits against the
preflight snapshot, refusing on any drift. Reference inventory includes all
configured work scopes and class bindings. An unavailable, suspended or
possibly truncated scope refuses the handover rather than being skipped.

Holds present before the run are kept unless named with --clear-hold.
Progress is persisted to a manifest so an interrupted run resumes from the
last verified phase, and a write whose outcome cannot be proven exits 2 for
operator reconciliation instead of retrying. Without --apply the command
only runs the preflight and prints the plan.

When the retired owner holds more than one pair, name each other pair with
--sibling-pair SOURCE:STEP. Each sibling is verified to be the same
retired owner's source and step of one workflow, and only those records
may remain owned by the retired identity. Run the command once per pair,
naming the not-yet-released pairs as siblings.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.Source = args[0]
			return exitForCode(cmdConvoyReleasePair(opts, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&opts.Step, "step", "", "Execution step bead owned by the same dead owner (required)")
	cmd.Flags().StringVar(&opts.From, "from", "", "Assignee recorded on both source and step (required)")
	cmd.Flags().StringVar(&opts.OldSession, "old-session", "", "Closed session bead that held the pair (required)")
	cmd.Flags().StringVar(&opts.HandoverID, "handover-id", "", "Stable handover identifier (default pair-<source>-<step>)")
	cmd.Flags().StringVar(&opts.Manifest, "manifest", "", "Manifest path (default <city>/.gc/handovers/<handover-id>.json)")
	cmd.Flags().StringSliceVar(&opts.ClearHolds, "clear-hold", nil, "Pre-existing hold label the operator is authorized to clear (repeatable)")
	cmd.Flags().StringSliceVar(&opts.Siblings, "sibling-pair", nil, "Other SOURCE:STEP pair held by the same retired owner (repeatable)")
	cmd.Flags().StringVar(&opts.StopAfter, "stop-after", "", "Stop after this phase ("+strings.Join(releasePairPhases, ", ")+")")
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "Perform the handover; without it only the preflight runs")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output JSON")
	return cmd
}

type releasePairRun struct {
	opts     releasePairOptions
	cityPath string
	cfg      *config.City
	stores   map[string]beads.Store
	sessions *sessionpkg.Store
}

func cmdConvoyReleasePair(opts releasePairOptions, stdout, stderr io.Writer) int {
	fail := func(code int, format string, args ...any) int {
		fmt.Fprintf(stderr, "gc convoy release-pair: "+format+"\n", args...) //nolint:errcheck // best-effort stderr
		return code
	}
	opts.Source = strings.TrimSpace(opts.Source)
	opts.Step = strings.TrimSpace(opts.Step)
	opts.From = strings.TrimSpace(opts.From)
	opts.OldSession = strings.TrimSpace(opts.OldSession)
	if opts.Source == "" || opts.Step == "" || opts.From == "" || opts.OldSession == "" {
		return fail(1, "source, --step, --from and --old-session are required")
	}
	for _, hold := range opts.ClearHolds {
		if !beadmeta.HasDispatchHold([]string{hold}) {
			return fail(1, "--clear-hold %q is not a dispatch hold label", hold)
		}
	}
	for i, pair := range opts.Siblings {
		src, step, ok := strings.Cut(strings.TrimSpace(pair), ":")
		src, step = strings.TrimSpace(src), strings.TrimSpace(step)
		if !ok || src == "" || step == "" || strings.Contains(step, ":") {
			return fail(1, "--sibling-pair %q must be SOURCE:STEP", pair)
		}
		if src == opts.Source || step == opts.Step || src == step {
			return fail(1, "--sibling-pair %q overlaps the pair being released", pair)
		}
		opts.Siblings[i] = src + ":" + step
	}
	if opts.StopAfter != "" && !slices.Contains(releasePairPhases, opts.StopAfter) {
		return fail(1, "--stop-after %q is not a phase", opts.StopAfter)
	}
	if opts.HandoverID == "" {
		opts.HandoverID = "pair-" + opts.Source + "-" + opts.Step
	}
	cityPath, err := resolveCity()
	if err != nil {
		return fail(1, "%v", err)
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return fail(1, "%v", err)
	}
	if opts.Manifest == "" {
		opts.Manifest = filepath.Join(cityPath, ".gc", "handovers", opts.HandoverID+".json")
	}
	workStore, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return fail(1, "opening city store: %v", err)
	}
	run := &releasePairRun{
		opts:     opts,
		cityPath: cityPath,
		cfg:      cfg,
		stores:   map[string]beads.Store{},
		sessions: cliSessionFrontDoor(workStore, cfg, cityPath),
	}
	ctx, cancel := sourceWorkflowCommandContext()
	defer cancel()
	var result releasePairResult
	runErr := sourceworkflow.WithLock(ctx, cityPath, cityPath, opts.Source, func() error {
		var err error
		result, err = run.execute()
		return err
	})
	if runErr != nil {
		code := 1
		if errors.Is(runErr, errReleasePairUnknown) {
			code = releasePairExitUnknown
		}
		if opts.JSON {
			return writeJSONError(stdout, stderr, "release_pair_refused", runErr.Error(), code)
		}
		return fail(code, "%v", runErr)
	}
	if opts.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return fail(1, "encoding result: %v", err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "result=%s handover=%s completed=%s", result.Result, result.HandoverID, strings.Join(result.Completed, ",")) //nolint:errcheck
	if result.Next != "" {
		fmt.Fprintf(stdout, " next=%s", result.Next) //nolint:errcheck
	}
	fmt.Fprintf(stdout, " manifest=%s\n", result.Manifest) //nolint:errcheck
	if result.Detail != "" {
		fmt.Fprintln(stdout, result.Detail) //nolint:errcheck
	}
	return 0
}

func (r *releasePairRun) execute() (releasePairResult, error) {
	m, exists, err := loadReleasePairManifest(r.opts.Manifest)
	if err != nil {
		return releasePairResult{}, err
	}
	if !exists {
		m, err = r.preflight()
		if err != nil {
			return releasePairResult{}, fmt.Errorf("preflight refused: %w", err)
		}
		if !r.opts.Apply {
			return r.result(m, "preflight-verified", "dry run; rerun with --apply to perform the handover"), nil
		}
		if err := saveReleasePairManifest(r.opts.Manifest, m); err != nil {
			return releasePairResult{}, err
		}
	} else {
		if err := r.matchesManifest(m); err != nil {
			return releasePairResult{}, err
		}
		if err := r.bindStores(m); err != nil {
			return releasePairResult{}, err
		}
		if !r.opts.Apply {
			if m.Pending != nil {
				return r.result(m, "pending", "a write is pending reconciliation; rerun with --apply"), nil
			}
			return r.result(m, "in-progress", ""), nil
		}
		if m.Pending != nil {
			if err := r.reconcilePending(m); err != nil {
				return releasePairResult{}, err
			}
		}
	}
	for len(m.Completed) < len(releasePairPhases) {
		if r.opts.StopAfter != "" && slices.Contains(m.Completed, r.opts.StopAfter) {
			return r.result(m, "stopped", ""), nil
		}
		phase := releasePairPhases[len(m.Completed)]
		if err := r.runPhase(m, phase); err != nil {
			return releasePairResult{}, fmt.Errorf("phase %s: %w", phase, err)
		}
	}
	return r.result(m, "released", "the original step and source are open, unassigned and routed; verify a worker claims the step, then the same worker claims the source"), nil
}

func (r *releasePairRun) result(m *releasePairManifest, outcome, detail string) releasePairResult {
	res := releasePairResult{SchemaVersion: "1", OK: true, Command: "convoy.release-pair", Action: "release-pair", HandoverID: m.HandoverID, Manifest: r.opts.Manifest, Result: outcome, Completed: slices.Clone(m.Completed), Detail: detail}
	if len(m.Completed) < len(releasePairPhases) {
		res.Next = releasePairPhases[len(m.Completed)]
	}
	if res.Completed == nil {
		res.Completed = []string{}
	}
	return res
}

func (r *releasePairRun) matchesManifest(m *releasePairManifest) error {
	if m.HandoverID != r.opts.HandoverID || m.Source != r.opts.Source || m.Step != r.opts.Step || m.From != r.opts.From || m.OldSession != r.opts.OldSession {
		return fmt.Errorf("manifest %s records a different handover; inspect it instead of reusing it", r.opts.Manifest)
	}
	if !slices.Equal(sortedStrings(m.Siblings), sortedStrings(r.opts.Siblings)) {
		return fmt.Errorf("manifest %s was prepared with --sibling-pair %v; resume with the same pairs", r.opts.Manifest, m.Siblings)
	}
	if !slices.Equal(sortedStrings(m.ClearHolds), sortedStrings(r.opts.ClearHolds)) {
		return fmt.Errorf("manifest %s was authorized with --clear-hold %v; resume with the same authority", r.opts.Manifest, m.ClearHolds)
	}
	if len(m.Completed) > len(releasePairPhases) || !slices.Equal(m.Completed, releasePairPhases[:len(m.Completed)]) {
		return fmt.Errorf("manifest %s phase sequence is corrupt: %v", r.opts.Manifest, m.Completed)
	}
	return nil
}

// resolve finds the single store that owns id and remembers it.
func (r *releasePairRun) resolve(id string) (beads.Store, beads.Bead, error) {
	if store, ok := r.stores[id]; ok {
		b, err := store.Get(id)
		return store, b, err
	}
	store, _, err := resolveOwningStoreDir(id, r.cfg, r.cityPath, func(dir string) (beads.Store, error) {
		return openStoreAtForCity(dir, r.cityPath)
	})
	if err != nil {
		return nil, beads.Bead{}, fmt.Errorf("resolving %s: %w", id, err)
	}
	b, err := store.Get(id)
	if err != nil {
		return nil, beads.Bead{}, fmt.Errorf("reading %s: %w", id, err)
	}
	r.stores[id] = store
	return store, b, nil
}

func (r *releasePairRun) bindStores(m *releasePairManifest) error {
	for _, id := range m.ids() {
		if _, _, err := r.resolve(id); err != nil {
			return err
		}
	}
	return nil
}

func (m *releasePairManifest) ids() []string {
	return []string{m.Source, m.Convoy, m.Root, m.Step, m.Finalizer}
}

func (r *releasePairRun) preflight() (*releasePairManifest, error) {
	o := r.opts
	_, source, err := r.resolve(o.Source)
	if err != nil {
		return nil, err
	}
	stepStore, step, err := r.resolve(o.Step)
	if err != nil {
		return nil, err
	}
	rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return nil, fmt.Errorf("step %s carries no %s", step.ID, beadmeta.RootBeadIDMetadataKey)
	}
	_, root, err := r.resolve(rootID)
	if err != nil {
		return nil, err
	}
	convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if convoyID == "" {
		return nil, fmt.Errorf("root %s carries no %s", root.ID, beadmeta.InputConvoyIDMetadataKey)
	}
	convoyStore, convoyBead, err := r.resolve(convoyID)
	if err != nil {
		return nil, err
	}
	session, err := r.sessions.Get(o.OldSession)
	if err != nil {
		return nil, fmt.Errorf("reading old session %s: %w", o.OldSession, err)
	}
	if !session.Closed {
		return nil, fmt.Errorf("old session %s is not closed", o.OldSession)
	}
	if session.ID != o.From && session.Alias != o.From && session.AgentName != o.From {
		return nil, fmt.Errorf("old session %s belongs to %q, not %q", o.OldSession, firstNonEmpty(session.Alias, session.AgentName), o.From)
	}
	for _, b := range []beads.Bead{source, step} {
		if b.Assignee != o.From {
			return nil, fmt.Errorf("%s is assigned to %q, not %q", b.ID, b.Assignee, o.From)
		}
		if b.Status != "open" && b.Status != "in_progress" && b.Status != "blocked" {
			return nil, fmt.Errorf("%s status %q cannot be handed over", b.ID, b.Status)
		}
		if err := releasePairSessionRefsStale(b, o.OldSession, session.SessionName); err != nil {
			return nil, err
		}
		if deadline := strings.TrimSpace(b.Metadata[beads.WorkLeaseExpiresMetadataKey]); deadline != "" {
			expires, err := time.Parse(time.RFC3339Nano, deadline)
			if err != nil {
				return nil, fmt.Errorf("%s lease deadline %q is unreadable: %w", b.ID, deadline, err)
			}
			if time.Now().Before(expires) {
				return nil, fmt.Errorf("%s holds an unexpired work lease until %s", b.ID, deadline)
			}
		}
	}
	if root.Status == "closed" || convoyBead.Status == "closed" {
		return nil, fmt.Errorf("root %s or convoy %s is closed", root.ID, convoyBead.ID)
	}
	if err := releasePairSessionRefsStale(root, o.OldSession, session.SessionName); err != nil {
		return nil, err
	}
	if src := strings.TrimSpace(root.Metadata[beadmeta.SourceBeadIDMetadataKey]); src != "" && src != source.ID {
		return nil, fmt.Errorf("root %s is sourced from %s, not %s", root.ID, src, source.ID)
	}
	if err := releasePairConvoyTracks(convoyStore, convoyID, source.ID); err != nil {
		return nil, err
	}
	family, err := beads.HandlesFor(stepStore).Live.List(beads.ListQuery{
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing workflow %s: %w", root.ID, err)
	}
	finalizerID := ""
	for _, member := range family {
		if member.ID == step.ID || member.Status == "closed" {
			continue
		}
		if strings.TrimSpace(member.Assignee) != "" {
			return nil, fmt.Errorf("workflow member %s is assigned to %q; only the named step may be owned", member.ID, member.Assignee)
		}
		if member.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize {
			if finalizerID != "" {
				return nil, fmt.Errorf("workflow %s has more than one open finalizer", root.ID)
			}
			finalizerID = member.ID
		}
	}
	if finalizerID == "" {
		return nil, fmt.Errorf("workflow %s has no open finalizer", root.ID)
	}
	if _, _, err := r.resolve(finalizerID); err != nil {
		return nil, err
	}
	m := &releasePairManifest{
		HandoverID: o.HandoverID,
		Source:     source.ID,
		Step:       step.ID,
		Root:       root.ID,
		Convoy:     convoyID,
		Finalizer:  finalizerID,
		From:       o.From,
		OldSession: o.OldSession,
		ClearHolds: sortedStrings(o.ClearHolds),
		Siblings:   sortedStrings(o.Siblings),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Preimage:   map[string]releasePairSnap{},
		Expected:   map[string]releasePairSnap{},
		FenceAdded: map[string]bool{},
		Completed:  []string{},
	}
	for _, id := range m.ids() {
		_, b, err := r.resolve(id)
		if err != nil {
			return nil, err
		}
		m.Preimage[id] = releasePairSnapOf(b)
		m.Expected[id] = releasePairSnapOf(b)
	}
	siblingIDs, err := r.verifySiblings(m, session.SessionName)
	if err != nil {
		return nil, err
	}
	inv, err := r.inventory(m)
	if err != nil {
		return nil, err
	}
	for _, ref := range inv.References {
		if ref.Assignee == o.From && !siblingIDs[ref.ID] {
			return nil, fmt.Errorf("open reference %s is also owned by %q; hand it over first", ref.ID, o.From)
		}
	}
	if len(inv.Waits) > 0 {
		return nil, fmt.Errorf("open session waits reference the pair: %s", strings.Join(inv.Waits, ","))
	}
	m.Inventory = inv
	return m, nil
}

// verifySiblings checks each named sibling is another source/step pair of the
// same retired owner and returns the ids allowed to stay owned by it.
func (r *releasePairRun) verifySiblings(m *releasePairManifest, oldSessionName string) (map[string]bool, error) {
	allowed := map[string]bool{}
	for _, pair := range m.Siblings {
		srcID, stepID, _ := strings.Cut(pair, ":")
		if allowed[srcID] || allowed[stepID] || slices.Contains(m.ids(), srcID) || slices.Contains(m.ids(), stepID) {
			return nil, fmt.Errorf("sibling pair %s overlaps another pair", pair)
		}
		_, src, err := r.resolve(srcID)
		if err != nil {
			return nil, err
		}
		_, step, err := r.resolve(stepID)
		if err != nil {
			return nil, err
		}
		for _, b := range []beads.Bead{src, step} {
			if b.Assignee != m.From {
				return nil, fmt.Errorf("sibling %s is assigned to %q, not %q", b.ID, b.Assignee, m.From)
			}
			if b.Status == "closed" {
				return nil, fmt.Errorf("sibling %s is closed", b.ID)
			}
			if err := releasePairSessionRefsStale(b, m.OldSession, oldSessionName); err != nil {
				return nil, fmt.Errorf("sibling %w", err)
			}
		}
		rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" || rootID == m.Root {
			return nil, fmt.Errorf("sibling step %s does not belong to a separate workflow", step.ID)
		}
		_, root, err := r.resolve(rootID)
		if err != nil {
			return nil, err
		}
		if s := strings.TrimSpace(root.Metadata[beadmeta.SourceBeadIDMetadataKey]); s != "" && s != src.ID {
			return nil, fmt.Errorf("sibling root %s is sourced from %s, not %s", root.ID, s, src.ID)
		}
		convoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if convoyID == "" {
			return nil, fmt.Errorf("sibling root %s carries no %s", root.ID, beadmeta.InputConvoyIDMetadataKey)
		}
		convoyStore, _, err := r.resolve(convoyID)
		if err != nil {
			return nil, err
		}
		if err := releasePairConvoyTracks(convoyStore, convoyID, src.ID); err != nil {
			return nil, fmt.Errorf("sibling %w", err)
		}
		allowed[src.ID], allowed[step.ID] = true, true
	}
	return allowed, nil
}

// releasePairSessionRefsStale refuses a record bound to any session other than
// the closed one being retired.
func releasePairSessionRefsStale(b beads.Bead, oldSessionID, oldSessionName string) error {
	if v := strings.TrimSpace(b.Metadata[beadmeta.SessionIDMetadataKey]); v != "" && v != oldSessionID {
		return fmt.Errorf("%s is bound to session %s, not the retired %s", b.ID, v, oldSessionID)
	}
	if v := strings.TrimSpace(b.Metadata[beadmeta.SessionNameMetadataKey]); v != "" && v != oldSessionName && !strings.HasSuffix(v, oldSessionID) {
		return fmt.Errorf("%s is bound to session name %s, not the retired %s", b.ID, v, oldSessionID)
	}
	return nil
}

func releasePairConvoyTracks(store beads.Store, convoyID, sourceID string) error {
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		return fmt.Errorf("listing convoy %s members: %w", convoyID, err)
	}
	for _, dep := range deps {
		if dep.DependsOnID == sourceID && dep.Type == convoy.TrackingDepType {
			return nil
		}
	}
	return fmt.Errorf("convoy %s does not track source %s", convoyID, sourceID)
}

// inventory captures every open record outside the five that structurally names
// the pair or the retired session, the pair's dependency edges, and open waits.
// Any read error, partial answer or possible truncation fails closed.
func (r *releasePairRun) inventory(m *releasePairManifest) (releasePairInventory, error) {
	current, err := r.sessions.List("", "")
	if err != nil {
		return releasePairInventory{}, fmt.Errorf("checking replacement sessions: %w", err)
	}
	for _, candidate := range current {
		if candidate.AgentName == m.From || slices.Contains(sessionpkg.AssigneeIdentities(candidate), m.From) {
			return releasePairInventory{}, fmt.Errorf("open session %s still owns %q; refusing handover", candidate.ID, m.From)
		}
	}
	needles := append(m.ids(), m.OldSession, m.From)
	own := map[string]bool{}
	for _, id := range m.ids() {
		own[id] = true
	}
	var inv releasePairInventory
	candidates, skipped := convoyCheckStoreCandidates(r.cfg, r.cityPath)
	if len(skipped) != 0 {
		return inv, fmt.Errorf("cannot inventory suspended rig stores %v; refusing an incomplete handover", skipped)
	}
	views := make([]convoyStoreView, 0, len(candidates)+len(coordclass.Classes()))
	for _, dir := range candidates {
		store, err := openStoreAtForCity(dir, r.cityPath)
		if err != nil {
			return inv, fmt.Errorf("opening reference scope %s: %w", dir, err)
		}
		views = append(views, convoyStoreView{path: dir, store: store})
	}
	routes := cliStorageRoutes(r.cityPath)
	for _, class := range coordclass.Classes() {
		if store, relocated := routes.storeFor(class); relocated {
			if store == nil {
				return inv, fmt.Errorf("reference class %s has no store", class)
			}
			views = append(views, convoyStoreView{path: "class:" + class.String(), store: store})
		}
	}
	seenStores := []beads.Store{}
	seenRefs := map[string]bool{}
	for _, view := range views {
		inv.Scopes = append(inv.Scopes, view.path)
		store := view.store
		if slices.ContainsFunc(seenStores, func(s beads.Store) bool { return s == store }) {
			continue
		}
		seenStores = append(seenStores, store)
		rows, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth, Limit: releasePairInventoryLimit + 1})
		if err != nil {
			return inv, fmt.Errorf("listing references in %s: %w", view.path, err)
		}
		if len(rows) > releasePairInventoryLimit {
			return inv, fmt.Errorf("reference scan exceeded %d open records; refusing a possibly truncated inventory", releasePairInventoryLimit)
		}
		for _, row := range rows {
			if own[row.ID] || seenRefs[row.ID] || row.Status == "closed" {
				continue
			}
			if releasePairReferences(row, needles) {
				seenRefs[row.ID] = true
				inv.References = append(inv.References, releasePairSnapOf(row))
			}
		}
	}
	sort.Strings(inv.Scopes)
	sort.Slice(inv.References, func(i, j int) bool { return inv.References[i].ID < inv.References[j].ID })
	for _, id := range []string{m.Source, m.Step} {
		for _, direction := range []string{"up", "down"} {
			deps, err := r.stores[id].DepList(id, direction)
			if err != nil {
				return inv, fmt.Errorf("listing %s dependencies of %s: %w", direction, id, err)
			}
			inv.Deps = append(inv.Deps, deps...)
		}
	}
	sort.Slice(inv.Deps, func(i, j int) bool {
		a, b := inv.Deps[i], inv.Deps[j]
		return a.IssueID+"\x00"+a.DependsOnID+"\x00"+a.Type < b.IssueID+"\x00"+b.DependsOnID+"\x00"+b.Type
	})
	waits, err := r.sessions.ListWaits("", "")
	if err != nil {
		return inv, fmt.Errorf("listing session waits: %w", err)
	}
	for _, w := range waits {
		if sessionpkg.IsWaitTerminalState(w.State) || w.Status == "closed" {
			continue
		}
		refs := w.SessionID == m.OldSession || slices.ContainsFunc(w.DepIDs, func(dep string) bool { return slices.Contains(needles, dep) })
		if refs {
			inv.Waits = append(inv.Waits, w.ID)
		}
	}
	sort.Strings(inv.Waits)
	if inv.References == nil {
		inv.References = []releasePairSnap{}
	}
	if inv.Deps == nil {
		inv.Deps = []beads.Dep{}
	}
	if inv.Waits == nil {
		inv.Waits = []string{}
	}
	return inv, nil
}

func releasePairReferences(b beads.Bead, needles []string) bool {
	fields := []string{b.Assignee, b.ParentID}
	for k, v := range b.Metadata {
		if slices.ContainsFunc(releasePairProseKeys, func(word string) bool { return strings.Contains(k, word) }) {
			continue
		}
		fields = append(fields, v)
	}
	for _, dep := range b.Dependencies {
		fields = append(fields, dep.IssueID, dep.DependsOnID)
	}
	for _, field := range fields {
		for _, needle := range needles {
			if needle != "" && strings.Contains(field, needle) {
				return true
			}
		}
	}
	return false
}

// check re-reads all five records and the inventory and refuses on any drift
// from the last verified postimage.
func (r *releasePairRun) check(m *releasePairManifest) error {
	for _, id := range m.ids() {
		b, err := r.stores[id].Get(id)
		if err != nil {
			return fmt.Errorf("re-reading %s: %w", id, err)
		}
		if diff := releasePairDiff(m.Expected[id], releasePairSnapOf(b), true); diff != "" {
			return fmt.Errorf("%s changed since the last verified phase (%s); refusing", id, diff)
		}
	}
	inv, err := r.inventory(m)
	if err != nil {
		return err
	}
	if diff := releasePairInventoryDiff(m.Inventory, inv); diff != "" {
		return fmt.Errorf("references or waits changed since preflight (%s); refusing", diff)
	}
	session, err := r.sessions.Get(m.OldSession)
	if err != nil {
		return fmt.Errorf("re-reading old session %s: %w", m.OldSession, err)
	}
	if !session.Closed {
		return fmt.Errorf("old session %s is no longer closed", m.OldSession)
	}
	return nil
}

// plan returns the record a phase writes, the update and the exact postimage.
func (r *releasePairRun) plan(m *releasePairManifest, phase string) (string, beads.UpdateOpts, releasePairSnap, error) {
	id := m.Step
	switch {
	case strings.HasSuffix(phase, "-source"):
		id = m.Source
	case strings.HasSuffix(phase, "-root"):
		id = m.Root
	}
	before := m.Expected[id]
	meta := map[string]string{releasePairHandoverIDKey: m.HandoverID, releasePairHandoverKey: phase}
	opts := beads.UpdateOpts{Metadata: meta}
	after := before.clone()
	switch {
	case strings.HasPrefix(phase, "fence"):
		if !slices.Contains(before.Labels, releasePairFenceLabel) {
			opts.Labels = []string{releasePairFenceLabel}
			after.Labels = append(after.Labels, releasePairFenceLabel)
		}
	case strings.HasPrefix(phase, "prepare"):
		for _, key := range releasePairSessionKeys {
			if before.Metadata[key] != "" {
				meta[key] = ""
			}
		}
		if id != m.Root {
			empty, open := "", "open"
			opts.Assignee, opts.Status = &empty, &open
			after.Assignee, after.Status = "", "open"
			if before.Metadata[beads.WorkLeaseExpiresMetadataKey] != "" {
				meta[beads.WorkLeaseExpiresMetadataKey] = ""
			}
		}
	case strings.HasPrefix(phase, "release"):
		if before.Status != "open" || before.Assignee != "" {
			return "", opts, after, fmt.Errorf("%s is not open and unassigned before release", id)
		}
		var remove []string
		if m.FenceAdded[id] {
			remove = append(remove, releasePairFenceLabel)
		}
		for _, hold := range m.ClearHolds {
			if slices.Contains(before.Labels, hold) && !slices.Contains(remove, hold) {
				remove = append(remove, hold)
			}
		}
		if len(remove) > 0 {
			opts.RemoveLabels = remove
			after.Labels = slices.DeleteFunc(after.Labels, func(l string) bool { return slices.Contains(remove, l) })
		}
	}
	for k, v := range meta {
		after.Metadata[k] = v
	}
	return id, opts, after, nil
}

func (r *releasePairRun) runPhase(m *releasePairManifest, phase string) error {
	if err := r.check(m); err != nil {
		return err
	}
	id, opts, planned, err := r.plan(m, phase)
	if err != nil {
		return err
	}
	before := m.Expected[id]
	writer, ok := beads.ConditionalWriterForTarget(r.stores[id])
	if !ok {
		return fmt.Errorf("store owning %s has no revision-guarded writes", id)
	}
	m.Pending = &releasePairPending{Phase: phase, ID: id, ExpectedRevision: before.Revision, Planned: planned}
	if err := saveReleasePairManifest(r.opts.Manifest, m); err != nil {
		return err
	}
	writeErr := writer.UpdateIfMatch(id, before.Revision, opts)
	if writeErr != nil && (beads.IsPreconditionFailed(writeErr) || isReleasePairRefusedBeforeWrite(writeErr)) {
		m.Pending = nil
		if err := saveReleasePairManifest(r.opts.Manifest, m); err != nil {
			return err
		}
		return fmt.Errorf("conditional write on %s refused, nothing written: %w", id, writeErr)
	}
	// Success and unclassified errors both reconcile against the store.
	if err := r.reconcilePending(m); err != nil {
		if writeErr != nil {
			return fmt.Errorf("%w (write error: %w)", err, writeErr)
		}
		return err
	}
	if !slices.Contains(m.Completed, phase) {
		return fmt.Errorf("conditional write on %s did not land: %w", id, writeErr)
	}
	return nil
}

func isReleasePairRefusedBeforeWrite(err error) bool {
	var unsupported *beads.ConditionalUpdateFieldUnsupportedError
	return errors.As(err, &unsupported) || errors.Is(err, beads.ErrConditionalWriteUnsupported) || errors.Is(err, beads.ErrEmptyConditionalUpdate)
}

// reconcilePending decides an interrupted or just-issued write from the store:
// the exact planned postimage completes the phase, the untouched preimage
// clears it for retry, anything else is an unknown outcome.
func (r *releasePairRun) reconcilePending(m *releasePairManifest) error {
	p := m.Pending
	b, err := r.stores[p.ID].Get(p.ID)
	if err != nil {
		return fmt.Errorf("%w: re-reading %s after %s: %w", errReleasePairUnknown, p.ID, p.Phase, err)
	}
	current := releasePairSnapOf(b)
	switch {
	case current.Revision != p.ExpectedRevision && releasePairDiff(p.Planned, current, false) == "":
		if strings.HasPrefix(p.Phase, "fence") && !slices.Contains(m.Expected[p.ID].Labels, releasePairFenceLabel) {
			m.FenceAdded[p.ID] = true
		}
		m.Expected[p.ID] = current
		m.Completed = append(m.Completed, p.Phase)
		m.Pending = nil
	case releasePairDiff(m.Expected[p.ID], current, true) == "":
		m.Pending = nil
	default:
		return fmt.Errorf("%w: %s after %s matches neither the preimage nor the planned postimage (%s); reconcile using %s before retrying",
			errReleasePairUnknown, p.ID, p.Phase, releasePairDiff(p.Planned, current, false), r.opts.Manifest)
	}
	return saveReleasePairManifest(r.opts.Manifest, m)
}

func releasePairSnapOf(b beads.Bead) releasePairSnap {
	meta := make(map[string]string, len(b.Metadata))
	for k, v := range b.Metadata {
		meta[k] = v
	}
	return releasePairSnap{
		ID:       b.ID,
		Revision: b.Revision,
		Status:   b.Status,
		Assignee: b.Assignee,
		Type:     b.Type,
		ParentID: b.ParentID,
		Labels:   sortedStrings(b.Labels),
		Metadata: meta,
	}
}

func (s releasePairSnap) clone() releasePairSnap {
	out := s
	out.Labels = slices.Clone(s.Labels)
	out.Metadata = make(map[string]string, len(s.Metadata))
	for k, v := range s.Metadata {
		out.Metadata[k] = v
	}
	return out
}

// releasePairDiff names the first difference; withRevision also compares the
// revision token.
func releasePairDiff(want, got releasePairSnap, withRevision bool) string {
	switch {
	case withRevision && want.Revision != got.Revision:
		return fmt.Sprintf("revision %d -> %d", want.Revision, got.Revision)
	case want.Status != got.Status:
		return fmt.Sprintf("status %q -> %q", want.Status, got.Status)
	case want.Assignee != got.Assignee:
		return fmt.Sprintf("assignee %q -> %q", want.Assignee, got.Assignee)
	case want.Type != got.Type || want.ParentID != got.ParentID:
		return "type or parent"
	case !slices.Equal(sortedStrings(want.Labels), sortedStrings(got.Labels)):
		return fmt.Sprintf("labels %v -> %v", want.Labels, got.Labels)
	}
	keys := map[string]bool{}
	for k := range want.Metadata {
		keys[k] = true
	}
	for k := range got.Metadata {
		keys[k] = true
	}
	for k := range keys {
		if want.Metadata[k] != got.Metadata[k] {
			return fmt.Sprintf("metadata %s %q -> %q", k, want.Metadata[k], got.Metadata[k])
		}
	}
	return ""
}

func releasePairInventoryDiff(want, got releasePairInventory) string {
	if !slices.Equal(want.Scopes, got.Scopes) {
		return fmt.Sprintf("reference scopes %v -> %v", want.Scopes, got.Scopes)
	}
	if len(want.References) != len(got.References) {
		return fmt.Sprintf("%d open references -> %d", len(want.References), len(got.References))
	}
	for i := range want.References {
		if want.References[i].ID != got.References[i].ID {
			return fmt.Sprintf("reference %s -> %s", want.References[i].ID, got.References[i].ID)
		}
		if diff := releasePairDiff(want.References[i], got.References[i], true); diff != "" {
			return want.References[i].ID + " " + diff
		}
	}
	if !slices.Equal(want.Deps, got.Deps) {
		return "dependency edges"
	}
	if !slices.Equal(want.Waits, got.Waits) {
		return fmt.Sprintf("waits %v -> %v", want.Waits, got.Waits)
	}
	return ""
}

func loadReleasePairManifest(path string) (*releasePairManifest, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	var m releasePairManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("decoding manifest %s: %w", path, err)
	}
	if m.FenceAdded == nil {
		m.FenceAdded = map[string]bool{}
	}
	return &m, true, nil
}

func saveReleasePairManifest(path string, m *releasePairManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("committing manifest: %w", err)
	}
	return nil
}
