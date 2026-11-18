package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubPolicyCommandsAndDoctor(t *testing.T) {
	t.Parallel()
	fixture := newEnrolledPolicyProject(t, "---\ntracker:\n  kind: hub_native\n  api_key: fixture-token\n  repository: example-org/orders\n---\nWork the issue.\n")
	configPath, root := fixture.configPath, filepath.Dir(fixture.configPath)
	_, _, descriptor, err := resolveHubPolicy(t.Context(), configPath, "orders")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("canceled configuration read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, _, err := resolveHubPolicy(ctx, configPath, "orders"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled policy read = %v, want context canceled", err)
		}
	})
	var approval policy.Approval
	for _, test := range []struct {
		name, subcommand, project string
		valid                     bool
	}{
		{"inspect", "inspect", "orders", true}, {"approve", "approve", "orders", true}, {"unknown project", "inspect", "missing", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := newHubPolicyCommand(func(name string) string {
				if name == "DETENT_HUB_ADMIN_TOKEN" {
					return fixture.adminToken
				}
				return ""
			})
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{test.subcommand, "--config", configPath, "--project", test.project})
			err := cmd.ExecuteContext(t.Context())
			if (err == nil) != test.valid {
				t.Fatalf("command error=%v, valid=%t", err, test.valid)
			}
			if test.valid && !strings.Contains(output.String(), descriptor.ID) {
				t.Fatalf("missing policy output: %s", output.String())
			}
			if test.subcommand == "approve" {
				if err := json.Unmarshal(output.Bytes(), &approval); err != nil {
					t.Fatal(err)
				}
				if err := approval.Policy.Match(descriptor); err != nil || approval.ApprovedBy == "" {
					t.Fatalf("stored approval = %+v, match error = %v", approval, err)
				}
			}
			for _, private := range []string{"fixture-token", "fixture-admin", root} {
				if test.valid && strings.Contains(output.String(), private) {
					t.Fatalf("private value in policy output: %s", private)
				}
			}
		})
	}
	loaded, err := readLocalConfigurationFile(t.Context(), configPath)
	if err != nil {
		t.Fatal(err)
	}
	managed := project.ManagerConfigFromGlobal(loaded).Projects[0]
	workflow, err := project.LoadWorkflow(managed)
	if err != nil {
		t.Fatal(err)
	}
	runtimeProject, err := projectfixture.New(project.Config{Project: managed, Workflow: workflow}, project.Dependencies{Runner: orchestrator.FakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtimeProject.Close(); err != nil {
			t.Error(err)
		}
	})
	runtimePolicy, err := workflowconfig.ResolvePolicy(runtimeProject.Workflow())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimePolicy.Match(descriptor); err != nil {
		t.Fatalf("approval did not resolve runtime host settings: %v", err)
	}
	check := checkDoctorHubPolicy(t.Context(), loaded, loaded.Projects[0], doctorDeps{lookupEnv: func(string) string { return "fixture-admin" }})
	if check.Status != doctorOK || !strings.Contains(check.Detail, approval.ApprovedBy) || !strings.Contains(check.Detail, descriptor.ID) {
		t.Fatalf("doctor policy = %#v", check)
	}
	t.Run("enrolled runner credentials", func(t *testing.T) {
		organization := "org_" + strings.Repeat("a", 32)
		projectID := "prj_" + strings.Repeat("b", 32)
		var credential string
		mappedWorkflow := workflow
		mappedWorkflow.Config = project.MapNativeTracker(mappedWorkflow.Config, true)
		enrolledPolicy, err := project.ResolvePolicy(managed, mappedWorkflow)
		if err != nil {
			t.Fatal(err)
		}
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v2/organizations/"+organization+"/projects/"+projectID+"/policy" || r.Header.Get("Authorization") != "Bearer "+credential {
				t.Error("doctor did not use the enrolled policy credential and project scope")
			}
			if err := json.NewEncoder(w).Encode(policy.Approval{Policy: enrolledPolicy}); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(hub.Close)
		path := filepath.Join(t.TempDir(), "private", "identity.json")
		identity, err := runnerauth.Initialize(path, hub.URL)
		if err != nil {
			t.Fatal(err)
		}
		credential = identity.Credential
		identity.Identity.OrganizationID = tracker.OrganizationID(organization)
		identity.Identity.ProjectIDs = []tracker.ProjectID{tracker.ProjectID(projectID)}
		identity.Identity.Operations = []string{runnerauth.Read}
		identity.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
		if err := runnerauth.Save(path, identity); err != nil {
			t.Fatal(err)
		}
		enrolled := loaded
		enrolled.Client = globalconfig.HubClient{URL: hub.URL, IdentityFile: path, OrganizationID: organization, NativeProjects: map[string]string{"orders": projectID}}
		check := checkDoctorHubPolicy(t.Context(), enrolled, enrolled.Projects[0], doctorDeps{lookupEnv: func(string) string { return "" }})
		if check.Status != doctorOK {
			t.Fatalf("enrolled doctor policy = %#v", check)
		}
	})
	if err := os.WriteFile(filepath.Join(fixture.origin, "WORKFLOW.md"), []byte("---\ntracker:\n  kind: hub_native\n  api_key: fixture-token\n  repository: example-org/orders\ngate:\n  kind: artifact\n---\nChanged instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDoctorWorkflowSourceGit(t, fixture.origin, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-am", "change workflow")
	runDoctorWorkflowSourceGit(t, fixture.checkout, "fetch", "origin")
	check = checkDoctorHubPolicy(t.Context(), loaded, loaded.Projects[0], doctorDeps{lookupEnv: func(string) string { return "fixture-admin" }})
	if check.Status != doctorOK || !strings.Contains(check.Detail, descriptor.ID) || !strings.Contains(check.Detail, "gate=command") {
		t.Fatalf("doctor did not preserve approved native policy = %#v", check)
	}
	t.Run("invalid approved descriptor", func(t *testing.T) {
		invalid := descriptor
		invalid.ID = "policy_" + strings.Repeat("0", 64)
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewEncoder(w).Encode(policy.Approval{Policy: invalid}); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(hub.Close)
		configured := loaded
		configured.Client = globalconfig.HubClient{URL: hub.URL, OrganizationID: loaded.Client.OrganizationID, NativeProjects: loaded.Client.NativeProjects}
		check := checkDoctorHubPolicy(t.Context(), configured, configured.Projects[0], doctorDeps{lookupEnv: func(string) string { return "fixture-admin" }})
		if check.Status != doctorFail || !strings.Contains(check.Detail, "policy_mismatch") || !strings.Contains(check.Hint, "hub policy inspect") {
			t.Fatalf("doctor accepted invalid policy = %#v", check)
		}
	})
}

func TestHubPolicyInspectRejectsMappedNativeUnsupportedWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, feature, want string
	}{
		{"intake", "intake:\n  sources:\n    - name: errors\n      kind: webhook\n      secret: test-secret\n      creates:\n        status: Backlog\n", "intake"},
		{"routines", "schedule_ownership:\n  enabled: true\n  key: example-org/orders\n  repository: example-org/orders\nroutines:\n  - name: audit\n    schedule: '0 * * * *'\n    prompt: Inspect.\n", "routines"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := "---\ntracker:\n  kind: hub_native\n  repository: example-org/orders\n  api_key: test-token\n" + test.feature + "---\nPrompt\n"
			fixture := newEnrolledPolicyProject(t, raw)
			cfg, _, _, err := resolveHubPolicy(t.Context(), fixture.configPath, "orders")
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "retired configuration key") {
				t.Fatalf("mapped policy inspection error = %v, want %s retired-key guidance", err, test.want)
			}
			if len(cfg.Projects) != 1 || cfg.Client.NativeProjects["orders"] != "prj_org_example" {
				t.Fatalf("enrolled projects = %+v native = %v, want the Hub project mapped to its checkout", cfg.Projects, cfg.Client.NativeProjects)
			}
			check := checkDoctorHubPolicy(t.Context(), cfg, cfg.Projects[0], doctorDeps{})
			if check.Status != doctorFail || !strings.Contains(check.Detail, test.want) || !strings.Contains(check.Detail, "retired configuration key") {
				t.Fatalf("mapped doctor policy = %#v, want %s retired-key guidance", check, test.want)
			}
		})
	}
}

func TestHubPolicyInspectUsesNativeStartupTracker(t *testing.T) {
	t.Parallel()
	fixture := newEnrolledPolicyProject(t, "---\ntracker:\n  kind: hub_native\n  repository: example-org/orders\n  api_key: test-token\n---\nPrompt\n")
	configPath := fixture.configPath
	cfg, inspected, descriptor, err := resolveHubPolicy(t.Context(), configPath, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
		t.Fatalf("inspected tracker = %s, want hub_native", inspected.Config.Tracker.Kind)
	}
	managed := project.ManagerConfigFromGlobal(cfg).Projects[0]
	runtime, err := project.LoadWorkflow(managed)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Config = project.MapNativeTracker(runtime.Config, true)
	runtimeDescriptor, err := project.ResolvePolicy(managed, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := descriptor.Match(runtimeDescriptor); err != nil {
		t.Fatalf("inspected descriptor differs from startup descriptor: %v", err)
	}
}
