package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestPruneCodexLogs(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name               string
		alive              bool
		scanErr            error
		limit              int64
		removed            bool
		rows               int
		shared             bool
		startsDuringVacuum bool
	}{
		{name: "retains cutoff and newer", limit: codexLogSizeLimit, rows: 2},
		{name: "exact floor retained", limit: 8192, rows: 2},
		{name: "live server preserves all", alive: true, limit: 1, rows: 3},
		{name: "server starts during vacuum", startsDuringVacuum: true, limit: 1, rows: 2},
		{name: "unknown process state preserves all", scanErr: errors.New("ps failed"), limit: 1, rows: 3},
		{name: "above floor removed after vacuum", limit: 1, removed: true},
		{name: "hard linked user database preserved", limit: 1, rows: 3, shared: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, workerCodexProfileDir)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "logs_2.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.ExecContext(t.Context(), "CREATE TABLE logs (ts INTEGER NOT NULL); INSERT INTO logs VALUES (?), (?), (?)", now.Add(-8*24*time.Hour).Unix(), now.Add(-7*24*time.Hour).Unix(), now.Unix()); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(home, "logs_2.sqlite")
			if tt.shared {
				if err := os.Link(path, userPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(userPath, []byte("user database untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(userPath)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			err = pruneCodexLogs(t.Context(), home, now, tt.limit, func(context.Context) (bool, error) {
				calls++
				return tt.alive || (tt.startsDuringVacuum && calls > 1), tt.scanErr
			})
			if !errors.Is(err, tt.scanErr) {
				t.Fatalf("error=%v want %v", err, tt.scanErr)
			}
			after, err := os.ReadFile(userPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("modified user database")
			}
			if tt.removed {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("database remains: %v", err)
				}
				return
			}
			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var rows int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM logs").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != tt.rows {
				t.Fatalf("rows=%d want %d", rows, tt.rows)
			}
		})
	}
}

type retentionFixture struct {
	protected map[string]bool
	err       error
}

func (f retentionFixture) ProtectedCodexSessions(context.Context, time.Time) (map[string]bool, error) {
	result := map[string]bool{}
	for id, value := range f.protected {
		result[id] = value
	}
	return result, f.err
}

func writeRollout(t *testing.T, home, name, id, origin string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "08")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".jsonl")
	content := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"originator":%q}}`+"\n", id, origin)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneCodexRollouts(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, origin                            string
		age                                     time.Duration
		protected, recentSibling, removed, fail bool
	}{
		{name: "old Detent", origin: "detent-orchestrator", age: 31 * 24 * time.Hour, removed: true},
		{name: "other origin", origin: "codex_cli_rs", age: 31 * 24 * time.Hour},
		{name: "active reference", origin: "detent-orchestrator", age: 31 * 24 * time.Hour, protected: true},
		{name: "recent", origin: "detent-orchestrator", age: 24 * time.Hour},
		{name: "exact cutoff", origin: "detent-orchestrator", age: 30 * 24 * time.Hour},
		{name: "young sibling", origin: "detent-orchestrator", age: 31 * 24 * time.Hour, recentSibling: true},
		{name: "store failure", origin: "detent-orchestrator", age: 31 * 24 * time.Hour, fail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeRollout(t, home, "old", "session-1", tt.origin, now.Add(-tt.age))
			if tt.recentSibling {
				writeRollout(t, home, "young", "session-1", tt.origin, now)
			}
			fixture := retentionFixture{protected: map[string]bool{"session-1": tt.protected}}
			if tt.fail {
				fixture.err = errors.New("store unavailable")
			}
			bytes, err := pruneCodexRollouts(t.Context(), home, now, fixture)
			if !errors.Is(err, fixture.err) {
				t.Fatal(err)
			}
			_, statErr := os.Stat(path)
			if errors.Is(statErr, os.ErrNotExist) != tt.removed {
				t.Fatalf("removed=%v want %v", statErr, tt.removed)
			}
			if (bytes > 0) != tt.removed {
				t.Fatalf("removed bytes=%d", bytes)
			}
		})
	}
}

func TestDoctorCodexStorage(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int64
		warn bool
	}{{"small", 4096, false}, {"threshold", 2 << 30, false}, {"large", (2 << 30) + 1, true}} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, workerCodexProfileDir)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(dir, "state_5.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Truncate(tt.size); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			checks := checkDoctorCodexStorage(t.Context(), "test", workflowconfig.Default(), func(key string) string {
				if key == "CODEX_HOME" {
					return home
				}
				return ""
			})
			if len(checks) != 1 {
				t.Fatalf("checks=%+v", checks)
			}
			if (checks[0].Status == doctorWarn) != tt.warn || !strings.Contains(checks[0].Detail, "state_5.sqlite") {
				t.Fatalf("check=%+v", checks[0])
			}
		})
	}
}

func TestCodexRetentionPreservesUnownedFiles(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name, content string
		symlink       bool
	}{
		{name: "malformed", content: "not json\n"},
		{name: "missing identity", content: `{"type":"session_meta","payload":{"originator":"detent-orchestrator"}}`},
		{name: "recent metadata", content: fmt.Sprintf(`{"type":"session_meta","payload":{"id":"thread","originator":"detent-orchestrator","timestamp":%q}}`, now.Format(time.RFC3339Nano))},
		{name: "unknown metadata age", content: `{"type":"session_meta","payload":{"id":"thread","originator":"detent-orchestrator","timestamp":"invalid"}}`},
		{name: "symlink", symlink: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeRollout(t, home, "rollout", "thread", "detent-orchestrator", now.Add(-40*24*time.Hour))
			if tt.symlink {
				target := filepath.Join(home, "target.jsonl")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
					t.Fatal(err)
				}
				old := now.Add(-40 * 24 * time.Hour)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := pruneCodexRollouts(t.Context(), home, now, retentionFixture{})
			if err != nil || removed != 0 {
				t.Fatalf("removed=%d err=%v", removed, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPruneCodexLogsProfileBoundaries(t *testing.T) {
	for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
		t.Run(profile, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, profile)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(home, "logs_2.sqlite")
			if err := os.WriteFile(userPath, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(userPath, filepath.Join(dir, "logs_2.sqlite")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			state := filepath.Join(dir, "state_5.sqlite")
			if err := os.WriteFile(state, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := pruneCodexLogs(t.Context(), home, time.Now(), 1, func(context.Context) (bool, error) { return false, nil }); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{userPath, state} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "preserve" {
					t.Fatalf("%s changed: %q %v", path, data, err)
				}
			}
		})
	}
}

func TestCodexAppServerInProcessList(t *testing.T) {
	for _, tt := range []struct {
		name, output string
		alive        bool
	}{
		{name: "empty"},
		{name: "worker", output: "/opt/bin/codex /opt/bin/codex app-server --listen stdio://", alive: true},
		{name: "wrapper", output: "sh sh -c codex app-server", alive: true},
		{name: "spaces", output: "/Applications/My Codex.app/bin/codex /Applications/My Codex.app/bin/codex app-server", alive: true},
		{name: "different subcommand", output: "codex codex exec task"},
		{name: "argument substring", output: "codex codex exec app-server-example"},
		{name: "other process", output: "server server app-server"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexAppServerInProcessList(tt.output); got != tt.alive {
				t.Fatalf("alive=%v want %v", got, tt.alive)
			}
		})
	}
}

func TestPruneCodexRolloutDescendants(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	for _, tt := range []struct {
		name         string
		protected    map[string]bool
		recentParent bool
		wantRemoved  bool
	}{
		{name: "active parent", protected: map[string]bool{"parent": true}},
		{name: "recent parent", recentParent: true},
		{name: "parent absent from tree", protected: map[string]bool{"missing": true}},
		{name: "unprotected family", wantRemoved: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			parent := "parent"
			if tt.name == "parent absent from tree" {
				parent = "missing"
			} else {
				mtime := old
				if tt.recentParent {
					mtime = now
				}
				writeRollout(t, home, "z-parent", parent, "detent-orchestrator", mtime)
			}
			var paths []string
			// Grandchild sorts before child and parent to exercise order independence.
			for _, child := range []struct{ name, id, parent string }{
				{"a-grandchild", "grandchild", "child"},
				{"b-child", "child", parent},
			} {
				path := writeRollout(t, home, child.name, child.id, "detent-orchestrator", old)
				content := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"parent_thread_id":%q,"originator":"detent-orchestrator"}}`, child.id, child.parent)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, path)
			}
			if _, err := pruneCodexRollouts(t.Context(), home, now, retentionFixture{protected: tt.protected}); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				_, err := os.Stat(path)
				if errors.Is(err, os.ErrNotExist) != tt.wantRemoved {
					t.Fatalf("%s: stat=%v, want removed=%v", path, err, tt.wantRemoved)
				}
			}
		})
	}
}
