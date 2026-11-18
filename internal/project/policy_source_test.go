package project

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRepositoryPolicySource(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name                                                                              string
		gitRef, feature, dirty, local, ancestor, missingRemote, cosmetic, wrongRepository bool
		wantSource, wantReachable, external                                               bool
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
		{name: "external split definition retains unavailable provenance", external: true},
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
			if test.external {
				external := t.TempDir()
				for _, name := range []string{"WORKFLOW.md", "detent.yaml"} {
					raw, err := os.ReadFile(filepath.Join(repo, name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(external, name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				workflowPath = filepath.Join(external, "WORKFLOW.md")
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
			scheduling := &managedRepositoryScheduling{}
			p := &Project{cfg: cfg, policyScheduling: scheduling}
			if _, err := p.loadManagedWorkflow(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			if (scheduling.source != nil) != test.wantSource {
				t.Fatalf("managed application lost repository authority: %+v", scheduling.source)
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

type managedRepositoryScheduling struct {
	policyTestScheduling
	source *policy.RepositorySource
}

func (s *managedRepositoryScheduling) ResolveProjectWorkflow(_ context.Context, _ string, workflow workflowconfig.Workflow, source *policy.RepositorySource) (workflowconfig.Workflow, error) {
	s.source = source
	return workflow, nil
}

type policyReportTransport func(*http.Request) (*http.Response, error)

func (f policyReportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerifiedRunnerPrompt(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := initWorkflowSourceRepo(t)
	const repository = "example-org/example-project"
	const origin = "https://github.com/" + repository + ".git"
	runWorkflowSourceGit(t, repo, "config", "url."+repo+"/.insteadOf", origin)
	runWorkflowSourceGit(t, repo, "remote", "add", "origin", origin)
	cfg := globalconfig.Project{ID: "example", Workdir: repo, Workflow: filepath.Join(repo, "WORKFLOW.md")}
	const prompt = "Approved repository instructions."
	for name, body := range map[string]string{"WORKFLOW.md": prompt + "\n", "detent.yaml": "schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runWorkflowSourceGit(t, repo, "add", "WORKFLOW.md", "detent.yaml")
	runWorkflowSourceGit(t, repo, "commit", "-m", "repository policy")
	first := strings.TrimSpace(runWorkflowSourceGit(t, repo, "rev-parse", "HEAD"))
	local, err := LoadWorkflowContext(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	local.Definition.Revision = first
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("Unrelated repository change.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runWorkflowSourceGit(t, repo, "add", "README.md")
	runWorkflowSourceGit(t, repo, "commit", "-m", "unrelated change")
	second := strings.TrimSpace(runWorkflowSourceGit(t, repo, "rev-parse", "HEAD"))
	for _, tc := range []struct {
		name                                                                 string
		settings, pinnedRevision, repair, unverified, changed, reload, older bool
	}{
		{name: "same policy on next commit"},
		{name: "older approval at same commit", pinnedRevision: true, older: true},
		{name: "replacement without approved gate", settings: true, pinnedRevision: true},
		{name: "replacement after unrelated commit", settings: true},
		{name: "stale checkout at a different revision", settings: true, changed: true},
		{name: "changed repository inputs at pinned revision", settings: true, pinnedRevision: true, changed: true},
		{name: "verified repair", repair: true}, {name: "unverified repair", repair: true, unverified: true},
		{name: "reload after workflow edit", reload: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := local
			if tc.settings {
				sources := *base.DefinitionSources
				if tc.changed {
					sources.Workflow = []byte("Changed approved source.")
				}
				sources.HasLocalConfig = true
				sources.LocalConfig = []byte("schema: 1\ngate:\n  run: make approved-gate\n")
				base, err = workflowconfig.ParseProjectDefinition(sources)
				if err != nil {
					t.Fatal(err)
				}
				base.Definition.Revision = first
			}
			if tc.older {
				authored := *base.Authored
				authored.Version = 3
				base.Authored = &authored
			}
			approval, err := ResolvePolicy(cfg, base)
			if err != nil {
				t.Fatal(err)
			}
			shared, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, approval)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := workflowconfig.NewExecutionConfiguration(shared, approval, workflowconfig.ResolveCloudModelSelection(workflowconfig.ModelSelection{}, workflowconfig.ModelSelection{}))
			if err != nil {
				t.Fatal(err)
			}
			var observations []policy.Observation
			client, err := api.New(api.Config{URL: "https://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: policyReportTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch {
				case strings.HasSuffix(r.URL.Path, "/policy/observed"):
					var o policy.Observation
					if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
						t.Fatal(err)
					}
					observations = append(observations, o)
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "/policy"):
					if tc.repair {
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "policy_mismatch", "message": workflowconfig.ErrNativePromptUnavailable.Error()})
					} else {
						_ = json.NewEncoder(w).Encode(policy.Approval{Policy: approval, ExecutionConfiguration: execution})
					}
				default:
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{})
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := dispatch.NewScheduler(client, dispatch.SchedulerConfig{OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{cfg.ID: "prj_example"}, Machine: api.Machine{ID: "machine_example", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			current := cfg
			wantCommit := second
			if tc.pinnedRevision {
				current.WorkflowRef = first
				wantCommit = first
			}
			runner, err := LoadWorkflowContext(t.Context(), current)
			if err != nil {
				t.Fatal(err)
			}
			if tc.unverified {
				current.Workdir = t.TempDir()
			}
			_, err = configureProjectPolicy(t.Context(), current, &runner, scheduler)
			if tc.changed && tc.pinnedRevision {
				if !errors.Is(err, workflowconfig.ErrNativeAuthoredMismatch) || len(observations) != 0 || runner.Config.Policy.ID != "" {
					t.Fatalf("changed source at pinned revision: %v", err)
				}
				return
			}
			if tc.repair {
				if err == nil || !strings.Contains(err.Error(), "approved workflow instructions are unavailable") || runner.Config.Policy.ID != "" {
					t.Fatalf("unavailable approval admitted work or lost reason: %v", err)
				}
				if tc.unverified {
					if len(observations) != 0 {
						t.Fatal("reported an unverified repair")
					}
					return
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if runner.Prompt != shared.Prompt || runner.Config.Policy.ID != approval.ID || runner.Config.Policy.Workflow.Revision != first {
					t.Errorf("execution prompt %q policy %s at %s", runner.Prompt, runner.Config.Policy.ID, runner.Config.Policy.Workflow.Revision)
				}
				if tc.settings && runner.Config.Gate.Run != "make approved-gate" {
					t.Error("replacement discarded approved gate")
				}
			}
			if len(observations) != 1 {
				t.Fatalf("reported %d observations, want one", len(observations))
			}
			o := observations[0]
			if tc.changed || tc.older {
				if o.ID == approval.ID || o.Source == nil || o.Source.Commit != wantCommit || o.Source.Repository != repository || o.Workflow == nil || o.Workflow.Revision != wantCommit || o.Authored.Files["WORKFLOW.md"] != prompt+"\n" {
					t.Fatal("lost the verified candidate or its commit")
				}
				return
			}
			if o.ID != approval.ID || o.Source == nil || o.Source.Commit != wantCommit || o.Source.Repository != repository || o.Workflow == nil || o.Workflow.Revision != wantCommit {
				t.Fatalf("observation policy %s, source %+v, want policy %s at %s", o.ID, o.Source, approval.ID, wantCommit)
			}
			if tc.reload {
				const next = "A new notebook. \t\n"
				if err := os.WriteFile(current.Workflow, []byte(next), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, repo, "commit", "-am", "edit workflow")
				head := strings.TrimSpace(runWorkflowSourceGit(t, repo, "rev-parse", "HEAD"))
				p := &Project{cfg: current, id: ID(current.ID), policyScheduling: scheduler, logger: slog.Default(), workflow: runner, hubPolicyID: approval.ID, workflowSource: WorkflowSourceStatus{Hash: runner.SourceHash}}
				if err := p.reconcileWorkflow(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(observations) != 2 {
					t.Fatalf("reload reported %d total observations", len(observations))
				}
				pending := observations[1]
				if pending.ID == approval.ID || pending.Authored.Files["WORKFLOW.md"] != next || pending.Source == nil || pending.Source.Commit != head || pending.Source.DefaultBranchHead != head || !pending.Source.DefaultBranchReachable || pending.Workflow.Revision != head {
					t.Fatal("reload lost the candidate or its default-branch provenance")
				}
				if p.observedPolicy.ID != pending.ID || p.Workflow().Prompt != prompt+"\n" || p.Workflow().Config.Policy.ID != approval.ID || p.Workflow().Definition.Revision != first {
					t.Fatal("reload replaced the current approved work")
				}
			}

		})
	}
}
