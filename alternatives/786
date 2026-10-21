package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerProviderReports(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, alias       string
		file, missing, fail, usage bool
		billing                    string
		billingFail                bool
	}{
		{name: "default and refresh", account: "macbook-pro-m1", alias: "macbook-pro-m1"},
		{name: "normalized instance", account: "Owner's MacBook", alias: "owner-s-macbook"},
		{name: "bounded instance alias", account: strings.Repeat("a", 80), alias: strings.Repeat("a", 64)},
		{name: "explicit file precedence", file: true},
		{name: "observed default", account: "runner", alias: "runner", usage: true},
		{name: "observed explicit file", file: true, usage: true},
		{name: "native subscription", account: "runner", alias: "runner", billing: "subscription"},
		{name: "native API key with explicit file", file: true, billing: "api_key"},
		{name: "native subscription with explicit file", file: true, billing: "subscription"},
		{name: "unavailable native auth", file: true, billingFail: true},
		{name: "explicit missing file does not fall back", file: true, missing: true},
		{name: "catalog failure remains instance owned", fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
			cfg := globalconfig.Config{Global: globalconfig.Settings{MaxConcurrentAgents: 4}}
			explicit := providercapacity.Report{Billing: "unknown", Provider: "openai", Backend: "codex", AccountAlias: "example-runner", Models: []string{"configured-model"}, MaxConcurrent: 12, Availability: "available", ObservedAt: now}
			if test.file {
				cfg.Client.ProviderCapacityFile = filepath.Join(t.TempDir(), "capacity.json")
				if !test.missing {
					raw, err := json.Marshal([]providercapacity.Report{explicit})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(cfg.Client.ProviderCapacityFile, raw, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			catalogFailure := errors.New("catalog unavailable")
			var usage *runner.ProviderUsage
			observed := now.Add(-time.Hour)
			percent := 85.0
			if test.usage {
				usage = &runner.ProviderUsage{}
				usage.ObserveBilling("codex", "subscription")
				explicit.Billing = "subscription"
				usage.Observe("codex", &telemetry.RateLimits{Primary: &telemetry.RateLimitBucket{WindowKind: "five_hour", PercentUsed: &percent, ObservedAt: &observed}})
				explicit.UsageWindows = []providercapacity.UsageWindow{{Kind: "five_hour", PercentUsed: &percent, ObservedAt: observed}}
			}
			var billing func(context.Context, workflowconfig.AgentBackend) (string, error)
			if test.billing != "" || test.billingFail {
				billing = func(_ context.Context, backend workflowconfig.AgentBackend) (string, error) {
					if backend.ID != "codex" {
						t.Fatalf("billing backend = %q", backend.ID)
					}
					if test.billingFail {
						return "subscription", errors.New("native auth unavailable")
					}
					return test.billing, nil
				}
				if !test.billingFail {
					explicit.Billing = test.billing
				}
			}
			source := runnerProviderReports(t.Context(), func() globalconfig.Config { return cfg }, test.account, func() time.Time { return now }, func(_ context.Context, backend workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
				if test.file {
					t.Fatal("explicit file invoked the default reporter")
				}
				if backend.ID != "codex" || backend.Command != "codex app-server" {
					t.Fatalf("backend=%+v", backend)
				}
				if test.fail {
					return nil, catalogFailure
				}
				return []runner.AgentModel{{ID: "sol", Model: "sol"}, {ID: "astra", Model: "astra"}}, nil
			}, billing, usage)
			for range 6 {
				reports, err := source()
				if test.missing || test.fail {
					if err == nil || test.fail && (!errors.Is(err, catalogFailure) || !errors.Is(err, providercapacity.ErrCatalogUnavailable)) {
						t.Fatalf("source error=%v", err)
					}
					return
				}
				if err != nil || len(reports) != 1 {
					t.Fatalf("reports=%+v, err=%v", reports, err)
				}
				if test.billing != "" && reports[0].Billing != test.billing {
					t.Fatalf("billing = %q, want %q", reports[0].Billing, test.billing)
				}
				if test.billingFail && reports[0].Billing != "unknown" {
					t.Fatalf("failed auth billing = %q", reports[0].Billing)
				}
				if test.usage && (reports[0].Billing != "subscription" || len(reports[0].UsageWindows) != 1 || reports[0].UsageWindows[0].PercentUsed == nil || *reports[0].UsageWindows[0].PercentUsed != 85 || !reports[0].UsageWindows[0].ObservedAt.Equal(observed)) {
					t.Fatalf("usage changed during heartbeat: %+v", reports[0].UsageWindows)
				}
				if test.file {
					if !reflect.DeepEqual(reports[0], explicit) {
						t.Fatalf("explicit report replaced: %+v", reports)
					}
				} else {
					report := reports[0]
					if report.Provider != "openai" || report.Backend != "codex" || report.AccountAlias != test.alias || report.MaxConcurrent != 0 || report.Availability != "unknown" || !report.ObservedAt.Equal(now) || !reflect.DeepEqual(report.Models, []string{"sol", "astra", "provider_default"}) {
						t.Fatalf("default report=%+v", report)
					}
					raw, err := json.Marshal(report)
					if err != nil || strings.Contains(string(raw), "max_concurrent") {
						t.Fatalf("default account limit was serialized: %s, %v", raw, err)
					}
				}
				now = now.Add(30 * time.Second)
			}
		})
	}
}

func TestRunnerProviderProjectReports(t *testing.T) {
	t.Parallel()
	for _, catalog := range []bool{false, true} {
		t.Run(fmt.Sprintf("catalog=%t", catalog), func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Config{Client: globalconfig.HubClient{IdentityFile: "enrolled"}}
			write := func(name, backend, kind, model string) globalconfig.Project {
				t.Helper()
				path := filepath.Join(t.TempDir(), "WORKFLOW.md")
				raw := fmt.Sprintf("---\nagents:\n  backends:\n    - id: %s\n      kind: %s\n  routes:\n    - name: default\n      default: true\n      backend: %s\n      model: %s\n---\nWork on the issue.\n", backend, kind, backend, model)
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				return globalconfig.Project{ID: name, Workflow: path}
			}
			cfg.Projects = []globalconfig.Project{write("code", "codex-code", "codex", "sol"), write("design", "claude-design", "claude_code", "opus")}
			projects := &runnerProjects{cfg: cfg, runtime: func() globalconfig.Config { return cfg }}
			now := time.Now().UTC()
			failedBackend := ""
			const incidentCommand = "codex --config shell_environment_policy.inherit=all --config model_reasoning_effort=low app-server"
			otherCommand := incidentCommand
			otherModel := "sol"
			source := runnerProviderReports(t.Context(), projects.providerConfiguration, "runner", func() time.Time { return now }, func(_ context.Context, backend workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
				if backend.ID == failedBackend {
					return nil, errors.New("catalog unavailable")
				}
				if backend.ID == "codex-code" && backend.CodexOptions().TurnTimeoutMS == 1000 && backend.Command != otherCommand {
					t.Fatalf("project execution command changed: %q", backend.Command)
				}
				if !catalog {
					return nil, nil
				}
				if backend.ID == "codex-code" {
					if backend.CodexOptions().TurnTimeoutMS == 1000 {
						return []runner.AgentModel{{ID: "astra", Model: "other-code-model"}}, nil
					}
					return []runner.AgentModel{{ID: "sol", Model: "code-model"}}, nil
				}
				return []runner.AgentModel{{ID: "opus", Model: "design-model"}}, nil
			}, nil)
			workspaces, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			check := func(designModel string, codeModels ...string) {
				t.Helper()
				reports, err := source()
				if err != nil || len(reports) != 2 {
					t.Fatalf("project reports=%+v, err=%v", reports, err)
				}
				if err := providercapacity.Validate(reports); err != nil {
					t.Fatalf("invalid project reports: %v", err)
				}
				for i, expected := range []struct{ backend, provider, model string }{{"codex-code", "openai", "sol"}, {"claude-design", "anthropic", designModel}} {
					index := slices.IndexFunc(reports, func(report providercapacity.Report) bool { return report.Backend == expected.backend })
					if index < 0 {
						t.Fatalf("missing backend %s: %+v", expected.backend, reports)
					}
					report := reports[index]
					codePresent := slices.ContainsFunc(projects.cfg.Projects, func(selected globalconfig.Project) bool { return selected.ID == "code" })
					if i == 0 && !codePresent {
						expected.model = "astra"
					}
					want := []string{expected.model, "provider_default"}
					if catalog {
						want = []string{expected.model, []string{"code-model", "design-model"}[i], "provider_default"}
						if i == 1 && designModel != "opus" {
							want = []string{"opus", "design-model", designModel, "provider_default"}
						}
						if i == 0 && !codePresent {
							want = []string{"astra", "other-code-model", "provider_default"}
						}
					}
					if i == 0 {
						want = append(want, codeModels...)
					}
					slices.Sort(want)
					got := slices.Clone(report.Models)
					slices.Sort(got)
					if report.Backend != expected.backend || report.Provider != expected.provider || report.Availability != "unknown" || report.MaxConcurrent != 0 || !slices.Equal(got, want) {
						t.Fatalf("project report=%+v, want backend=%s models=%v", report, expected.backend, want)
					}
				}
				for _, selected := range project.ManagerConfigFromGlobal(projects.providerConfiguration()).Projects {
					workflow, err := project.LoadWorkflowContext(t.Context(), selected)
					if err != nil {
						t.Fatal(err)
					}
					execution, err := runner.NewRunner(runner.Dependencies{Workflow: workflow, Workspace: workspaces, AgentBackendFactory: runner.AgentBackendFactoryFunc(buildAgentBackend)})
					if err != nil {
						t.Fatal(err)
					}
					requirement, err := execution.DispatchCapacity(t.Context(), runner.RunRequest{ProviderReports: reports})
					wantBackend, wantModel := "codex-code", "sol"
					switch selected.ID {
					case "design":
						wantBackend, wantModel = "claude-design", designModel
					case "other-code":
						wantModel = otherModel
					}
					if err != nil || requirement.Backend != wantBackend || requirement.Model != wantModel || !slices.ContainsFunc(reports, func(report providercapacity.Report) bool { return report.Supports(requirement) }) {
						t.Fatalf("project %s dispatch=%+v, err=%v", selected.ID, requirement, err)
					}
				}
			}
			check("opus")
			for _, failed := range []string{"codex-code", "claude-design"} {
				failedBackend = failed
				reports, err := source()
				healthy := providercapacity.Requirement{Role: runner.RoleCode, Backend: "codex-code", Model: "sol"}
				provider := "openai"
				if failed == "codex-code" {
					healthy.Backend, healthy.Model, provider = "claude-design", "opus", "anthropic"
				}
				if err != nil || len(reports) != 1 || !reports[0].Supports(healthy) || reports[0].Provider != provider || reports[0].AccountAlias != "runner" || reports[0].Availability != "unknown" {
					t.Fatalf("healthy report lost after %s catalog failure: reports=%+v err=%v", failed, reports, err)
				}
				if reports[0].Backend == failed {
					t.Fatalf("failed backend remained claimable: %+v", reports)
				}
			}
			failedBackend = ""
			check("opus")
			raw, err := os.ReadFile(cfg.Projects[1].Workflow)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.Projects[1].Workflow, []byte(strings.ReplaceAll(string(raw), "model: opus", "model: sonnet")), 0o600); err != nil {
				t.Fatal(err)
			}
			check("sonnet")
			cfg.Projects = append([]globalconfig.Project(nil), cfg.Projects...)
			cfg.Projects[1] = write("design", "claude-design", "claude_code", "haiku")
			check("haiku")
			projects.cfg.Projects = append(projects.cfg.Projects, write("other-code", "codex-code", "codex", "sol"))
			check("haiku")
			other := &projects.cfg.Projects[2]
			raw, err = os.ReadFile(other.Workflow)
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.ReplaceAll(string(raw), "kind: codex", "kind: codex\n      command: codex --config shell_environment_policy.inherit=all --config model_reasoning_effort=low app-server\n      options:\n        turn_timeout_ms: 1000"))
			raw = []byte(strings.ReplaceAll(string(raw), "model: sol", "model: astra"))
			otherModel = "astra"
			if err := os.WriteFile(other.Workflow, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if catalog {
				check("haiku", "astra", "other-code-model")
			} else {
				check("haiku", "astra")
			}
			selected := slices.Clone(projects.cfg.Projects)
			for _, command := range []string{incidentCommand, "codex -c model_reasoning_effort=high --config shell_environment_policy.inherit=all app-server"} {
				otherCommand = command
				if err := os.WriteFile(other.Workflow, []byte(strings.ReplaceAll(string(raw), incidentCommand, command)), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, order := range [][]globalconfig.Project{selected, {selected[2], selected[1], selected[0]}, {selected[0], selected[1]}, {selected[2], selected[1]}} {
					projects.cfg.Projects = slices.Clone(order)
					if len(order) == 3 && catalog {
						check("haiku", "astra", "other-code-model")
					} else if len(order) == 3 {
						check("haiku", "astra")
					} else {
						check("haiku")
					}
				}
			}
			projects.cfg.Projects = selected
			other = &projects.cfg.Projects[2]
			for _, conflict := range []struct{ name, from, to string }{
				{"provider", "kind: codex", "kind: codex\n      provider: other-provider"},
				{"account command", incidentCommand, "env CODEX_HOME=/example/other codex app-server"},
				{"account config", incidentCommand, "codex --config forced_chatgpt_account_id=other app-server"},
				{"provider config", incidentCommand, "codex --config model_provider=other app-server"},
				{"profile", incidentCommand, "codex --profile other app-server"},
				{"wrapper", incidentCommand, "other-codex --config model_reasoning_effort=low app-server"},
				{"shell command", incidentCommand, "codex --config model_reasoning_effort=low app-server; other-command"},
				{"shell newline", "command: " + incidentCommand, "command: \"codex\\n--config model_reasoning_effort=low app-server\""},
				{"unknown config", incidentCommand, "codex --config other_option=low app-server"},
				{"incomplete config", incidentCommand, "codex --config app-server"},
				{"model provider", "turn_timeout_ms: 1000", "turn_timeout_ms: 1000\n        model_provider: other-provider"},
				{"shell", "turn_timeout_ms: 1000", "turn_timeout_ms: 1000\n        shell: other-shell"},
				{"backend kind", "kind: codex\n      command: " + incidentCommand + "\n      options:\n        turn_timeout_ms: 1000", "kind: claude_code"},
			} {
				t.Run(conflict.name, func(t *testing.T) {
					if err := os.WriteFile(other.Workflow, []byte(strings.ReplaceAll(string(raw), conflict.from, conflict.to)), 0o600); err != nil {
						t.Fatal(err)
					}
					reports, err := source()
					if err != nil || len(reports) != 1 || reports[0].Backend != "claude-design" || reports[0].Provider != "anthropic" || !slices.Contains(reports[0].Models, "haiku") || reports[0].Availability != "unknown" {
						t.Fatalf("unrelated report after identity conflict=%+v, err=%v", reports, err)
					}
					if err := providercapacity.Validate(reports); err != nil {
						t.Fatal(err)
					}
				})
			}
			cfg.Client.ProviderCapacityFile = writeProviderCapacityFixture(t, t.TempDir())
			reports, err := source()
			if err != nil || len(reports) != 1 || reports[0].Backend != "codex" || !reflect.DeepEqual(reports[0].Models, []string{"provider_default"}) {
				t.Fatalf("explicit report after reload=%+v, err=%v", reports, err)
			}
		})
	}
}

func TestRunnerProviderProtocolIdentity(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"app-server", "headless", "rpc"} {
		t.Run(protocol, func(t *testing.T) {
			a := workflowconfig.AgentBackend{Kind: workflowconfig.AgentBackendCodex, Command: "codex app-server", Protocol: "app-server"}
			b := a
			b.Protocol = protocol
			if got := runnerProviderCompatible(a, b); got != (protocol == "app-server") {
				t.Fatalf("backend protocols %s and %s compatible=%t", a.Protocol, b.Protocol, got)
			}
		})
	}
}

func writeProviderCapacityFixture(t *testing.T, dir string) string {
	t.Helper()
	raw, err := json.Marshal([]providercapacity.Report{{Provider: "openai", Backend: "codex", AccountAlias: "runner", Models: []string{"provider_default"}, Availability: "unknown", ObservedAt: time.Now().UTC()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider-capacity.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPIKeyReportSuppressesQuota(t *testing.T) {
	t.Parallel()
	usage := &runner.ProviderUsage{}
	usage.ObserveBilling("codex", "api_key")
	reports := withProviderUsage([]providercapacity.Report{{Backend: "codex", UsageWindows: []providercapacity.UsageWindow{{Kind: "weekly"}}}}, []*runner.ProviderUsage{usage})
	if reports[0].Billing != "api_key" || len(reports[0].UsageWindows) != 0 {
		t.Fatalf("API report = %+v", reports[0])
	}
}
