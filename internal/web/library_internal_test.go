package web

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLibrarySQLiteReadOnlyExpandsHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dbPath := filepath.Join(home, "library", "work-items.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.ExecContext(context.Background(), "create table seed(id integer)"); err != nil {
		t.Fatalf("create seed table error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got, ok, warning, err := openLibrarySQLiteReadOnly(context.Background(), "~/library/work-items.db")
	if err != nil {
		t.Fatalf("openLibrarySQLiteReadOnly() error = %v", err)
	}
	if !ok {
		t.Fatalf("openLibrarySQLiteReadOnly() ok = false, warning = %q", warning)
	}
	if _, err := got.ExecContext(context.Background(), "create table should_fail(id integer)"); err == nil {
		t.Fatal("read-only library database accepted a write")
	}
	if err := got.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
