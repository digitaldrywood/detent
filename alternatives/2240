package localboard

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalBoardReadBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, statement, source string
		wantError               bool
		wantCount               int
	}{
		{"terminal prerequisite is retained", `UPDATE detent_work_items SET state='Done' WHERE id='z-first'`, "default", false, 2},
		{"source project isolation", "", "other", false, 1},
		{"dependency cycle", `UPDATE detent_work_items SET description='Depends on: #20' WHERE id='z-first'`, "default", true, 0},
		{"missing prerequisite", `UPDATE detent_work_items SET description='Depends on: #999' WHERE id='a-second'`, "default", true, 0},
		{"cross-project prerequisite", `UPDATE detent_work_items SET description='Depends on: other/repository#10' WHERE id='a-second'`, "default", true, 0},
		{"invalid timestamp", `UPDATE detent_work_items SET created_at='title-secret' WHERE id='z-first'`, "default", true, 0},
		{"malformed comment event", `UPDATE detent_work_item_events SET payload_json='body-secret' WHERE event_kind='comment_edit'`, "default", true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, err := os.ReadFile(filepath.Join("testdata", "local_sqlite.sql"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "board.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), string(fixture)); err != nil {
				t.Fatal(err)
			}
			if test.statement != "" {
				if _, err := db.ExecContext(t.Context(), test.statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			specialPath := filepath.Join(filepath.Dir(path), "board ?#.sqlite")
			if err := os.Rename(path, specialPath); err != nil {
				t.Fatal(err)
			}
			path = specialPath
			records, err := Read(t.Context(), path, test.source, "local_sqlite", "example-board", []string{"Done"})
			if (err != nil) != test.wantError {
				t.Fatalf("records=%+v err=%v", records, err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "title-secret") || strings.Contains(err.Error(), "body-secret") {
					t.Fatal("error exposed content")
				}
				return
			}
			if len(records) != test.wantCount {
				t.Fatalf("records=%+v", records)
			}
			if test.source == "other" && records[0].Title != "Other project" {
				t.Fatal("wrong source project")
			}
			if test.name == "terminal prerequisite is retained" && (records[0].State != "Done" || len(records[1].Dependencies) != 1) {
				t.Fatal("lost terminal prerequisite")
			}
			copyPath := filepath.Join(t.TempDir(), "copy.sqlite")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(copyPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			copied, err := Read(t.Context(), copyPath, test.source, "local_sqlite", "example-board", []string{"Done"})
			if err != nil {
				t.Fatal(err)
			}
			if copied[0].Key != records[0].Key {
				t.Fatal("moving a copy changed import identity")
			}
		})
	}
}

func TestLocalBoardMissingFileStaysMissing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := Read(t.Context(), path, "default", "local_sqlite", "example-board", nil); err == nil {
		t.Fatal("missing source was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reader created missing source: %v", err)
	}
}
