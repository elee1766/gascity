package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	store, err := beads.OpenSQLiteWorkStore(dir,
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(prefix),
		beads.WithSQLiteStoreRetention(0, 0))
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
