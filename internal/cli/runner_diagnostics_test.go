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
		pi                                     bool
		checkout                               bool
		doctor                                 doctorStatus
		auth                                   bool
		wantCheckout, wantDoctor, wantProvider string
	}{
		{"missing checkout", false, false, doctorOK, true, "failed", "pending", "pending"},
		{"failed doctor", false, true, doctorFail, true, "passed", "failed", "passed"},
		{"missing provider", false, true, doctorOK, false, "passed", "passed", "failed"},
		{"success", false, true, doctorOK, true, "passed", "passed", "passed"},
		{"warnings", false, true, doctorWarn, true, "passed", "warning", "passed"},
		{"Pi auth unknown", true, true, doctorOK, false, "passed", "passed", "pending"},
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
				body := "---\ntracker:\n  kind: memory\n  repository: acme/orders\n"
				if tt.pi {
					body += "agents:\n  backends:\n    - id: pi\n      kind: pi_agent\n  routes:\n    - backend: pi\n      default: true\n"
				}
				body += "---\nPRIVATE WORKFLOW\n"
				if err := os.WriteFile(filepath.Join(root, "WORKFLOW.md"), []byte(body), 0600); err != nil {
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
			if tt.checkout && (doctors != 1 || !tt.pi && auths == 0 || tt.pi && auths != 0) {
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
	for _, test := range []struct {
		name              string
		homePaths         bool
		invalidWeight     bool
		workflowDirectory bool
		workdirFile       bool
		wantError         string
	}{
		{name: "absent checkout"},
		{name: "absent home-relative paths", homePaths: true},
		{name: "invalid config with absent checkout", invalidWeight: true, wantError: "weight: must be a positive integer"},
		{name: "workflow is a directory", workflowDirectory: true, wantError: "workflow: path does not exist"},
		{name: "workdir is a file", workdirFile: true, wantError: "workdir: path does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "identity.json"), workspaces: filepath.Join(root, "missing")}
			workdir := filepath.Join(paths.workspaces, "orders")
			workflow := filepath.Join(workdir, "WORKFLOW.md")
			config := runnerConfig("http://127.0.0.1:1", "org_test", "host", 1, paths, []runnerRegisteredCheck{{Name: "orders", ID: "prj_orders", Workdir: workdir}})
			if test.homePaths {
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(home, workdir)
				if err != nil {
					t.Fatal(err)
				}
				config.Projects[0].Workdir = "~/" + relative
				config.Projects[0].Workflow = "~/" + filepath.Join(relative, "WORKFLOW.md")
			}
			if test.invalidWeight {
				config.Projects[0].Weight = 0
			}
			if test.workflowDirectory {
				if err := os.MkdirAll(workflow, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.workdirFile {
				if err := os.MkdirAll(paths.workspaces, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(workdir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writeRunnerConfig(paths.config, config); err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerSetupConfig(paths.config)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Path != paths.config || len(cfg.Projects) != 1 || cfg.Client.NativeProjects["orders"] != "prj_orders" || cfg.Projects[0].Workdir != workdir || cfg.Projects[0].Workflow != workflow || cfg.Global.Cache.MaxAge == 0 {
				t.Fatalf("config=%+v", cfg)
			}
		})
	}
}
