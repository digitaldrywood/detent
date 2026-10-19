package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalConfigurationEnrolledStartupRenewsCredentials(t *testing.T) {
	for _, name := range []string{"runner_alpha", "runner_beta"} {
		t.Run(name, func(t *testing.T) {
			for _, test := range []struct {
				name    string
				held    bool
				revoked bool
				wantErr string
			}{
				{name: "expired credential"},
				{name: "held writer", held: true, wantErr: "instance lock is held"},
				{name: "revoked credential", revoked: true, wantErr: "unauthorized"},
			} {
				t.Run(test.name, func(t *testing.T) {
					var file runnerauth.File
					renewals := 0
					expires := time.Now().Add(runnerauth.CredentialTTL).UTC()
					projectID := tracker.ProjectID("prj_" + name)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if test.revoked || r.Header.Get("Authorization") != "Bearer "+file.Credential {
							w.WriteHeader(http.StatusUnauthorized)
							if err := json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized"}); err != nil {
								t.Error(err)
							}
							return
						}
						identity := file.Identity
						switch {
						case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID+"/renew"):
							renewals++
							identity.ExpiresAt = expires
						case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID):
							if renewals == 0 {
								w.WriteHeader(http.StatusUnauthorized)
								return
							}
							identity.ExpiresAt = expires
						case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/"+string(projectID)):
							if err := json.NewEncoder(w).Encode(tracker.NativeProject{ID: projectID, OrganizationID: "org_example"}); err != nil {
								t.Error(err)
							}
							return
						default:
							t.Errorf("unexpected startup request: %s %s", r.Method, r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						if err := json.NewEncoder(w).Encode(identity); err != nil {
							t.Error(err)
						}
					}))
					t.Cleanup(server.Close)
					dir := t.TempDir()
					identityPath := filepath.Join(dir, "private", "identity.json")
					var err error
					file, err = runnerauth.Initialize(identityPath, server.URL)
					if err != nil {
						t.Fatal(err)
					}
					file.Identity.OrganizationID = "org_example"
					file.Identity.ProjectIDs = []tracker.ProjectID{projectID}
					file.Identity.Operations = []string{runnerauth.Read}
					file.Identity.ExpiresAt = time.Now().Add(-time.Hour)
					if err := runnerauth.Save(identityPath, file); err != nil {
						t.Fatal(err)
					}
					before, err := os.ReadFile(identityPath)
					if err != nil {
						t.Fatal(err)
					}
					if test.held {
						lock, err := instancelock.Acquire(identityPath + ".lock")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := lock.Close(); err != nil {
								t.Error(err)
							}
						})
					}
					configPath := filepath.Join(dir, "global.yaml")
					config := fmt.Sprintf("apiVersion: detent/v1\nkind: GlobalConfig\nworkspace_root: %s\nclient:\n  hub_url: %s\n  organization_id: org_example\n  identity_file: %s\n  capacity: 1\n", filepath.Join(dir, "work"), server.URL, identityPath)
					if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
						t.Fatal(err)
					}
					boot, err := resolveBootConfig(t.Context(), configPath, "", runtimeFlags{}, defaultOptions())
					if test.wantErr != "" {
						if err == nil || !strings.Contains(err.Error(), test.wantErr) {
							t.Fatalf("startup error = %v, want %s", err, test.wantErr)
						}
						if after, readErr := os.ReadFile(identityPath); readErr != nil || !bytes.Equal(before, after) || renewals != 0 {
							t.Fatalf("refused startup changed credentials: %v, renewals=%d", readErr, renewals)
						}
						return
					}
					if err != nil || boot.Mode != BootModeRunning || boot.Global.Client.NativeProjects[string(projectID)] != string(projectID) {
						t.Fatalf("enrolled startup did not resolve current project: %v", err)
					}
					saved, err := runnerauth.Load(identityPath)
					if err != nil || !saved.Identity.ExpiresAt.Equal(expires) || saved.Credential != file.Credential || renewals != 1 {
						t.Fatalf("startup renewal was not persisted: %v, renewals=%d", err, renewals)
					}
				})
			}
		})
	}
}

func TestLocalConfigurationStartupAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	workflow := filepath.Join(dir, "detent.yaml")
	if err := os.WriteFile(workflow, []byte("workflow fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := globalconfig.DefaultAt(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []globalconfig.Project{{ID: "one", Workflow: workflow, Workdir: dir, Weight: 4, Priority: 1}, {ID: "two", Workflow: workflow, Workdir: dir, Weight: 8, Priority: 2}}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	global := document["global"].(map[string]any)
	agents, ok := global["agents"].(map[string]any)
	if !ok {
		agents = map[string]any{}
		global["agents"] = agents
	}
	agents["model_selection"] = map[string]any{"preset": "sol_first", "normal_model": "legacy-model"}
	raw, err = yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = globalconfig.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(dir, "detent.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := initializeLocalConfiguration(t.Context(), db, cfg, logger)
	if err != nil || len(first.Projects) != 2 || first.Projects[0].ID != "two" || first.Projects[0].ModelSelection.Model("normal") != "legacy-model" {
		t.Fatalf("initial=%+v,%v", first.Projects, err)
	}
	rank, err := db.LocalProjectRank(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.LocalProjectRank(t.Context(), &projectsettings.RankChange{ExpectedRevision: rank.Revision, ProjectIDs: []tracker.ProjectID{"one", "two"}}); err != nil {
		t.Fatal(err)
	}
	model, err := db.LocalModelSelection(t.Context(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if model.Selection == nil {
		t.Fatal("organization model selection is missing")
	}
	selection := *model.Selection
	selection.NormalModel = new("db-model")
	if _, err := db.LocalModelSelection(t.Context(), "", &projectsettings.ModelSelectionChange{ExpectedRevision: model.Revision, Selection: &selection}); err != nil {
		t.Fatal(err)
	}
	document["projects"] = 42
	agents["model_selection"] = map[string]any{"preset": "unknown"}
	raw, err = yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restart, err := readLocalConfigurationFile(path)
	if err != nil || len(restart.Projects) != 2 || restart.Projects[0].ID != "one" || restart.Projects[0].ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("restart=%+v,%v", restart.Projects, err)
	}
	manager := &globalReloadManager{}
	reloader := &globalConfigReloader{current: first, manager: manager, settings: db, logger: logger}
	reloader.handle(t.Context(), configwatcher.FileUpdate[globalconfig.Config]{Path: path})
	if manager.calls != 1 || !reflect.DeepEqual(globalProjectIDs(reloader.current.Projects), []string{"one", "two"}) || manager.config.Projects[0].GlobalAgents.ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("reload=%+v,calls=%d", reloader.current.Projects, manager.calls)
	}
	if err := writeLocalConfigurationFile(path, restart); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "projects:") || strings.Contains(string(raw), "model_selection:") {
		t.Fatalf("machine YAML contains project settings: %s", raw)
	}
	saved, err := readLocalConfigurationFile(path)
	if err != nil || len(saved.Projects) != 2 || saved.Global.Agents.ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("saved=%+v,%v", saved.Projects, err)
	}
	candidate := globalProjectCandidates(saved.Projects)
	if candidate[0].Rank != 0 || candidate[1].Rank != 1 {
		t.Fatalf("ranks=%+v", candidate)
	}
}
