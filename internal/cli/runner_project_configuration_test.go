package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectConfigurationOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	for _, scenario := range []string{"read", "intake off", "foreign project", "revoked project grant", "replaced mapping", "foreign binding", "expired identity"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			identityPath := filepath.Join(root, "identity.json")
			identity, err := runnerauth.Initialize(identityPath, "http://127.0.0.1:1")
			if err != nil {
				t.Fatal(err)
			}
			identity.Identity.OrganizationID = "org_test"
			identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			identity.Identity.ExpiresAt = time.Now().Add(time.Hour)
			if err := runnerauth.Save(identityPath, identity); err != nil {
				t.Fatal(err)
			}
			workflow := filepath.Join(root, "WORKFLOW.md")
			if err := os.WriteFile(workflow, []byte("Private instructions\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: memory\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "intake off" {
				cfg.Global.LocalIntakeEnabled = new(false)
			}
			cfg.Client = globalconfig.HubClient{URL: identity.HubURL, IdentityFile: identityPath, OrganizationID: "org_test", NativeProjects: map[string]string{"local-name": "prj_test"}, Capacity: 2}
			cfg.Projects = []globalconfig.Project{{ID: "local-name", Workflow: workflow, Workdir: root, Paused: true, Weight: 1}}
			if err := globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
				t.Fatal(err)
			}
			cfg, err = globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			attempts, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(root, "runtime.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := attempts.Close(); err != nil {
					t.Error(err)
				}
			})
			manager, err := project.NewManager(project.ManagerConfigFromGlobal(cfg), project.ManagerDependencies{ProjectFactory: func(selected globalconfig.Project) (*project.Project, error) {
				return project.Load(selected, project.Dependencies{Runner: orchestrator.FakeRunner{}, WorkAttempts: attempts})
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, p := range manager.Registry().List() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			state := newGlobalConfigState(cfg)
			owner := runnerProjectConfigurationOwner(cfg, state.get, project.NewConfigurationOwner(cfg, state.get, manager, attempts, nil))
			id := "prj_test"
			switch scenario {
			case "foreign project":
				id = "prj_foreign"
			case "revoked project grant":
				identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_other"}
			case "replaced mapping":
				changed := cfg
				changed.Client.NativeProjects = map[string]string{"local-name": "prj_other"}
				state.set(changed)
			case "foreign binding":
				identity.Identity.Binding = runnerauth.NewBinding()
			case "expired identity":
				identity.Identity.ExpiresAt = time.Now().Add(-time.Hour)
			}
			if scenario == "foreign binding" {
				raw, err := json.Marshal(identity)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(identityPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := runnerauth.Save(identityPath, identity); err != nil {
				t.Fatal(err)
			}
			view := owner(t.Context(), id, nil)
			if view.ProjectID != id {
				t.Fatalf("foreign identity in receipt=%+v", view)
			}
			if scenario == "read" || scenario == "intake off" {
				if view.Constraint != "" || !view.Registered || view.EffectivePolicy == nil || view.LocalBindingPolicy == nil || view.AllowLocalBinding {
					t.Fatalf("selected owner=%+v", view)
				}
				if view.LocalIntakeEnabled != (scenario != "intake off") {
					t.Fatalf("effective intake read = %t", view.LocalIntakeEnabled)
				}
				if strings.Contains(view.Constraint, root) {
					t.Fatal("private path escaped owner")
				}
			} else if view.Source != "unavailable" || view.Constraint == "" || view.Registered || view.Applied {
				t.Fatalf("authority refusal=%+v", view)
			}
		})
	}
}
