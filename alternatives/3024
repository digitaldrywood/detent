package store

import (
	"context"
	"testing"
)

func openHistoryTestStore(t *testing.T, path string) *sqliteStore {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	db, err := openSQLite(context.Background(), Config{Path: path})
	if err != nil {
		t.Fatalf("openSQLite() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
