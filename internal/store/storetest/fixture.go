package storetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/store"
)

var migratedDatabase = sync.OnceValues(buildMigratedDatabase)

func Open(t testing.TB) store.Store {
	t.Helper()

	backend, err := store.Open(t.Context(), store.Config{
		Backend: store.BackendSQLite,
		Path:    NewDatabasePath(t),
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	return backend
}

func NewDatabasePath(t testing.TB) string {
	t.Helper()

	database, err := migratedDatabase()
	if err != nil {
		t.Fatalf("build migrated store fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "detent.db")
	if err := os.WriteFile(path, database, 0o600); err != nil {
		t.Fatalf("write migrated store fixture: %v", err)
	}
	return path
}

func buildMigratedDatabase() (_ []byte, returnErr error) {
	dir, err := os.MkdirTemp("", "detent-store-fixture-")
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, os.RemoveAll(dir))
	}()

	path := filepath.Join(dir, "detent.db")
	backend, err := store.Open(context.Background(), store.Config{
		Backend: store.BackendSQLite,
		Path:    path,
	})
	if err != nil {
		return nil, err
	}
	if err := backend.Close(); err != nil {
		return nil, err
	}
	database, readErr := os.ReadFile(path)
	return database, readErr
}
