package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorNativeWorkflowInstructions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		kind   string
		prompt string
		status doctorStatus
		want   []string
	}{
		{name: "native clean", kind: workflowconfig.TrackerHubNative, prompt: "Commit the change and run make check.", status: doctorOK},
		{name: "native workpad", kind: workflowconfig.TrackerHubNative, prompt: "Keep the `## Codex Workpad` comment current.", status: doctorWarn, want: []string{"Codex Workpad"}},
		{name: "native pull request", kind: workflowconfig.TrackerHubNative, prompt: "Open a Pull Request with gh pr create.", status: doctorWarn, want: []string{"gh", "pull request"}},
		{name: "native github api", kind: workflowconfig.TrackerHubNative, prompt: "Query the GitHub API for checks.", status: doctorWarn, want: []string{"GitHub API"}},
		{name: "native gh without space", kind: workflowconfig.TrackerHubNative, prompt: "Read the ghost file with high effort through the night.", status: doctorOK},
		{name: "native gh api", kind: workflowconfig.TrackerHubNative, prompt: "Run `gh api repos/acme/orders`.", status: doctorWarn, want: []string{"gh"}},
		{name: "native status marker", kind: workflowconfig.TrackerHubNative, prompt: "Post a detent-status block.", status: doctorWarn, want: []string{"detent-status"}},
		{name: "github tracker", kind: workflowconfig.TrackerGitHub, prompt: "Keep the Codex Workpad and open a pull request.", status: doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := checkDoctorNativeWorkflowInstructions("orders", workflowconfig.Config{Tracker: workflowconfig.Tracker{Kind: tt.kind}}, tt.prompt)
			if check.Status != tt.status {
				t.Fatalf("status = %v, want %v (%s)", check.Status, tt.status, check.Detail)
			}
			if check.Name != "Project orders native workflow instructions" {
				t.Fatalf("name = %q", check.Name)
			}
			for _, want := range tt.want {
				if !strings.Contains(check.Detail, want) {
					t.Errorf("detail %q missing %q", check.Detail, want)
				}
			}
		})
	}
}

func TestHubPolicyInspectWarnsOnNativeGitHubSteps(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		kind        string
		want        bool
		workflow    string
		wantLanding bool
		wantPark    bool
	}{
		{name: "native", kind: workflowconfig.TrackerHubNative, want: true},
		{name: "memory", kind: workflowconfig.TrackerMemory},
		{name: "four lane workflow has both completion paths", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  active_states: [Todo, In Progress]\n  observed_states: [Human Review]\n  terminal_states: [Done]\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Human Review, Done]\n      In Progress: [Todo, Human Review, Done]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: false, wantPark: false},
		{name: "missing terminal transitions warn before approval", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  active_states: [Todo, In Progress]\n  observed_states: [Human Review]\n  terminal_states: [Done]\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Human Review]\n      In Progress: [Todo, Human Review]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: true, wantPark: false},
		{name: "missing park transitions warn before approval", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  active_states: [Todo, In Progress]\n  observed_states: [Human Review]\n  terminal_states: [Done]\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: false, wantPark: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			definition := tt.workflow
			if definition == "" {
				definition = "---\ntracker:\n  kind: " + tt.kind + "\n---\nKeep the Codex Workpad current and open a pull request.\n"
			}
			if err := os.WriteFile(workflowPath, []byte(definition), 0o600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "global.yaml")
			cfg, err := globalconfig.DefaultAt(configPath, globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: root, Weight: 1}}
			if err := globalconfig.Write(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			cmd := newHubPolicyCommand(func(string) string { return "" })
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"inspect", "--config", configPath, "--project", "orders"})
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(stderr.String(), "GitHub-only steps"); got != tt.want {
				t.Fatalf("warning present = %t, want %t: %s", got, tt.want, stderr.String())
			}
			for _, check := range []struct {
				message string
				want    bool
			}{{"no landing path", tt.wantLanding}, {"no park lane", tt.wantPark}} {
				if tt.workflow != "" && strings.Contains(stderr.String(), check.message) != check.want {
					t.Fatalf("%s warning mismatch: %s", check.message, stderr.String())
				}
				if strings.Contains(stdout.String(), check.message) {
					t.Fatal("completion warning leaked into descriptor")
				}
			}
			if strings.Contains(stdout.String(), "GitHub-only steps") {
				t.Fatal("warning leaked into descriptor output")
			}
		})
	}
}
