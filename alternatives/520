package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// collectRunnerLocalChecks reuses doctor and the configured provider's local
// authentication probe. Raw details never cross the runner/Hub boundary.
func collectRunnerLocalChecks(ctx context.Context, cfg globalconfig.Config, name string, diagnose func(context.Context, doctorConfig) doctorReport, auth func(context.Context, workflowconfig.AgentBackend) bool, report ...func(runnerauth.LocalChecks)) runnerauth.LocalChecks {
	checks := runnerauth.LocalChecks{Checkout: "failed", Configuration: "pending", Doctor: "pending", Provider: "pending"}
	publish := func() {
		for _, emit := range report {
			emit(checks)
		}
	}
	defer publish()
	var selected globalconfig.Project
	for _, candidate := range cfg.Projects {
		if candidate.ID == name {
			selected = candidate
			break
		}
	}
	if selected.Workdir == "" {
		return checks
	}
	if _, err := os.Stat(filepath.Join(selected.Workdir, ".git")); err != nil {
		return checks
	}
	checks.Checkout, checks.CheckoutAt = "passed", time.Now().UTC()
	checks.Configuration = "running"
	publish()
	if _, err := os.Stat(selected.Workflow); selected.WorkflowRef == "" && errors.Is(err, os.ErrNotExist) {
		checks.Configuration, checks.ConfigurationAt = "missing", time.Now().UTC()
		return checks
	}
	_, workflow, _, err := resolveRunnerProjectPolicy(ctx, cfg, name)
	checks.ConfigurationAt = time.Now().UTC()
	if err != nil {
		checks.Configuration = "failed"
		checks.ConfigurationMessage = strings.Join(strings.Fields(regexp.MustCompile("\x60[^\x60]*\x60|\x22[^\x22]*\x22|\x27[^\x27]*\x27").ReplaceAllString(err.Error(), "[value]")), " ")
		if len(checks.ConfigurationMessage) > 8000 {
			checks.ConfigurationMessage = checks.ConfigurationMessage[:8000]
		}
		if strings.Contains(checks.ConfigurationMessage, "retired configuration key") {
			checks.ConfigurationFix = "Run detent config migrate detent.yaml in the repository, review and commit the changes, then retry."
		}
		if errors.Is(err, workflowconfig.ErrNoProjectDefinition) || errors.Is(err, os.ErrNotExist) {
			checks.Configuration = "missing"
		}
		return checks
	}
	checks.Configuration = "passed"
	checks.RunnerSetupDeclared = new(workflow.Config.Hooks.RunnerSetup != "")
	checks.Doctor = "running"
	publish()
	diagnosis := diagnose(ctx, doctorConfig{ConfigPath: cfg.Path, ProjectID: name, Output: io.Discard, CheckTimeout: doctorCheckTimeout, Flags: runtimeFlags{Port: runtimeIntFlag{Value: 0, Set: true}}})
	checks.Doctor = "passed"
	for _, check := range diagnosis.Checks {
		if check.Status == doctorFail {
			checks.Doctor = "failed"
			break
		}
		if check.Status == doctorWarn {
			checks.Doctor = "warning"
		}
	}
	checks.DoctorAt = time.Now().UTC()
	checks.Provider = "running"
	publish()
	checks.Provider = "passed"
	routes := workflow.Config.AgentRouteConfigs()
	for _, backend := range workflow.Config.AgentBackendConfigs() {
		if backend.Disabled || !slices.ContainsFunc(routes, func(route workflowconfig.AgentRoute) bool { return route.Backend == backend.ID }) {
			continue
		}
		if !slices.Contains(checks.ProviderKinds, backend.Kind) {
			checks.ProviderKinds = append(checks.ProviderKinds, backend.Kind)
		}
		if backend.Kind == workflowconfig.AgentBackendPiAgent || backend.Kind == workflowconfig.AgentBackendOpenCode {
			// These protocols have no authentication-status contract. Keep this unknown
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
	checks.ProviderAt = time.Now().UTC()
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
	case workflowconfig.AgentBackendAntigravity:
		return backend.AntigravityOptions().Home != "" && probeBackendSignIn(ctx, backend) == nil
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
		source := ""
		for _, selected := range cfg.Projects {
			if selected.ID == name && checks.Checkout == "passed" {
				source = selected.Workdir
				break
			}
		}
		report, err := workspace.ProbeCommitReport(ctx, source, workspace.CommitIdentity{Name: "Detent runner", Email: string(file.Identity.MachineID) + "@runners.detent.invalid"})
		if err != nil {
			return err
		}
		checks.CommitName, checks.CommitEmail, checks.CommitSigning = report.Identity.Name, report.Identity.Email, report.Signing
		native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(cfg.Client.NativeProjects[name]))
		if err != nil {
			return err
		}
		if err := native.HeartbeatMachine(ctx, hubclient.Machine{ID: file.Identity.MachineID, DisplayName: cfg.Client.DisplayName, Capacity: cfg.Client.Capacity, Version: firstNonBlankString(version, "dev"), LocalChecks: &checks, Problems: report.Problems()}); err != nil {
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
	for name, id := range cfg.Client.NativeProjects {
		native, err := client.Native(file.Identity.OrganizationID, tracker.ProjectID(id))
		if err != nil {
			return nil, err
		}
		checks := collectRunnerLocalChecks(ctx, cfg, name, func(ctx context.Context, cfg doctorConfig) doctorReport {
			return runDoctorStartupPreflight(ctx, cfg, options{}, doctorDeps{})
		}, probeRunnerProviderAuth)
		if checks.Configuration == "passed" {
			_, _, descriptor, err := resolveRunnerProjectPolicy(ctx, cfg, name)
			if err != nil {
				return nil, err
			}
			if err := native.ReportObservedPolicy(ctx, descriptor); err != nil {
				return nil, err
			}
		}
		reports[name] = checks
	}
	return reports, nil
}

// Registration must report missing checkout paths before the normal config
// loader can accept them. Keep the normal loader's expansion, defaults, and
// validation of everything except absent project paths.
func readRunnerSetupConfig(path string) (globalconfig.Config, error) {
	return globalconfig.Read(path, globalconfig.WithMissingProjectPaths())
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

func runnerProjectLocalChecks(ctx context.Context, cfg globalconfig.Config, name string, report ...func(runnerauth.LocalChecks)) runnerauth.LocalChecks {
	return collectRunnerLocalChecks(ctx, cfg, name, func(ctx context.Context, doctor doctorConfig) doctorReport {
		opts := options{readDoctorProject: func(string, string) (globalconfig.Config, []string, error) {
			resolved, _, _, err := resolveRunnerProjectPolicy(ctx, cfg, name)
			return resolved, nil, err
		}}
		return runDoctorStartupPreflight(ctx, doctor, opts, doctorDeps{})
	}, probeRunnerProviderAuth, report...)
}
