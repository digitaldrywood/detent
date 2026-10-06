package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestConfigMigrateCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, filename, raw string
		fails               bool
	}{
		{"yaml", "detent.yaml", "schema: 1\ntracker:\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n", false},
		{"frontmatter", "WORKFLOW.md", "---\ntracker:\n  active_states: [Todo]\n---\nKeep this prompt.\n", false},
		{"invalid legacy", "detent.yaml", "schema: 1\ntracker:\n  active_states: [Todo, '']\n", true},
		{"mixed", "detent.yaml", "schema: 1\ntracker:\n  lanes: []\n  active_states: []\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.filename)
			if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := NewRootCommand(context.Background())
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"config", "migrate", path})
			err := cmd.Execute()
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
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatal("migration changed file permissions")
			}
		})
	}
}

func TestDoctorWorkflowLanes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, raw string
		warning   bool
	}{
		{"legacy", "active_states: [Todo]", true},
		{"ordered", "lanes: [{name: Todo, role: active}]", false},
		{"default", "kind: memory", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  " + test.raw + "\n---\nWork.\n"))
			if err != nil {
				t.Fatal(err)
			}
			check, found := checkDoctorWorkflowLanes("example", workflow.Config)
			if found != test.warning {
				t.Fatalf("warning=%t, want %t", found, test.warning)
			}
			if found && (check.Status != doctorWarn || !strings.Contains(check.Detail, "must be migrated") || !strings.Contains(check.Hint, "detent config migrate")) {
				t.Fatalf("check=%#v", check)
			}
		})
	}
}
