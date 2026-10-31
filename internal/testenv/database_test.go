package testenv

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type databaseFixtureCloser func() error

func (f databaseFixtureCloser) Close() error { return f() }

func TestDatabaseTemplateCopiesOpenInDeepScratch(t *testing.T) {
	root, err := filepath.EvalSymlinks(TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for len(root) < 440 {
		root = filepath.Join(root, strings.Repeat("d", max(1, min(60, 439-len(root)))))
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP", "GOTMPDIR"} {
		t.Setenv(key, root)
	}
	pathForTest := DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, err
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE fixture (id INTEGER PRIMARY KEY)"); err != nil {
			return nil, errors.Join(err, db.Close())
		}
		return db, nil
	})
	for _, name := range []string{"first", "second"} {
		t.Run(strings.Repeat(name, 20), func(t *testing.T) {
			db, err := sql.Open("sqlite", pathForTest(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := db.ExecContext(t.Context(), "INSERT INTO fixture (id) VALUES (1)"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDatabaseTemplateKeepsClosedImageAndPrivateCopies(t *testing.T) {
	t.Parallel()
	var opens atomic.Int64
	pathForTest := DatabaseTemplate(func(_ context.Context, path string) (io.Closer, error) {
		opens.Add(1)
		return databaseFixtureCloser(func() error {
			// The committed image becomes readable only when the opener closes.
			return os.WriteFile(path, []byte("closed image"), 0o600)
		}), nil
	})
	first, second := pathForTest(t), pathForTest(t)
	if first == second || opens.Load() != 1 {
		t.Fatalf("copies=%q,%q opens=%d", first, second, opens.Load())
	}
	if err := os.WriteFile(first, []byte("first test writes"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || string(data) != "closed image" {
		t.Fatalf("second copy=%q error=%v", data, err)
	}
	third := pathForTest(t)
	data, err = os.ReadFile(third)
	if err != nil || string(data) != "closed image" || opens.Load() != 1 {
		t.Fatalf("template mutated: third=%q opens=%d error=%v", data, opens.Load(), err)
	}
	info, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("copy permissions=%v", info.Mode().Perm())
	}
}
