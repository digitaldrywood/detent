package web_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestMCPLocalProjectConfiguration(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	root := t.TempDir()
	path := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\ntracker:\n  kind: memory\n---\nPrivate instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithProjectPathLiterals())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []globalconfig.Project{{ID: "selected", Workflow: path, Workdir: root, Weight: 1, Paused: true}}
	if err := globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
		t.Fatal(err)
	}
	cfg, err = globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := project.NewManager(project.ManagerConfigFromGlobal(cfg), project.ManagerDependencies{Registry: deps.Registry, ProjectDependencies: project.Dependencies{WorkAttempts: deps.Store}})
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
	handoffs := 0
	deps.ProjectConfigOwner = project.NewConfigurationOwner(cfg, func() globalconfig.Config { return cfg }, manager, deps.Store, func(context.Context, globalconfig.Config, string, string, string) error { handoffs++; return nil })
	server := fleetTestServer(t, deps)
	_, raw := fleetCall(t, server, "detent_admin_token", operatortool.LocalProjectConfiguration, map[string]any{"project_id": "selected"})
	var view project.ManagedConfigView
	if err := json.Unmarshal(raw, &view); err != nil || view.Constraint != "" || view.EffectivePolicy == nil {
		t.Fatalf("view=%s error=%v", raw, err)
	}
	for _, private := range []string{root, "Private instructions"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private provenance returned")
		}
	}
	args := map[string]any{"request_id": "detach-once", "project_id": "selected", "expected_config_revision": view.ConfigRevision, "expected_policy_id": view.EffectivePolicy.ID, "checkpoint": strings.Repeat("a", 64)}
	readToken, _ := createRemoteMCPKey(t, server, "read only", []string{"read"}, []string{"selected"})
	response, denied := fleetCall(t, server, readToken, "detach_local_project", args)
	if len(denied) != 0 || handoffs != 0 {
		t.Fatalf("read-only mutation=%s", response.Body.String())
	}
	scoped, _ := createRemoteMCPKey(t, server, "scoped", []string{"read", "admin"}, []string{"selected"})
	_, denied = fleetCall(t, server, scoped, operatortool.LocalProjectConfiguration, map[string]any{"project_id": "foreign"})
	if len(denied) != 0 {
		t.Fatal("cross-project read accepted")
	}
	_, raw = fleetCall(t, server, "detent_admin_token", "detach_local_project", args)
	var action struct {
		ID         string         `json:"action_id"`
		Connection string         `json:"connection_id"`
		Status     string         `json:"status"`
		Preview    chatpkg.Action `json:"preview"`
	}
	if err := json.Unmarshal(raw, &action); err != nil || action.Status != "pending" || handoffs != 0 {
		t.Fatalf("approval bypassed=%s error=%v", raw, err)
	}
	fleetDecision(t, server, action.Connection, action.ID, "confirm", "")
	_, raw = fleetCall(t, server, "detent_admin_token", operatortool.ActionResult, map[string]any{"action_id": action.ID})
	if err := json.Unmarshal(raw, &action); err != nil || action.Status != "succeeded" || handoffs != 1 {
		t.Fatalf("execution=%s handoffs=%d error=%v", raw, handoffs, err)
	}
	if !strings.Contains(action.Preview.Result, `"saved":true`) || !strings.Contains(action.Preview.Result, `"applied":false`) {
		t.Fatalf("false detach receipt=%s", raw)
	}
	_, replay := fleetCall(t, server, "detent_admin_token", "detach_local_project", args)
	if len(replay) == 0 || handoffs != 1 {
		t.Fatalf("repeated effect=%s handoffs=%d", replay, handoffs)
	}
	restarted := fleetTestServer(t, deps)
	_, durable := fleetCall(t, restarted, "detent_admin_token", "detach_local_project", args)
	if !strings.Contains(string(durable), `"configuration_receipt"`) || !strings.Contains(string(durable), `"saved":true`) || !strings.Contains(string(durable), `"applied":false`) || handoffs != 1 {
		t.Fatalf("durable retry=%s handoffs=%d", durable, handoffs)
	}
	args["checkpoint"] = strings.Repeat("b", 64)
	_, conflict := fleetCall(t, server, "detent_admin_token", "detach_local_project", args)
	if len(conflict) != 0 || handoffs != 1 {
		t.Fatal("changed retry identity accepted")
	}
	missing := fleetTestServer(t, testDeps(t))
	_, raw = fleetCall(t, missing, "detent_admin_token", operatortool.LocalProjectConfiguration, map[string]any{"project_id": "selected"})
	if !strings.Contains(string(raw), "not installed or is stopped") || strings.Contains(string(raw), `"applied":true`) {
		t.Fatalf("missing owner=%s", raw)
	}
}
