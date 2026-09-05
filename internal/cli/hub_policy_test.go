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
