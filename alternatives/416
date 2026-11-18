package cli

import (
	"bytes"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

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
		{name: "four lane workflow has both completion paths", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  lanes:\n    - name: Human Review\n      role: holding\n    - name: Todo\n      role: active\n    - name: In Progress\n      role: active\n    - name: Done\n      role: terminal\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Human Review, Done]\n      In Progress: [Todo, Human Review, Done]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: false, wantPark: false},
		{name: "missing terminal transitions warn before approval", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  lanes:\n    - name: Human Review\n      role: holding\n    - name: Todo\n      role: active\n    - name: In Progress\n      role: active\n    - name: Done\n      role: terminal\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Human Review]\n      In Progress: [Todo, Human Review]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: true, wantPark: false},
		{name: "missing park transitions warn before approval", kind: workflowconfig.TrackerHubNative, workflow: "---\ntracker:\n  kind: hub_native\n  lanes:\n    - name: Human Review\n      role: holding\n    - name: Todo\n      role: active\n    - name: In Progress\n      role: active\n    - name: Done\n      role: terminal\nagent:\n  stop_run:\n    target_state: Human Review\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Human Review: [In Progress, Done]\n      Done: [Todo]\n---\nComplete the issue.\n", wantLanding: false, wantPark: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition := tt.workflow
			if definition == "" {
				definition = "---\ntracker:\n  kind: " + tt.kind + "\n---\nKeep the Codex Workpad current and open a pull request.\n"
			}
			configPath := newEnrolledPolicyProject(t, definition).configPath
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
