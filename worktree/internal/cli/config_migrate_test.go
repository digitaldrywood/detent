package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigMigrateCommand(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("../config/testdata/local-era.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, filename, raw string
		symlinkTarget       string
		fails               bool
	}{
		{"retired", "detent.yaml", string(fixture), "", false},
		{"yaml", "detent.yaml", "schema: 1\ntracker:\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n", "", false},
		{"frontmatter", "WORKFLOW.md", "---\ntracker:\n  active_states: [Todo]\n---\nKeep this prompt.\n", "", false},
		{"invalid legacy", "detent.yaml", "schema: 1\ntracker:\n  active_states: [Todo, '']\n", "", true},
		{"mixed", "detent.yaml", "schema: 1\ntracker:\n  lanes: []\n  active_states: []\n", "", true},
		{"contained symlink", "detent.yaml", "schema: 1\ntracker:\n  active_states: [Todo]\n", "config/workflow.yaml", false},
		{"escaping symlink", "detent.yaml", "schema: 1\ntracker:\n  active_states: [Todo]\n", "../workflow.yaml", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "project", test.filename)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			target := path
			if test.symlinkTarget != "" {
				target = filepath.Join(filepath.Dir(path), test.symlinkTarget)
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(test.symlinkTarget, path); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			cmd := NewRootCommand(context.Background())
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--format", "pretty", "config", "migrate", path})
			err = cmd.Execute()
			if (err != nil) != test.fails {
				t.Fatalf("error = %v, output = %s", err, &output)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.fails {
				if string(raw) != test.raw {
					t.Fatal("failed migration changed file")
				}
			} else if !strings.Contains(string(raw), "lanes:") || strings.Contains(string(raw), "active_states:") {
				t.Fatalf("migration output = %s", raw)
			}
			if test.name == "retired" {
				for _, key := range []string{"intake", "routines", "retro", "backlog_admission", "tracker.github_status_source", "tracker.project_slug", "tracker.status_field", "tracker.status_label_prefix", "tracker.write_probe_issue", "server.host", "server.port", "server.board_snapshot_stale_after_seconds"} {
					if !strings.Contains(output.String(), "removed "+key+"\n") {
						t.Fatalf("missing removal %s: %s", key, &output)
					}
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != before.Mode().Perm() {
				t.Fatalf("migration changed file permissions from %v to %v", before.Mode().Perm(), info.Mode().Perm())
			}
		})
	}
}
