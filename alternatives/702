package cli

import (
	"context"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestScanIssueExposureReportsConfiguredPrivateSourceMatches(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Workflow{Config: workflowconfig.Default()}
	workflow.Config.Tracker.Kind = workflowconfig.TrackerGitHub
	workflow.Config.Tracker.Repository = "private/source"
	workflow.Config.Retro.Enabled = true
	workflow.Config.Retro.ProductRepository = "public/destination"
	workflow.Config.Workspace.Root = "/srv/private/worktrees"
	workflow.Config.Identity.GitHubLogin = "private-user"
	global := globalconfig.Config{Projects: []globalconfig.Project{{ID: "private", Workdir: "/srv/private", Workflow: "WORKFLOW.md"}}}

	report, err := scanIssueExposure(context.Background(), global, "private", exposureDeps{
		loadWorkflow: func(globalconfig.Project) (workflowconfig.Workflow, error) {
			return workflow, nil
		},
		repositoryInfo: func(_ context.Context, _ workflowconfig.Config, repository string) (ghconnector.RepositoryInfo, error) {
			if repository == "private/source" {
				return ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, nil
			}
			return ghconnector.RepositoryInfo{Visibility: "public"}, nil
		},
		scan: func(_ context.Context, _ workflowconfig.Config, repository string, identifiers []string) ([]ghconnector.IssueExposure, error) {
			if repository != "public/destination" {
				t.Fatalf("scan repository = %q", repository)
			}
			if !containsString(identifiers, "private/source") || !containsString(identifiers, "/srv/private") || !containsString(identifiers, "private-user") {
				t.Fatalf("scan identifiers = %#v", identifiers)
			}
			return []ghconnector.IssueExposure{{Repository: repository, Number: 42, URL: "https://github.com/public/destination/issues/42", MatchedIdentifier: "private/source"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("scanIssueExposure() error = %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].IssueNumber != 42 || report.Findings[0].SourceProject != "private" || len(report.Warnings) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
