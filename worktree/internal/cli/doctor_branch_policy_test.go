package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorRespectsProjectCIPolicy(t *testing.T) {
	t.Parallel()
	for _, event := range []string{"pull_request", "merge_group", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			workflows := filepath.Join(root, ".github", "workflows")
			if err := os.MkdirAll(workflows, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workflows, "ci.yml"), []byte("on:\n  "+event+":\njobs:\n  verify:\n    runs-on: ubuntu-latest\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = "github"
			cfg.Tracker.Repository = "owner/repo"
			cfg.Workspace.SourceRoot = root
			cfg.Deliverable.Kind = workflowconfig.DeliverablePullRequest
			deps := successfulDoctorDeps()
			deps.githubBranchPolicy = func(context.Context, workflowconfig.Config, string) (ghconnector.BranchMergePolicy, error) {
				return ghconnector.BranchMergePolicy{Branch: "main", Strict: true}, nil
			}
			checks := checkDoctorBranchPolicy(t.Context(), "alpha", globalconfig.Project{ID: "alpha"}, cfg, deps)
			foundPolicy := false
			for _, check := range checks {
				if strings.Contains(check.Name, " INV-5 ") || strings.Contains(check.Name, " INV-8 ") || strings.Contains(check.Hint, "zero PR CI") || strings.Contains(check.Hint, "Disable strict") {
					t.Errorf("repository development policy leaked into project diagnostics: %#v", check)
				}
				if check.Name == "Project alpha branch policy" {
					foundPolicy = true
					if check.Status != doctorOK || !strings.Contains(check.Detail, "requires up-to-date branches: true") || check.Hint != "" {
						t.Errorf("strict branch policy must remain supported: %#v", check)
					}
				}
			}
			if !foundPolicy {
				t.Fatal("missing branch policy evidence")
			}
		})
	}
}
