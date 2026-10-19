package project

import (
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"

	"github.com/digitaldrywood/detent/internal/connector"

	"github.com/digitaldrywood/detent/internal/publication"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestBuildRetroIssueStoresRoutesProductRepository(t *testing.T) {
	t.Parallel()

	cfg := projectRaceWorkflowConfig()
	cfg.Tracker.Repository = "example/project"
	cfg.Retro.Enabled = true
	cfg.Retro.ProductRepository = "digitaldrywood/detent"
	cfg.Retro.AllowPublicCrossProjectDetails = true
	cfg.Workspace.Root = "/srv/project/worktrees"
	cfg.Identity.GitHubLogin = "private-user"
	cfg.Retro.Normalize()
	projectConnector := fakehub.New(fakehub.Config{Stateful: true})
	productConnector := fakehub.New(fakehub.Config{Stateful: true})
	var repositories []string
	var productPolicy publication.Policy

	projectIssues, productIssues, created, err := buildRetroIssueStores(t.Context(), cfg, projectConnector, func(candidate workflowconfig.Config) (connector.Connector, error) {
		repositories = append(repositories, candidate.Tracker.Repository)
		productPolicy = candidate.Tracker.Publication
		return productConnector, nil
	})
	if err != nil {
		t.Fatalf("buildRetroIssueStores() error = %v", err)
	}
	if projectIssues != projectConnector {
		t.Fatalf("project issue store = %T, want project connector", projectIssues)
	}
	if productIssues != productConnector || created != productConnector {
		t.Fatalf("product issue store/connector = %T/%T, want product connector", productIssues, created)
	}
	if len(repositories) != 1 || repositories[0] != "digitaldrywood/detent" {
		t.Fatalf("product repositories = %v, want digitaldrywood/detent", repositories)
	}
	if productPolicy.DestinationRepository != "digitaldrywood/detent" || len(productPolicy.Sources) != 1 {
		t.Fatalf("product publication policy = %#v", productPolicy)
	}
	source := productPolicy.Sources[0]
	if source.Repository != "example/project" || len(source.Workspaces) == 0 || source.Workspaces[0] != "/srv/project/worktrees" || len(source.Logins) != 1 || source.Logins[0] != "private-user" {
		t.Fatalf("product publication source = %#v", source)
	}
	if !productPolicy.AllowPublicCrossProjectDetails {
		t.Fatal("product publication override = false, want true")
	}
}

func projectRaceWorkflowConfig() workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.APIKey = "fixture-token"
	cfg.Tracker.ProjectSlug = "fixture-project"
	cfg.Tracker.Repository = "example-org/example-project"
	cfg.Polling.IntervalMS = int(time.Hour / time.Millisecond)
	return cfg
}
