package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	sqliteWorkNotesKey    = "gc.work.notes"
	sqliteWorkCommentsKey = "gc.work.comments"
)

type sqliteWorkComment struct {
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type sqliteWorkView struct {
	beads.Bead
	Revision int64               `json:"revision"`
	Notes    string              `json:"notes,omitempty"`
	Comments []sqliteWorkComment `json:"comments,omitempty"`
}

func sqliteWorkBeadView(b beads.Bead) (sqliteWorkView, error) {
	view := sqliteWorkView{Bead: b, Revision: b.Revision, Notes: b.Metadata[sqliteWorkNotesKey]}
	if value := b.Metadata[sqliteWorkCommentsKey]; value != "" {
		if err := json.Unmarshal([]byte(value), &view.Comments); err != nil {
			return view, fmt.Errorf("reading comments of %s: %w", b.ID, err)
		}
	}
	return view, nil
}

// doSQLiteWorkBd is a closed native command surface. Unsupported commands and
// flags fail before a write, and never fall through to an unrelated bd ledger.
func doSQLiteWorkBd(cityPath string, cfg *config.City, target execStoreTarget, args []string, stdout, stderr io.Writer) int {
	if err := runSQLiteWorkBd(cityPath, cfg, target, args, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "gc bd sqlite-work: %v\n", err) //nolint:errcheck
		return 1
	}
	return 0
}

func runSQLiteWorkBd(cityPath string, cfg *config.City, target execStoreTarget, args []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("gc bd sqlite-work", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOut := flags.Bool("json", false, "JSON output")
	actor := flags.String("actor", strings.TrimSpace(os.Getenv("BEADS_ACTOR")), "claim actor")
	_ = flags.StringP("directory", "C", "", "already-resolved scope")
	_ = flags.BoolP("quiet", "q", false, "quiet output")
	title := flags.String("title", "", "title")
	description := flags.StringP("description", "d", "", "description")
	kind := flags.StringP("type", "t", "task", "type")
	status := flags.StringP("status", "s", "", "status")
	priority := flags.IntP("priority", "p", 2, "priority")
	assignee := flags.StringP("assignee", "a", "", "assignee")
	matchAssignee := flags.String("if-assignee", "", "update only this owner")
	matchStatus := flags.String("if-status", "", "update only this status")
	matchRevision := flags.Int64("if-revision", 0, "update only this exact observed revision")
	idFlag := flags.String("id", "", "exact ID")
	parent := flags.String("parent", "", "parent ID")
	externalRef := flags.String("external-ref", "", "external issue URL")
	labels := flags.StringSliceP("labels", "l", nil, "labels")
	addLabels := flags.StringSlice("add-label", nil, "add labels")
	removeLabels := flags.StringSlice("remove-label", nil, "remove labels")
	metadata := flags.String("metadata", "", "metadata JSON")
	setMetadata := flags.StringArray("set-metadata", nil, "key=value")
	unsetMetadata := flags.StringArray("unset-metadata", nil, "metadata keys")
	notes := flags.String("notes", "", "replace notes")
	appendNotes := flags.String("append-notes", "", "append notes")
	claim := flags.Bool("claim", false, "atomically claim")
	reason := flags.StringP("reason", "r", "", "close reason")
	all := flags.Bool("all", false, "include closed")
	limit := flags.IntP("limit", "n", 0, "limit")
	direction := flags.String("direction", "down", "dependency direction")
	depType := flags.String("dep-type", "blocks", "dependency type")
	if err := flags.Parse(args); err != nil {
		return err
	}
	positionals := flags.Args()
	if len(positionals) == 0 {
		return errors.New("expected create, list, ready, show, update, heartbeat, reclaim, close, reopen, dep, or comments")
	}
	verb := positionals[0]
	positionals = positionals[1:]
	if verb == "new" {
		verb = "create"
	}
	allowed := map[string]string{
		"init":               "",
		"create":             "title description type status priority assignee id parent labels metadata set-metadata notes external-ref",
		"list":               "type status assignee parent labels all limit",
		"ready":              "assignee labels limit",
		"show":               "",
		"update":             "title description type status priority assignee parent labels add-label remove-label metadata set-metadata unset-metadata notes append-notes claim external-ref if-assignee if-status if-revision",
		"heartbeat":          "",
		"reclaim":            "id",
		"close":              "reason",
		"reopen":             "",
		"release-if-current": "",
		"dep":                "direction dep-type type",
		"comments":           "",
	}
	allowedFlags, supported := allowed[verb]
	if !supported {
		return fmt.Errorf("unsupported command %q", verb)
	}
	var flagErr error
	flags.Visit(func(f *pflag.Flag) {
		if !strings.Contains(" json actor directory quiet "+allowedFlags+" ", " "+f.Name+" ") {
			flagErr = fmt.Errorf("flag --%s is not supported for %s", f.Name, verb)
		}
	})
	if flagErr != nil {
		return flagErr
	}
	if flags.Changed("if-revision") && *matchRevision <= 0 {
		return errors.New("--if-revision must be positive")
	}
	if *priority < 0 || *priority > 4 {
		return errors.New("priority must be between 0 and 4")
	}
	if *limit < 0 {
		return errors.New("limit must not be negative")
	}
	patch := make(map[string]string)
	if *metadata != "" {
		if err := json.Unmarshal([]byte(*metadata), &patch); err != nil {
			return fmt.Errorf("metadata must be a string-valued JSON object: %w", err)
		}
		if patch == nil {
			return errors.New("metadata must be a JSON object")
		}
	}
	for _, pair := range *setMetadata {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return fmt.Errorf("invalid metadata pair %q", pair)
		}
		patch[key] = value
	}
	if flags.Changed("notes") && flags.Changed("append-notes") {
		return errors.New("notes and append-notes cannot be combined")
	}
	if flags.Changed("notes") {
		patch[sqliteWorkNotesKey] = *notes
	}
	if flags.Changed("external-ref") {
		patch["external_ref"] = *externalRef
	}
	if *reason != "" {
		patch["close_reason"] = *reason
	}

	if verb == "init" {
		if len(positionals) != 0 {
			return errors.New("init takes no positional arguments")
		}
		return initSQLiteWorkScope(target.ScopeRoot, cityPath)
	}
	store, err := openSQLiteWorkScope(target.ScopeRoot, cityPath, false)
	if err != nil {
		return err
	}
	defer store.CloseStore() //nolint:errcheck
	emitter := &emittingClassStore{Store: store, cityPath: cityPath}
	printRows := func(rows []beads.Bead) error {
		views := make([]sqliteWorkView, 0, len(rows))
		for _, b := range rows {
			view, err := sqliteWorkBeadView(b)
			if err != nil {
				return err
			}
			views = append(views, view)
		}
		if *jsonOut {
			return json.NewEncoder(stdout).Encode(views)
		}
		for _, view := range views {
			if _, err := fmt.Fprintf(stdout, "%s [%s] %s\n%s\n%s\n", view.ID, view.Status, view.Title, view.Description, view.Notes); err != nil {
				return err
			}
		}
		return nil
	}
	oneID := func() (string, error) {
		if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
			return "", fmt.Errorf("%s requires exactly one ID", verb)
		}
		return positionals[0], nil
	}
	switch verb {
	case "create":
		if *title == "" && len(positionals) == 1 {
			*title = positionals[0]
			positionals = nil
		}
		if len(positionals) != 0 || strings.TrimSpace(*title) == "" {
			return errors.New("create requires one nonempty title")
		}
		b := beads.Bead{ID: *idFlag, Title: *title, Description: *description, Type: *kind, Status: *status, Priority: priority, Assignee: *assignee, ParentID: *parent, Labels: *labels, Metadata: patch}
		b, err = emitter.Create(b)
		if err != nil {
			return err
		}
		if *jsonOut {
			view, err := sqliteWorkBeadView(b)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(view)
		}
		_, err = fmt.Fprintln(stdout, b.ID)
		return err
	case "list", "ready":
		if len(positionals) != 0 {
			return fmt.Errorf("%s takes no positional arguments", verb)
		}
		var rows []beads.Bead
		if verb == "ready" {
			rows, err = store.Ready(beads.ReadyQuery{Assignee: *assignee, TierMode: beads.TierBoth})
		} else {
			query := beads.ListQuery{Status: *status, Assignee: *assignee, ParentID: *parent, IncludeClosed: *all, AllowScan: true, TierMode: beads.TierBoth}
			if flags.Changed("type") {
				query.Type = *kind
			}
			rows, err = store.List(query)
		}
		if err != nil {
			return err
		}
		filtered := rows[:0]
		for _, row := range rows {
			match := true
			for _, label := range *labels {
				found := false
				for _, have := range row.Labels {
					if have == label {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				filtered = append(filtered, row)
			}
		}
		if *limit > 0 && len(filtered) > *limit {
			filtered = filtered[:*limit]
		}
		return printRows(filtered)
	case "show":
		if len(positionals) == 0 {
			return errors.New("show requires IDs")
		}
		rows := make([]beads.Bead, 0, len(positionals))
		for _, id := range positionals {
			b, err := store.Get(id)
			if err != nil {
				return err
			}
			rows = append(rows, b)
		}
		return printRows(rows)
	case "heartbeat":
		id, err := oneID()
		if err != nil {
			return err
		}
		if _, err = store.Heartbeat(id, *actor); err != nil {
			return err
		}
		emitter.emitUpdated(id)
		b, err := store.Get(id)
		if err != nil {
			return err
		}
		return printRows([]beads.Bead{b})
	case "reclaim":
		if *idFlag != "" {
			positionals = append(positionals, *idFlag)
		}
		id, err := oneID()
		if err != nil {
			return err
		}
		changed, _, err := store.ReclaimStale(id)
		if err != nil {
			return err
		}
		if changed {
			emitter.emitUpdated(id)
		}
		return json.NewEncoder(stdout).Encode(struct {
			Reclaimed bool `json:"reclaimed"`
		}{changed})
	case "release-if-current":
		if len(positionals) != 2 {
			return errors.New("release-if-current requires ID and expected assignee")
		}
		changed, err := emitter.ReleaseIfCurrent(positionals[0], positionals[1])
		if err != nil {
			return err
		}
		message := "skipped"
		if changed {
			message = "released"
		}
		_, err = fmt.Fprintln(stdout, message)
		return err
	case "comments":
		if len(positionals) < 2 {
			return errors.New("comments requires list ID or add ID text")
		}
		operation, id := positionals[0], positionals[1]
		if operation == "add" && len(positionals) == 3 {
			if strings.TrimSpace(*actor) == "" {
				return errors.New("comments add requires an actor")
			}
			if err := updateSQLiteWorkText(store, id, func(b beads.Bead) (map[string]string, error) {
				view, err := sqliteWorkBeadView(b)
				if err != nil {
					return nil, err
				}
				view.Comments = append(view.Comments, sqliteWorkComment{Author: *actor, Text: positionals[2], CreatedAt: time.Now().UTC()})
				data, err := json.Marshal(view.Comments)
				return map[string]string{sqliteWorkCommentsKey: string(data)}, err
			}); err != nil {
				return err
			}
			emitter.emitUpdated(id)
		} else if operation != "list" || len(positionals) != 2 {
			return errors.New("comments requires list ID or add ID text")
		}
		b, err := store.Get(id)
		if err != nil {
			return err
		}
		view, err := sqliteWorkBeadView(b)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(view.Comments)
	case "dep":
		if len(positionals) < 2 {
			return errors.New("dep requires add ID DEPENDENCY, remove ID DEPENDENCY, or list ID")
		}
		operation, id := positionals[0], positionals[1]
		if _, err := store.Get(id); err != nil {
			return err
		}
		switch operation {
		case "list":
			if len(positionals) != 2 {
				return errors.New("dep list requires one ID")
			}
			filter := ""
			if flags.Changed("type") {
				filter = *kind
			} else if flags.Changed("dep-type") {
				filter = *depType
			}
			rows, err := sqliteWorkDependencyRows(cityPath, target.ScopeRoot, store, id, *direction, filter)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(rows)
		case "add", "remove":
			if len(positionals) != 3 {
				return errors.New("dep add/remove requires two IDs")
			}
			if operation == "add" {
				if flags.Changed("type") {
					*depType = *kind
				}
				if _, err := store.Get(positionals[2]); err != nil {
					if !errors.Is(err, beads.ErrNotFound) {
						return err
					}
					if _, err := resolveSQLiteWorkDependency(context.Background(), cityPath, target.ScopeRoot, positionals[2]); err != nil {
						return err
					}
				}
				return emitter.DepAdd(id, positionals[2], *depType)
			}
			return emitter.DepRemove(id, positionals[2])
		default:
			return fmt.Errorf("unsupported dependency operation %q", operation)
		}
	case "update", "close", "reopen":
		id, err := oneID()
		if err != nil {
			return err
		}
		if *claim {
			changed := 0
			flags.Visit(func(f *pflag.Flag) {
				if f.Name != "json" && f.Name != "actor" && f.Name != "directory" && f.Name != "quiet" && f.Name != "claim" {
					changed++
				}
			})
			if changed > 0 {
				return errors.New("--claim cannot be combined with field edits")
			}
			b, won, err := emitter.Claim(id, *actor)
			if err != nil {
				return err
			}
			if !won {
				return fmt.Errorf("claim of %s lost or bead is not claimable", id)
			}
			return printRows([]beads.Bead{b})
		}
		opts := beads.UpdateOpts{Metadata: patch, UnsetMetadata: *unsetMetadata}
		if flags.Changed("title") {
			opts.Title = title
		}
		if flags.Changed("description") {
			opts.Description = description
		}
		if flags.Changed("type") {
			opts.Type = kind
		}
		if flags.Changed("priority") {
			opts.Priority = priority
		}
		if flags.Changed("parent") {
			opts.ParentID = parent
		}
		if flags.Changed("assignee") {
			opts.Assignee = assignee
		}
		if flags.Changed("status") {
			opts.Status = status
		}
		opts.Labels = append(*labels, (*addLabels)...)
		opts.RemoveLabels = *removeLabels
		if verb == "close" {
			value := "closed"
			opts.Status = &value
		}
		if verb == "reopen" {
			value := "open"
			opts.Status = &value
		}
		if (*appendNotes != "" || flags.Changed("if-assignee") || flags.Changed("if-status") || flags.Changed("if-revision")) && len(opts.UnsetMetadata) > 0 {
			return errors.New("conditional updates and --append-notes cannot be combined with --unset-metadata")
		}
		for attempt := 0; attempt < 8; attempt++ {
			b, err := store.Get(id)
			if err != nil {
				return err
			}
			if flags.Changed("if-revision") && b.Revision != *matchRevision {
				return fmt.Errorf("revision precondition failed for %s: expected %d, found %d", id, *matchRevision, b.Revision)
			}
			if flags.Changed("if-assignee") && b.Assignee != *matchAssignee || flags.Changed("if-status") && b.Status != *matchStatus {
				return printRows([]beads.Bead{b})
			}
			if opts.Status != nil && *opts.Status == "closed" && runWorkRecordCloseGate(args, target.ScopeRoot, cityPath, cfg, store, map[string]beads.Bead{id: b}, stderr) {
				return errors.New("work-record close gate refused the close")
			}
			if *appendNotes != "" {
				value := strings.TrimSuffix(b.Metadata[sqliteWorkNotesKey], "\n")
				if value != "" {
					value += "\n"
				}
				opts.Metadata[sqliteWorkNotesKey] = value + *appendNotes
			}
			if len(opts.UnsetMetadata) > 0 {
				err = emitter.Update(id, opts)
			} else {
				err = emitter.UpdateIfMatch(id, b.Revision, opts)
			}
			var conflict *beads.PreconditionFailedError
			if errors.As(err, &conflict) {
				if flags.Changed("if-revision") {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			b, err = store.Get(id)
			if err != nil {
				return err
			}
			return printRows([]beads.Bead{b})
		}
		return errors.New("concurrent work updates exhausted retry budget")
	}
	return fmt.Errorf("unsupported command %q", verb)
}

func updateSQLiteWorkText(store *beads.SQLiteWorkStore, id string, edit func(beads.Bead) (map[string]string, error)) error {
	for attempt := 0; attempt < 8; attempt++ {
		b, err := store.Get(id)
		if err != nil {
			return err
		}
		patch, err := edit(b)
		if err != nil {
			return err
		}
		err = store.UpdateIfMatch(id, b.Revision, beads.UpdateOpts{Metadata: patch})
		var conflict *beads.PreconditionFailedError
		if errors.As(err, &conflict) {
			continue
		}
		return err
	}
	return errors.New("concurrent work text updates exhausted retry budget")
}
