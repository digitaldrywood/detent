package cli

import (
	"context"
	"encoding/json"
	"errors"
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
func collectRunnerLocalChecks(ctx context.Context, cfg globalconfig.Config, name string, diagnose func(context.Context, doctorConfig) doctorReport, auth func(context.Context, workflowconfig.AgentBackend) bool) runnerauth.LocalChecks {
	checks := runnerauth.LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending"}
	resolved, workflow, _, err := resolveRunnerProjectPolicy(ctx, cfg, name)
	if err != nil || !runnerCheckoutReady(ctx, project.ManagerConfigFromGlobal(resolved).Projects[0]) {
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
		if backend.Kind == workflowconfig.AgentBackendPiAgent {
			// Pi RPC has no authentication-status contract. Keep this unknown
			// instead of reporting credentials as failed or authenticated.
			if checks.Provider == "passed" {
				checks.Provider = "pending"
			}
			continue
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

// reportRunnerSetup sends registration observations through the same negotiated
// heartbeat encoder used by the scheduler. Startup uses the scheduler's owner.
func reportRunnerSetup(ctx context.Context, cfg globalconfig.Config, version string) error {
	if cfg.Client.IdentityFile == "" || cfg.Path == "" {
		return nil
	}
	client, err := hubclient.New(hubclient.Config{URL: cfg.Client.URL, IdentityFile: cfg.Client.IdentityFile})
	if err != nil {
		return err
	}
	reports, err := collectRunnerSetupReports(ctx, cfg, client)
	if err != nil {
		return err
	}
	file, err := runnerauth.Load(cfg.Client.IdentityFile)
	if err != nil {
		return err
	}
	for name, checks := range reports {
		native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(cfg.Client.NativeProjects[name]))
		if err != nil {
			return err
		}
		if err := native.HeartbeatMachine(ctx, hubclient.Machine{ID: file.Identity.MachineID, DisplayName: cfg.Client.DisplayName, Capacity: cfg.Client.Capacity, Version: firstNonBlankString(version, "dev"), LocalChecks: &checks}); err != nil {
			return err
		}
	}
	return nil
}

// collectRunnerSetupReports preserves real startup observations without sending
// a second startup heartbeat or repeating local probes at each heartbeat tick.
func collectRunnerSetupReports(ctx context.Context, cfg globalconfig.Config, client *hubclient.Client) (map[string]runnerauth.LocalChecks, error) {
	if cfg.Client.IdentityFile == "" || cfg.Path == "" {
		return map[string]runnerauth.LocalChecks{}, nil
	}
	file, err := runnerauth.Load(cfg.Client.IdentityFile)
	if err != nil {
		return nil, err
	}
	reports := make(map[string]runnerauth.LocalChecks)
	for _, selected := range cfg.Projects {
		id := cfg.Client.NativeProjects[selected.ID]
		if id == "" {
			continue
		}
		native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(id))
		if err != nil {
			return nil, err
		}
		checks := collectRunnerLocalChecks(ctx, cfg, selected.ID, func(ctx context.Context, cfg doctorConfig) doctorReport {
			return runDoctorStartupPreflight(ctx, cfg, options{}, doctorDeps{})
		}, probeRunnerProviderAuth)
		if checks.Checkout == "passed" {
			_, _, descriptor, err := resolveRunnerSetupPolicy(ctx, cfg.Path, selected.ID)
			if err != nil {
				return nil, err
			}
			if err := native.ReportObservedPolicy(ctx, descriptor); err != nil {
				return nil, err
			}
		}
		reports[selected.ID] = checks
	}
	return reports, nil
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
	return resolveRunnerProjectPolicy(ctx, cfg, name)
}

func resolveRunnerProjectPolicy(ctx context.Context, cfg globalconfig.Config, name string) (globalconfig.Config, workflowconfig.Workflow, policy.Descriptor, error) {
	for _, selected := range cfg.Projects {
		if selected.ID == name {
			cfg.Projects = []globalconfig.Project{selected}
			break
		}
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].ID != name {
		return cfg, workflowconfig.Workflow{}, policy.Descriptor{}, errors.New("runner project is unavailable")
	}
	selected := project.ManagerConfigFromGlobal(cfg).Projects[0]
	workflow, err := project.LoadWorkflowContext(ctx, selected)
	if err != nil {
		return cfg, workflow, policy.Descriptor{}, err
	}
	descriptor, err := project.ResolvePolicy(selected, workflow)
	return cfg, workflow, descriptor, err
}

func runnerProjectLocalChecks(ctx context.Context, cfg globalconfig.Config, name string) runnerauth.LocalChecks {
	return collectRunnerLocalChecks(ctx, cfg, name, func(ctx context.Context, doctor doctorConfig) doctorReport {
		opts := options{readDoctorProject: func(string, string) (globalconfig.Config, []string, error) {
			resolved, _, _, err := resolveRunnerProjectPolicy(ctx, cfg, name)
			return resolved, nil, err
		}}
		return runDoctorStartupPreflight(ctx, doctor, opts, doctorDeps{})
	}, probeRunnerProviderAuth)
}
