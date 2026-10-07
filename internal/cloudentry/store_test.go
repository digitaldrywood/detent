package cloudentry

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestStoreEscapedPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, database := range []struct {
		name          string
		applicationID int64
		version       int
	}{
		{"registry", registryApplicationID, 20261006163000},
		{"auth", authApplicationID, 20261006222521},
	} {
		for _, directory := range []string{"plain", "cloud entry café 数据库 #100%"} {
			t.Run(database.name+"/"+directory, func(t *testing.T) {
				t.Parallel()
				// Native paths exercise Windows drive letters and backslashes there,
				// and POSIX paths on other hosts, without skipping either platform.
				path := filepath.Join(t.TempDir(), directory, database.name+" café #100%.db")
				s, err := openStore(t.Context(), path, database.applicationID, database.name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
				for _, pragma := range []struct{ name, want string }{
					{"busy_timeout", "5000"},
					{"foreign_keys", "1"},
					{"locking_mode", "normal"},
					{"synchronous", "2"},
					{"journal_mode", "wal"},
				} {
					var got string
					if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+pragma.name).Scan(&got); err != nil || got != pragma.want {
						t.Fatalf("%s = %q, %v; want %q", pragma.name, got, err, pragma.want)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				// Reopen the literal filename without a URI so a mis-escaped DSN
				// cannot hide a migrated database at a different path.
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
				var applicationID int64
				if err := db.QueryRowContext(t.Context(), "PRAGMA application_id").Scan(&applicationID); err != nil || applicationID != database.applicationID {
					t.Fatalf("application ID = %d, %v; want %d", applicationID, err, database.applicationID)
				}
				var version int
				if err := db.QueryRowContext(t.Context(), "SELECT max(version_id) FROM entry_schema_version").Scan(&version); err != nil || version != database.version {
					t.Fatalf("migration version = %d, %v; want %d", version, err, database.version)
				}
			})
		}
	}
}
