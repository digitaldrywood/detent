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

func TestDetentRolloutsReportingPreservesFiles(t *testing.T) {
	for _, tt := range []struct {
		name, origin string
		age          time.Duration
		want         int
	}{
		{name: "old Detent from any instance", origin: "detent-orchestrator", age: 40 * 24 * time.Hour, want: 1},
		{name: "recent Detent", origin: "detent-orchestrator", want: 1},
		{name: "non Detent", origin: "codex_cli_rs", age: 40 * 24 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeRollout(t, home, "rollout", "thread", tt.origin, time.Now().Add(-tt.age))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rollouts, err := detentRollouts(t.Context(), home)
			if err != nil {
				t.Fatal(err)
			}
			if len(rollouts) != tt.want {
				t.Fatalf("count=%d want %d", len(rollouts), tt.want)
			}
			if tt.want > 0 && rollouts[0].size != int64(len(before)) {
				t.Fatalf("size=%d want %d", rollouts[0].size, len(before))
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("report changed rollout: %v", err)
			}
		})
	}
}
