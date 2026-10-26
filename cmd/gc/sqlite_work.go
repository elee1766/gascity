package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

const sqliteWorkProvider = "sqlite-work"

type sqliteWorkIdentity struct {
	Provider string `json:"provider"`
	Prefix   string `json:"prefix"`
}

func sqliteWorkDir(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".gc", "store", "work")
}

// openSQLiteWorkScope never creates a missing ledger during an ordinary read.
// Explicit initialization owns creation and records its minting namespace.
func openSQLiteWorkScope(scopeRoot, cityPath string, initialize bool) (*beads.SQLiteWorkStore, error) {
	prefix, err := sqliteWorkScopePrefix(scopeRoot, cityPath)
	if err != nil {
		return nil, err
	}
	return openSQLiteWorkScopeWithPrefix(scopeRoot, prefix, initialize,
		beads.WithSQLiteDependencyResolver(func(ctx context.Context, id string) (beads.Bead, error) {
			return resolveSQLiteWorkDependency(ctx, cityPath, scopeRoot, id)
		}))
}

type sqliteDependencyScope struct{ root, prefix string }

func sqliteDependencyScopes(cityPath string, cfg *config.City) []sqliteDependencyScope {
	scopes := []sqliteDependencyScope{{cityPath, config.EffectiveHQPrefix(cfg)}}
	for _, rig := range cfg.Rigs {
		scopes = append(scopes, sqliteDependencyScope{resolveStoreScopeRoot(cityPath, rig.Path), rig.EffectivePrefix()})
	}
	return scopes
}

// sqliteWorkDependencyRows returns the bead-shaped contract consumed by cascade nudges.
func sqliteWorkDependencyRows(cityPath, sourceRoot string, store *beads.SQLiteWorkStore, id, direction, depType string) ([]bdByIDDepRow, error) {
	if direction != "up" && direction != "down" {
		return nil, fmt.Errorf("invalid dependency direction %q", direction)
	}
	result := []bdByIDDepRow{}
	appendRows := func(scope *beads.SQLiteWorkStore) error {
		edges, err := scope.DepList(id, direction)
		if err != nil {
			return err
		}
		for _, edge := range edges {
			if depType != "" && edge.Type != depType {
				continue
			}
			related := edge.DependsOnID
			if direction == "up" {
				related = edge.IssueID
			}
			row, err := scope.Get(related)
			if errors.Is(err, beads.ErrNotFound) && direction == "down" {
				row, err = resolveSQLiteWorkDependency(context.Background(), cityPath, sourceRoot, related)
			}
			if err != nil {
				return fmt.Errorf("reading dependency %s: %w", related, err)
			}
			result = append(result, bdByIDDepRow{Bead: row, DepType: edge.Type})
		}
		return nil
	}
	if err := appendRows(store); err != nil {
		return nil, err
	}
	if direction == "up" {
		cfg, err := loadCityConfigForBdPassthrough(cityPath, io.Discard)
		if err != nil {
			return nil, err
		}
		for _, candidate := range sqliteDependencyScopes(cityPath, cfg) {
			if samePath(candidate.root, sourceRoot) || rawBeadsProviderForScope(candidate.root, cityPath) != sqliteWorkProvider {
				continue
			}
			foreign, err := openSQLiteWorkScopeWithPrefix(candidate.root, candidate.prefix, false, beads.WithSQLiteStoreReadOnly())
			if err != nil {
				return nil, err
			}
			err = appendRows(foreign)
			closeErr := foreign.CloseStore()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ID == result[j].ID {
			return result[i].DepType < result[j].DepType
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func resolveSQLiteWorkDependency(ctx context.Context, cityPath, sourceRoot, id string) (beads.Bead, error) {
	if err := ctx.Err(); err != nil {
		return beads.Bead{}, err
	}
	cfg, err := loadCityConfigForBdPassthrough(cityPath, io.Discard)
	if err != nil {
		return beads.Bead{}, err
	}
	scopes := sqliteDependencyScopes(cityPath, cfg)
	var target *sqliteDependencyScope
	for i := range scopes {
		candidate := &scopes[i]
		if !strings.HasPrefix(id, candidate.prefix+"-") {
			continue
		}
		if target != nil {
			return beads.Bead{}, fmt.Errorf("ambiguous dependency namespace %q", id)
		}
		target = candidate
	}
	if target == nil || samePath(target.root, sourceRoot) {
		return beads.Bead{}, beads.ErrNotFound
	}
	if rawBeadsProviderForScope(target.root, cityPath) != sqliteWorkProvider {
		return beads.Bead{}, fmt.Errorf("dependency %s is not in a sqlite-work scope", id)
	}
	store, err := openSQLiteWorkScopeWithPrefix(target.root, target.prefix, false, beads.WithSQLiteStoreReadOnly())
	if err != nil {
		return beads.Bead{}, err
	}
	defer store.CloseStore() //nolint:errcheck
	if err := ctx.Err(); err != nil {
		return beads.Bead{}, err
	}
	return store.Get(id)
}

// openSQLiteWorkScopeWithPrefix also serves rig provisioning, whose validated
// candidate prefix is available before the topology is committed to disk.
func openSQLiteWorkScopeWithPrefix(scopeRoot, prefix string, initialize bool, extra ...beads.SQLiteStoreOption) (*beads.SQLiteWorkStore, error) {
	if prefix == "" {
		return nil, fmt.Errorf("sqlite-work: cannot resolve prefix for %s", scopeRoot)
	}
	dir := sqliteWorkDir(scopeRoot)
	identityPath := filepath.Join(dir, "identity.json")
	data, err := os.ReadFile(identityPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("sqlite-work identity: %w", err)
	}
	fresh := os.IsNotExist(err)
	if fresh {
		if !initialize {
			return nil, fmt.Errorf("sqlite-work: %s is not initialized; initialize this scope explicitly", dir)
		}
		for _, path := range []string{
			filepath.Join(scopeRoot, ".beads", "metadata.json"),
			filepath.Join(scopeRoot, ".gc", "beads.json"),
			filepath.Join(dir, "beads.sqlite"),
		} {
			if _, err := os.Stat(path); err == nil {
				return nil, fmt.Errorf("sqlite-work: refusing implicit migration or replacement of %s", path)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
	} else {
		var identity sqliteWorkIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			return nil, fmt.Errorf("sqlite-work identity: %w", err)
		}
		if identity.Provider != sqliteWorkProvider || identity.Prefix != prefix {
			return nil, fmt.Errorf("sqlite-work: identity mismatch at %s (provider %q, prefix %q, configured %q)", identityPath, identity.Provider, identity.Prefix, prefix)
		}
		if _, err := os.Stat(filepath.Join(dir, "beads.sqlite")); err != nil {
			return nil, fmt.Errorf("sqlite-work: existing ledger is missing or inaccessible: %w", err)
		}
	}
	opts := append([]beads.SQLiteStoreOption{
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(prefix),
		beads.WithSQLiteStoreRetention(0, 0)}, extra...)
	store, err := beads.OpenSQLiteWorkStore(dir, opts...)
	if err != nil {
		return nil, err
	}
	if fresh {
		data, err := json.Marshal(sqliteWorkIdentity{Provider: sqliteWorkProvider, Prefix: prefix})
		if err == nil {
			err = fsys.WriteFileAtomic(fsys.OSFS{}, identityPath, append(data, '\n'), 0o600)
		}
		if err != nil {
			_ = store.CloseStore()
			return nil, fmt.Errorf("recording sqlite-work identity: %w", err)
		}
	}
	return store, nil
}

func sqliteWorkScopePrefix(scopeRoot, cityPath string) (string, error) {
	cfg, err := loadCityConfigForBdPassthrough(cityPath, io.Discard)
	if err != nil {
		return "", err
	}
	for _, rig := range cfg.Rigs {
		if samePath(resolveStoreScopeRoot(cityPath, rig.Path), scopeRoot) {
			return rig.EffectivePrefix(), nil
		}
	}
	if samePath(resolveStoreScopeRoot(cityPath, cityPath), scopeRoot) {
		return config.EffectiveHQPrefix(cfg), nil
	}
	return "", fmt.Errorf("sqlite-work: unconfigured scope %s", scopeRoot)
}

func initSQLiteWorkScope(scopeRoot, cityPath string) error {
	store, err := openSQLiteWorkScope(scopeRoot, cityPath, true)
	if err != nil {
		return err
	}
	return store.CloseStore()
}
