package store

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSQLiteCreatesOnlyDatabaseDirectory(t *testing.T) {
	for _, test := range []struct {
		name string
		dsn  func(string) string
	}{
		{name: "plain path", dsn: plainPath},
		{name: "plain path with query", dsn: func(path string) string { return path + "?_pragma=foreign_keys(1)" }},
		{name: "uri", dsn: func(path string) string { return "file:" + path + "?mode=rwc" }},
		{name: "localhost uri", dsn: func(path string) string { return "file://localhost" + path }},
		{name: "escaped uri", dsn: func(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			workingDir := t.TempDir()
			t.Chdir(workingDir)
			path := filepath.Join(t.TempDir(), "database directory", "detent.db")
			opened, openErr := Open(t.Context(), Config{Path: test.dsn(path)})
			if openErr == nil {
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(workingDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("Open() created files in the working directory: %v", entries)
			}
			if openErr != nil {
				t.Fatalf("Open() error = %v", openErr)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database at intended path: %v", err)
			}
		})
	}
}
