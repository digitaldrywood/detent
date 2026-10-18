package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubPolicyCommandsAndDoctor(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n  repository: acme/orders\n---\nPRIVATE WORKFLOW\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "global.yaml")
	cfg, err := globalconfig.DefaultAt(configPath, globalconfig.WithHome(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: root, Weight: 1}}
	cfg.Global.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}
	cfg.Global.RateWindowPacing = workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}
	cfg.Global.Identity.Name = "repository-operator"
	cfg.Client = globalconfig.HubClient{URL: "http://127.0.0.1:1", MachineID: "machine_test"}
	if err := globalconfig.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	_, _, descriptor, err := resolveHubPolicy(t.Context(), configPath, "orders")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repositories/acme/orders/policy" || r.Header.Get("Authorization") != "Bearer fixture-admin" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			var change policy.Change
			if err := json.NewDecoder(r.Body).Decode(&change); err != nil {
				t.Error(err)
			}
			if err := change.Policy.Match(descriptor); err != nil {
				t.Error(err)
			}
		}
		if err := json.NewEncoder(w).Encode(policy.Approval{Policy: descriptor, ApprovedBy: "administrator", ApprovedAt: "2026-09-05T12:00:00Z"}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	cfg.Client.URL = server.URL
	if err := globalconfig.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, subcommand, project string
		valid                     bool
	}{
		{"inspect", "inspect", "orders", true}, {"approve", "approve", "orders", true}, {"unknown project", "inspect", "missing", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := newHubPolicyCommand(func(string) string { return "fixture-admin" })
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
			for _, private := range []string{"PRIVATE WORKFLOW", "fixture-admin", root} {
				if test.valid && strings.Contains(output.String(), private) {
					t.Fatalf("private value in policy output: %s", private)
				}
			}
		})
	}
	loaded, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatal(err)
	}
	managed := project.ManagerConfigFromGlobal(loaded).Projects[0]
	workflow, err := project.LoadWorkflow(managed)
	if err != nil {
		t.Fatal(err)
	}
	runtimeProject, err := project.New(project.Config{Project: managed, Workflow: workflow}, project.Dependencies{Runner: orchestrator.FakeRunner{}})
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
	if check.Status != doctorOK || !strings.Contains(check.Detail, "administrator") || !strings.Contains(check.Detail, descriptor.ID) {
		t.Fatalf("doctor policy = %#v", check)
	}
	t.Run("enrolled runner credentials", func(t *testing.T) {
		organization := "org_" + strings.Repeat("a", 32)
		projectID := "prj_" + strings.Repeat("b", 32)
		var credential string
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v2/organizations/"+organization+"/projects/"+projectID+"/policy" || r.Header.Get("Authorization") != "Bearer "+credential {
				t.Error("doctor did not use the enrolled policy credential and project scope")
			}
			if err := json.NewEncoder(w).Encode(policy.Approval{Policy: descriptor}); err != nil {
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
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: memory\n  repository: acme/orders\ngate:\n  kind: artifact\n---\nChanged instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check = checkDoctorHubPolicy(t.Context(), loaded, loaded.Projects[0], doctorDeps{lookupEnv: func(string) string { return "fixture-admin" }})
	if check.Status != doctorFail || !strings.Contains(check.Detail, "policy_mismatch") || !strings.Contains(check.Hint, "hub policy inspect") {
		t.Fatalf("doctor mismatch = %#v", check)
	}
}

func TestHubPolicyInspectRejectsMappedNativeUnsupportedWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, feature, want string
	}{
		{"intake", "intake:\n  sources:\n    - name: errors\n      kind: webhook\n      secret: test-secret\n      creates:\n        status: Backlog\n", "intake.sources"},
		{"routines", "schedule_ownership:\n  enabled: true\n  key: acme/orders\n  repository: acme/orders\nroutines:\n  - name: audit\n    schedule: '0 * * * *'\n    prompt: Inspect.\n", "routines"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			raw := "---\ntracker:\n  kind: github\n  project_slug: PVT_test\n  repository: acme/orders\n  api_key: test-token\n" + test.feature + "---\nPrompt\n"
			if err := os.WriteFile(workflowPath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "global.yaml")
			cfg, err := globalconfig.DefaultAt(configPath, globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: root, Weight: 1}}
			cfg.Client = globalconfig.HubClient{URL: "http://127.0.0.1:1", OrganizationID: "org_test", NativeProjects: map[string]string{"orders": "prj_test"}}
			if err := globalconfig.Write(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = resolveHubPolicy(t.Context(), configPath, "orders")
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "migrate") {
				t.Fatalf("mapped policy inspection error = %v, want %s migration guidance", err, test.want)
			}
			check := checkDoctorHubPolicy(t.Context(), cfg, cfg.Projects[0], doctorDeps{})
			if check.Status != doctorFail || !strings.Contains(check.Detail, test.want) || !strings.Contains(check.Detail, "migrate") {
				t.Fatalf("mapped doctor policy = %#v, want %s migration guidance", check, test.want)
			}
		})
	}
}

func TestHubPolicyInspectUsesNativeStartupTracker(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\ntracker:\n  kind: github\n  project_slug: PVT_test\n  repository: acme/orders\n  api_key: test-token\n---\nPrompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "global.yaml")
	cfg, err := globalconfig.DefaultAt(configPath, globalconfig.WithHome(root))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: root, Weight: 1}}
	cfg.Client = globalconfig.HubClient{URL: "http://127.0.0.1:1", OrganizationID: "org_test", NativeProjects: map[string]string{"orders": "prj_test"}}
	if err := globalconfig.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	_, inspected, descriptor, err := resolveHubPolicy(t.Context(), configPath, "orders")
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
