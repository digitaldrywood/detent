package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerStartupIsolatesRetiredProjectConfiguration(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		t.Run(map[bool]string{false: "single invalid project reports configuration", true: "healthy project claims work"}[healthy], func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			cfg, err := globalconfig.Default()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Path = filepath.Join(root, "global.yaml")
			cfg.Projects = nil
			cfg.Client.NativeProjects = map[string]string{"invalid": "prj_invalid"}
			for _, name := range []string{"invalid", "valid"} {
				if name == "valid" && !healthy {
					continue
				}
				dir := filepath.Join(root, name)
				checkout(t, dir)
				runDoctorWorkflowSourceGit(t, dir, "remote", "set-url", "origin", "https://github.com/example-org/"+name+".git")
				definition := "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/" + name + "\n"
				if name == "invalid" {
					definition += "routines: {}\n"
				}
				if err := os.WriteFile(filepath.Join(dir, "detent.yaml"), []byte(definition), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte("Work the issue.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runDoctorWorkflowSourceGit(t, dir, "add", ".")
				runDoctorWorkflowSourceGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "definition")
				cfg.Projects = append(cfg.Projects, globalconfig.Project{ID: name, Workdir: dir, Workflow: filepath.Join(dir, "WORKFLOW.md"), Weight: 1})
				cfg.Client.NativeProjects[name] = "prj_" + name
			}
			var descriptor policy.Descriptor
			if healthy {
				_, _, resolved, err := resolveRunnerProjectPolicy(t.Context(), cfg, "valid")
				if err != nil {
					t.Fatal(err)
				}
				descriptor = resolved
			}
			var identity runnerauth.Identity
			var invalidReport runnerauth.LocalChecks
			var problems []runnerauth.Problem
			claims := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeLocalChecksCapability, tracker.NativeProviderCapacityCapability}})
				case strings.HasSuffix(r.URL.Path, "/policy/observed"):
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "/policy"):
					_ = json.NewEncoder(w).Encode(policy.Approval{Policy: descriptor})
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var body struct {
						LocalChecks runnerauth.LocalChecks `json:"local_checks"`
						Problems    []runnerauth.Problem   `json:"problems"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if strings.Contains(r.URL.Path, "/prj_invalid/") {
						invalidReport, problems = body.LocalChecks, body.Problems
					}
					_ = json.NewEncoder(w).Encode(runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 2, ProjectIDs: identity.ProjectIDs}.Normalized()})
				case strings.HasSuffix(r.URL.Path, "/claims/preview"):
					_ = json.NewEncoder(w).Encode(tracker.NativeCapacityPage{Items: []tracker.NativeIssue{{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_valid", WorkItemID: "wi_example"}, State: "Todo", SchedulingEligible: new(true), DependencyReady: true}}})
				case strings.HasSuffix(r.URL.Path, "/claims"):
					if !strings.Contains(r.URL.Path, "/prj_valid/") {
						t.Error("invalid project claimed work")
					}
					claims++
					_ = json.NewEncoder(w).Encode(tracker.NativeLease{IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted}, ProviderReservation: &providercapacity.Reservation{}, ID: "lease_example", WorkItemID: "wi_example", PolicyID: descriptor.ID, FencingToken: 1, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
				case strings.HasSuffix(r.URL.Path, "/work-items/wi_example"):
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_valid", WorkItemID: "wi_example"}, State: "Todo", SchedulingEligible: new(true), DependencyReady: true}
					if r.URL.Query().Get("view") == "recovery" {
						_ = json.NewEncoder(w).Encode(tracker.NativeRecovery{Issue: issue})
					} else {
						_ = json.NewEncoder(w).Encode(issue)
					}
				case strings.HasSuffix(r.URL.Path, "/projects/prj_invalid"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_invalid", OrganizationID: "org_example", Profile: "native", Repository: "example-org/invalid"})
				case strings.HasSuffix(r.URL.Path, "/projects/prj_valid"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_valid", OrganizationID: "org_example", Profile: "native", Repository: "example-org/valid"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			identityPath := filepath.Join(root, "identity.json")
			file, err := runnerauth.Initialize(identityPath, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_invalid", "prj_valid"}
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			identity = file.Identity
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			cfg.Client.URL, cfg.Client.OrganizationID, cfg.Client.IdentityFile = server.URL, "org_example", identityPath
			cfg.Client.TokenEnvironment = ""
			cfg.Client.ProviderCapacityFile = writeProviderCapacityFixture(t, root)
			source, err := newHubScheduling(t.Context(), cfg, "test", hubSchedulingOptions{workspaceSupport: testWorkspaceSupport})
			if err != nil {
				t.Fatalf("startup: %v", err)
			}
			scheduler := source.(*hubclient.Scheduler)
			if err := scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			if invalidReport.Configuration != "failed" || !strings.Contains(invalidReport.ConfigurationMessage, "retired configuration key") || !strings.Contains(invalidReport.ConfigurationFix, "config migrate") {
				t.Fatalf("invalid project report: %+v", invalidReport)
			}
			found := false
			for _, problem := range problems {
				if problem.ProjectID == "prj_invalid" && problem.Code == "settings_invalid" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing settings_invalid: %+v", problems)
			}
			if err := scheduler.PrepareProject(t.Context(), "invalid"); err == nil {
				t.Fatal("retired configuration became dispatchable")
			}
			if healthy {
				issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "valid", Repository: "example-org/valid", Policy: descriptor, ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					return providercapacity.Requirement{}, nil
				}})
				if err != nil || len(issues) != 1 || issues[0].ID != "wi_example" || claims != 1 {
					t.Fatalf("healthy claim: %+v, %v, claims=%d", issues, err, claims)
				}
			} else if claims != 0 {
				t.Fatal("idle runner claimed work")
			}
		})
	}
}
