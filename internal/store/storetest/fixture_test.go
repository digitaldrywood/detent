package storetest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/digitaldrywood/detent/internal/store"
)

func TestOpenUsesIsolatedMigratedDatabase(t *testing.T) {
	t.Parallel()

	first := Open(t)
	evidence, err := first.RuntimeEvidence(context.Background(), store.RuntimeEvidenceQuery{})
	if err != nil {
		t.Fatalf("RuntimeEvidence() error = %v", err)
	}
	if evidence.MigrationVersion <= 0 {
		t.Fatalf("MigrationVersion = %d, want positive", evidence.MigrationVersion)
	}

	firstPath := NewDatabasePath(t)
	secondPath := NewDatabasePath(t)
	firstDB, err := sql.Open("sqlite", firstPath)
	if err != nil {
		t.Fatalf("sql.Open(first) error = %v", err)
	}
	t.Cleanup(func() { _ = firstDB.Close() })
	if _, err := firstDB.ExecContext(t.Context(), "CREATE TABLE fixture_probe (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create fixture probe: %v", err)
	}

	secondDB, err := sql.Open("sqlite", secondPath)
	if err != nil {
		t.Fatalf("sql.Open(second) error = %v", err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })
	var count int
	if err := secondDB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'fixture_probe'").Scan(&count); err != nil {
		t.Fatalf("query fixture probe: %v", err)
	}
	if count != 0 {
		t.Fatalf("fixture probe count = %d, want 0", count)
	}
}
