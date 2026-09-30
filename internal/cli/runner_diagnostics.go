package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// collectRunnerLocalChecks reuses doctor and the configured provider's local
// authentication probe. Raw details never cross the runner/Hub boundary.
func collectRunnerLocalChecks(ctx context.Context, cfg globalconfig.Config, name, workdir string, diagnose func(context.Context, doctorConfig) doctorReport, auth func(context.Context, workflowconfig.AgentBackend) bool) runnerauth.LocalChecks {
	checks := runnerauth.LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending"}
	if !runnerCheckoutReady(workdir) {
		return checks
	}
	_, workflow, _, err := resolveRunnerSetupPolicy(ctx, cfg.Path, name)
	if err != nil {
		return checks
	}
	checks.Checkout = "passed"
	report := diagnose(ctx, doctorConfig{ConfigPath: cfg.Path, ProjectID: name, Output: io.Discard, CheckTimeout: doctorCheckTimeout, Flags: runtimeFlags{Port: runtimeIntFlag{Value: 0, Set: true}}})
	checks.Doctor = "passed"
	for _, check := range report.Checks {
		if check.Status == doctorFail {
			checks.Doctor = "failed"
			break
		}
		if check.Status == doctorWarn {
			checks.Doctor = "warning"
		}
	}
	checks.Provider = "passed"
	routes := workflow.Config.AgentRouteConfigs()
	for _, backend := range workflow.Config.AgentBackendConfigs() {
		if backend.Disabled || !slices.ContainsFunc(routes, func(route workflowconfig.AgentRoute) bool { return route.Backend == backend.ID }) {
			continue
		}
		if !slices.Contains(checks.ProviderKinds, backend.Kind) {
			checks.ProviderKinds = append(checks.ProviderKinds, backend.Kind)
		}
		probeCtx, cancel := context.WithTimeout(ctx, doctorCheckTimeout)
		signedIn := auth(probeCtx, backend)
		cancel()
		if !signedIn {
			checks.Provider = "failed"
		}
	}
	if len(checks.ProviderKinds) == 0 {
		checks.Provider = "pending"
	}
	return checks
}

func probeRunnerProviderAuth(ctx context.Context, backend workflowconfig.AgentBackend) bool {
	switch backend.Kind {
	case workflowconfig.AgentBackendCodex:
		account, err := probeDoctorCodexAccount(ctx, backend, nil)
		return err == nil && strings.TrimSpace(account.Type) != ""
	case workflowconfig.AgentBackendClaudeCode:
		// Run auth status under the configured command's shell and environment.
		// The response may contain account information; only loggedIn leaves here.
		cmd := commandshell.Command(ctx, backend.Command+" auth status --json", backend.ClaudeCodeOptions().Shell)
		cmd.Env = os.Environ()
		output, err := cmd.Output()
		if err != nil {
			return false
		}
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		return json.Unmarshal(output, &status) == nil && status.LoggedIn
	default:
		return false
	}
}

// reportRunnerSetup runs on registration and runner startup, including before
// policy approval. Restarting after a local fix reports new observations using
// the existing heartbeat; no diagnostics polling loop is introduced.
func reportRunnerSetup(ctx context.Context, cfg globalconfig.Config, version string) error {
	if cfg.Client.IdentityFile == "" || cfg.Path == "" {
		return nil
	}
	client, err := hubclient.New(hubclient.Config{URL: cfg.Client.URL, IdentityFile: cfg.Client.IdentityFile})
	if err != nil {
		return err
	}
	file, err := runnerauth.Load(cfg.Client.IdentityFile)
	if err != nil {
		return err
	}
	for _, selected := range cfg.Projects {
		id := cfg.Client.NativeProjects[selected.ID]
		if id == "" {
			continue
		}
		native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(id))
		if err != nil {
			return err
		}
		checks := collectRunnerLocalChecks(ctx, cfg, selected.ID, selected.Workdir, func(ctx context.Context, cfg doctorConfig) doctorReport {
			return runDoctor(ctx, cfg, options{}, doctorDeps{})
		}, probeRunnerProviderAuth)
		if checks.Checkout == "passed" {
			_, _, descriptor, err := resolveRunnerSetupPolicy(ctx, cfg.Path, selected.ID)
			if err != nil {
				return err
			}
			if err := native.ReportObservedPolicy(ctx, descriptor); err != nil {
				return err
			}
		}
		if err := native.HeartbeatMachine(ctx, hubclient.Machine{ID: file.Identity.MachineID, DisplayName: cfg.Client.DisplayName, Capacity: cfg.Client.Capacity, Version: firstNonBlankString(version, "dev"), LocalChecks: &checks}); err != nil {
			return err
		}
	}
	return nil
}

// Registration must report missing checkout paths before the normal config
// loader can accept them. Keep the normal loader's expansion, defaults, and
// validation of everything except absent project paths.
func readRunnerSetupConfig(path string) (globalconfig.Config, error) {
	return globalconfig.Read(path, globalconfig.WithMissingProjectPaths())
}

func resolveRunnerSetupPolicy(ctx context.Context, path, name string) (globalconfig.Config, workflowconfig.Workflow, policy.Descriptor, error) {
	cfg, _, err := globalconfig.ReadProject(path, name)
	if err != nil {
		return cfg, workflowconfig.Workflow{}, policy.Descriptor{}, err
	}
	selected := project.ManagerConfigFromGlobal(cfg).Projects[0]
	workflow, err := project.LoadWorkflowContext(ctx, selected)
	if err != nil {
		return cfg, workflow, policy.Descriptor{}, err
	}
	descriptor, err := project.ResolvePolicy(selected, workflow)
	return cfg, workflow, descriptor, err
}
