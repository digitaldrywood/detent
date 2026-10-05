package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestRepositoryPolicySource(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name                                                                              string
		gitRef, feature, dirty, local, ancestor, missingRemote, cosmetic, wrongRepository bool
		wantSource, wantReachable                                                         bool
	}{
		{name: "committed working copy", wantSource: true, wantReachable: true},
		{name: "default branch ref", gitRef: true, wantSource: true, wantReachable: true},
		{name: "ancestor of default branch", gitRef: true, ancestor: true, wantSource: true, wantReachable: true},
		{name: "feature branch", gitRef: true, feature: true, wantSource: true},
		{name: "uncommitted config", dirty: true},
		{name: "uncommitted cosmetic config edit", cosmetic: true},
		{name: "uncommitted prompt", local: true},
		{name: "unavailable remote", missingRemote: true},
		{name: "different origin repository", wrongRepository: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := initWorkflowSourceRepo(t)
			runWorkflowSourceGit(t, repo, "checkout", "-b", "develop")
			workflowPath := filepath.Join(repo, "WORKFLOW.md")
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("WORKFLOW.md", "Committed instructions.\n")
			write("detent.yaml", "schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\n")
			runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md", "detent.yaml")
			runWorkflowSourceGit(t, repo, "commit", "-m", "initial definition")
			revision := strings.TrimSpace(runWorkflowSourceGit(t, repo, "rev-parse", "HEAD"))
			if test.ancestor {
				write("unrelated.txt", "Next default-branch commit.\n")
				runWorkflowSourceGit(t, repo, "add", "unrelated.txt")
				runWorkflowSourceGit(t, repo, "commit", "-m", "next commit")
			}
			origin := "https://github.com/acme/orders.git"
			if test.wrongRepository {
				origin = "https://github.com/acme/other.git"
			}
			runWorkflowSourceGit(t, repo, "config", "url."+repo+"/.insteadOf", origin)
			runWorkflowSourceGit(t, repo, "remote", "add", "origin", origin)
			if test.feature {
				runWorkflowSourceGit(t, repo, "checkout", "-b", "feature")
				write("WORKFLOW.md", "Feature instructions.\n")
				runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md")
				runWorkflowSourceGit(t, repo, "commit", "-m", "feature definition")
				runWorkflowSourceGit(t, repo, "symbolic-ref", "HEAD", "refs/heads/develop")
			}
			if test.dirty {
				write("detent.yaml", "schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [Done]\n")
			}
			if test.cosmetic {
				write("detent.yaml", "schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\n\n")
			}
			if test.local {
				write("WORKFLOW.md", "Uncommitted instructions.\n")
			}
			if test.missingRemote {
				runWorkflowSourceGit(t, repo, "remote", "remove", "origin")
			}
			cfg := globalconfig.Project{ID: "orders", Workdir: repo, Workflow: workflowPath}
			if test.gitRef {
				cfg.WorkflowRef = "develop"
				if test.ancestor {
					cfg.WorkflowRef = revision
				}
				if test.feature {
					cfg.WorkflowRef = "feature"
				}
			}
			workflow, err := LoadWorkflowContext(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			resolved, source := repositoryPolicySource(t.Context(), cfg, workflow)
			if (source != nil) != test.wantSource {
				t.Fatalf("source = %+v, want present %v", source, test.wantSource)
			}
			if source != nil {
				if source.Repository != "acme/orders" || source.Commit != resolved.Definition.Revision || source.DefaultBranch != "develop" || source.DefaultBranchHead == "" || source.DefaultBranchReachable != test.wantReachable {
					t.Fatalf("repository provenance = %+v, want reachable %v", source, test.wantReachable)
				}
			}
			if workflow.Prompt != resolved.Prompt || workflow.SourceHash != resolved.SourceHash {
				t.Fatal("provenance changed definition content")
			}
			if resolved.Definition.Layout != workflowconfig.ProjectDefinitionSplit {
				t.Fatalf("layout = %s", resolved.Definition.Layout)
			}
		})
	}
}
