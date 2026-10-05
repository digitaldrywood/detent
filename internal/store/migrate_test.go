package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func TestStoreTimestampMigrations(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name       string
		upgrade    bool
		outOfOrder bool
	}{
		{"fresh database", false, false},
		{"upgrade through 00068", true, false},
		{"older timestamp lands after newer timestamp", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			entries, err := migrationsFS.ReadDir("migrations")
			if err != nil {
				t.Fatal(err)
			}
			files := fstest.MapFS{}
			for _, entry := range entries {
				version, err := goose.NumericComponent(entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				if version > 68 {
					continue
				}
				name := "migrations/" + entry.Name()
				data, err := migrationsFS.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				files[name] = &fstest.MapFile{Data: data}
			}
			if test.upgrade {
				if err := runMigrationsFromFS(t.Context(), db, files); err != nil {
					t.Fatal(err)
				}
				if version := queryInt(t, db, "SELECT max(version_id) FROM goose_db_version WHERE is_applied = 1"); version != 68 {
					t.Fatalf("historical version=%d, want 68", version)
				}
			}
			files["migrations/20261005120001_second.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_second (id INTEGER);\nINSERT INTO timestamp_second VALUES (2);\n")}
			if test.outOfOrder {
				if err := runMigrationsFromFS(t.Context(), db, files); err != nil {
					t.Fatal(err)
				}
			}
			files["migrations/20261005120000_first.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE timestamp_first (id INTEGER);\nINSERT INTO timestamp_first VALUES (1);\n")}
			if err := runMigrationsFromFS(t.Context(), db, files); err != nil {
				t.Fatal(err)
			}
			if queryInt(t, db, "SELECT id FROM timestamp_first") != 1 || queryInt(t, db, "SELECT id FROM timestamp_second") != 2 {
				t.Fatal("timestamp migration data missing")
			}
			rows, err := db.QueryContext(t.Context(), "SELECT version_id FROM goose_db_version WHERE is_applied = 1 AND version_id > 0 ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var versions []int64
			for rows.Next() {
				var version int64
				if err := rows.Scan(&version); err != nil {
					t.Fatal(err)
				}
				versions = append(versions, version)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(versions) != len(files) {
				t.Fatalf("applied versions=%v, want %d entries", versions, len(files))
			}
			wantTail := []int64{68, 20261005120000, 20261005120001}
			if test.outOfOrder {
				wantTail = []int64{68, 20261005120001, 20261005120000}
			} else if !slices.IsSorted(versions) {
				t.Fatalf("migrations applied out of order: %v", versions)
			}
			if !slices.Equal(versions[len(versions)-3:], wantTail) {
				t.Fatalf("applied versions=%v, want tail %v", versions, wantTail)
			}
		})
	}
}
