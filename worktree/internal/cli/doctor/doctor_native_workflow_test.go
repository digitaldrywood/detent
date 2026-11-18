package doctor

import (
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestCheckDoctorNativeWorkflowInstructions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		kind   string
		prompt string
		status Status
		want   []string
	}{
		{name: "native clean", kind: workflowconfig.TrackerHubNative, prompt: "Commit the change and run make check.", status: OK},
		{name: "native workpad", kind: workflowconfig.TrackerHubNative, prompt: "Keep the `## Codex Workpad` comment current.", status: Warn, want: []string{"Codex Workpad"}},
		{name: "native pull request", kind: workflowconfig.TrackerHubNative, prompt: "Open a Pull Request with gh pr create.", status: Warn, want: []string{"gh", "pull request"}},
		{name: "native github api", kind: workflowconfig.TrackerHubNative, prompt: "Query the GitHub API for checks.", status: Warn, want: []string{"GitHub API"}},
		{name: "native gh without space", kind: workflowconfig.TrackerHubNative, prompt: "Read the ghost file with high effort through the night.", status: OK},
		{name: "native gh api", kind: workflowconfig.TrackerHubNative, prompt: "Run `gh api repos/acme/orders`.", status: Warn, want: []string{"gh"}},
		{name: "native status marker", kind: workflowconfig.TrackerHubNative, prompt: "Post a detent-status block.", status: Warn, want: []string{"detent-status"}},
		{name: "github tracker", kind: workflowconfig.TrackerGitHub, prompt: "Keep the Codex Workpad and open a pull request.", status: OK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := CheckNativeWorkflowInstructions("orders", workflowconfig.Config{Tracker: workflowconfig.Tracker{Kind: tt.kind}}, tt.prompt)
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
