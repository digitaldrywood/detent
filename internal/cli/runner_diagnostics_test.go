package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCollectRunnerLocalChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                                   string
		checkout                               bool
		doctor                                 doctorStatus
		auth                                   bool
		wantCheckout, wantDoctor, wantProvider string
	}{
		{"missing checkout", false, doctorOK, true, "failed", "pending", "pending"},
		{"failed doctor", true, doctorFail, true, "passed", "failed", "passed"},
		{"missing provider", true, doctorOK, false, "passed", "passed", "failed"},
		{"success", true, doctorOK, true, "passed", "passed", "passed"},
		{"warnings", true, doctorWarn, true, "passed", "warning", "passed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			cfg, err := globalconfig.DefaultAt(path, globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Path = path
			cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: filepath.Join(root, "WORKFLOW.md"), Workdir: root, Weight: 1}}
			if err := globalconfig.Write(path, cfg, globalconfig.WithMissingWorkflowFiles()); err != nil {
				t.Fatal(err)
			}
			if tt.checkout {
				if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "WORKFLOW.md"), []byte("---\ntracker:\n  kind: memory\n  repository: acme/orders\n---\nPRIVATE WORKFLOW\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var doctors, auths int
			checks := collectRunnerLocalChecks(t.Context(), cfg, "orders", root, func(_ context.Context, c doctorConfig) doctorReport {
				doctors++
				if c.ProjectID != "orders" || c.ConfigPath != path || c.AllowWriteProbes || c.Flags.Port.Value != 0 {
					t.Fatalf("wrong doctor context: %+v", c)
				}
				return doctorReport{Checks: []doctorCheck{{Status: tt.doctor, Detail: "secret-token PRIVATE WORKFLOW", Hint: "secret-hint"}}}
			}, func(_ context.Context, b workflowconfig.AgentBackend) bool { auths++; return tt.auth })
			if checks.Checkout != tt.wantCheckout || checks.Doctor != tt.wantDoctor || checks.Provider != tt.wantProvider {
				t.Fatalf("checks=%+v", checks)
			}
			if tt.checkout && (doctors != 1 || auths == 0) {
				t.Fatalf("probes doctor=%d auth=%d", doctors, auths)
			}
			if !tt.checkout && (doctors != 0 || auths != 0) {
				t.Fatal("probed before checkout")
			}
			encoded, err := json.Marshal(checks)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "PRIVATE") {
				t.Fatalf("private output escaped: %s", encoded)
			}
		})
	}
}

func TestReadRunnerSetupConfigMissingCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "identity.json"), workspaces: filepath.Join(root, "missing")}
	config := runnerConfig("http://127.0.0.1:1", "org_test", "host", 1, paths, []runnerRegisteredCheck{{Name: "orders", ID: "prj_orders", Workdir: filepath.Join(paths.workspaces, "orders")}})
	if _, err := writeRunnerConfig(paths.config, config); err != nil {
		t.Fatal(err)
	}
	cfg, err := readRunnerSetupConfig(paths.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != paths.config || len(cfg.Projects) != 1 || cfg.Client.NativeProjects["orders"] != "prj_orders" {
		t.Fatalf("config=%+v", cfg)
	}
}
