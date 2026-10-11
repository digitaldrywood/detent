package storetest

import (
	"context"
	"io"
	"testing"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
)

var databasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: path})
})

func Open(t testing.TB) store.Store {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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
	return databasePath(t)
}
