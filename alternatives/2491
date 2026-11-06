package postgres_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func TestCoreCutoverCopyRejectsUnimportedArtifactData(t *testing.T) {
	for _, populated := range []string{"", "artifact_services", "artifact_grants", "artifact_references"} {
		t.Run("populated_"+populated, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, table := range []string{"artifact_services", "artifact_grants", "artifact_references"} {
				if _, err := db.ExecContext(t.Context(), "CREATE TABLE "+table+" (id TEXT)"); err != nil {
					t.Fatal(err)
				}
				if table == populated {
					if _, err := db.ExecContext(t.Context(), "INSERT INTO "+table+" VALUES ('example')"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0400); err != nil {
				t.Fatal(err)
			}
			err = postgres.VerifyCoreCutoverCopy(t.Context(), path)
			if (err != nil) != (populated != "") {
				t.Fatalf("cutover source verification error=%v", err)
			}
		})
	}
}
