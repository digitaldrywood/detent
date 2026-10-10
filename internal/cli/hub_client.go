package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
)

type hubSchedulingOptions struct {
	applyProjects func(context.Context, globalconfig.Config) error
	setupStore    store.ProjectRunnerSetupStore
	logger        *slog.Logger
	runtimeConfig func() globalconfig.Config
	intakeToken   githubconnector.TokenSource
	problems      func() []runnerauth.Problem
}

func newHubScheduling(ctx context.Context, cfg globalconfig.Config, version string, options ...hubSchedulingOptions) (orchestrator.SchedulingSource, error) {
	clientConfig := cfg.Client
	if !clientConfig.Configured() {
		return nil, errors.New("hub client is not configured")
	}
	clientConfig = clientConfig.Normalized()
	token := strings.TrimSpace(os.Getenv(clientConfig.TokenEnvironment))
	if token == "" && clientConfig.IdentityFile == "" {
		return nil, errors.New("hub worker token environment variable " + clientConfig.TokenEnvironment + " is empty")
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	machineID := firstNonBlankString(clientConfig.MachineID, cfg.Global.Identity.Name, cfg.InstanceName, hostname)
	runnerID := machineID
	if clientConfig.IdentityFile != "" {
		file, err := runnerauth.Load(clientConfig.IdentityFile)
		if err != nil {
			return nil, err
		}
		if clientConfig.TokenEnvironment != "" || string(file.Identity.OrganizationID) != clientConfig.OrganizationID || clientConfig.MachineID != "" && clientConfig.MachineID != string(file.Identity.MachineID) {
			return nil, errors.New("hub runner configuration does not match the enrolled identity")
		}
		for _, id := range clientConfig.NativeProjects {
			if !slices.Contains(file.Identity.ProjectIDs, tracker.ProjectID(id)) {
				return nil, errors.New("hub project is outside runner enrollment")
			}
		}
		machineID = string(file.Identity.MachineID)
		runnerID = file.Identity.RunnerID
	}
	displayName := firstNonBlankString(clientConfig.DisplayName, cfg.Global.Identity.Name, cfg.InstanceName, machineID)
	capacity := clientConfig.Capacity
	if capacity <= 0 {
		capacity = cfg.Global.MaxConcurrentAgents
	}
	version = firstNonBlankString(version, "dev")
	client, err := hubclient.New(hubclient.Config{
		ArtifactServiceID: clientConfig.ArtifactServiceID,
		ArtifactBytes:     clientConfig.ArtifactBytes,
		URL:               clientConfig.URL,
		IdentityFile:      clientConfig.IdentityFile,
		TokenSource:       func() string { return os.Getenv(clientConfig.TokenEnvironment) },
		HTTPClient:        &http.Client{Timeout: clientConfig.RequestTimeout()},
	})
	if err != nil {
		return nil, err
	}
	nativeProjects := make(map[string]tracker.ProjectID, len(clientConfig.NativeProjects))
	for name, id := range clientConfig.NativeProjects {
		nativeProjects[name] = tracker.ProjectID(id)
	}
	checkouts := make(map[string]globalconfig.Project, len(nativeProjects))
	for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
		checkouts[selected.ID] = selected
	}
	var providerReports func() ([]providercapacity.Report, error)
	if clientConfig.ProviderCapacityFile != "" || clientConfig.IdentityFile != "" {
		providerReports = runnerProviderReports(ctx, cfg, firstNonBlankString(cfg.InstanceName, cfg.Global.Identity.Name, machineID), time.Now, runnerProviderModels)
	}
	var setupOptions hubSchedulingOptions
	if len(options) > 0 {
		setupOptions = options[0]
	}
	setup := project.NewRunnerSetup(runnerID, setupOptions.setupStore, setupOptions.logger)
	projects := &runnerProjects{client: client, cfg: cfg, setup: setup, logger: setupOptions.logger, checks: make(map[string]runnerauth.LocalChecks), apply: setupOptions.applyProjects, runtime: setupOptions.runtimeConfig}
	if projects.logger == nil {
		projects.logger = slog.Default()
	}
	prepareProject := projects.prepare
	setupResults := make(map[string]string, len(checkouts))
	for name := range checkouts {
		setupResults[name] = "passed"
		if err := prepareProject(ctx, name); err != nil {
			setupResults[name] = "failed"
		}
	}
	localChecks, err := collectRunnerSetupReports(ctx, cfg, client)
	if err != nil {
		return nil, err
	}
	for name, checks := range localChecks {
		checks.Setup = firstNonBlankString(setupResults[name], "failed")
		localChecks[name] = checks
		projects.checks[name] = checks
	}
	tokenSource := githubconnector.StaticTokenSource("")
	if len(options) > 0 && options[0].intakeToken != nil {
		tokenSource = options[0].intakeToken
	}
	github, err := githubconnector.NewClient(githubconnector.ClientConfig{TokenSource: tokenSource, HTTPClient: &http.Client{Timeout: clientConfig.RequestTimeout()}})
	if err != nil {
		return nil, err
	}
	var capacityConfiguration func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig
	if len(options) > 0 {
		capacityConfiguration = runnerCapacityOwner(cfg, options[0].runtimeConfig)
	}
	var reportProblems func() []runnerauth.Problem
	if len(options) > 0 {
		reportProblems = options[0].problems
	}
	return hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		PrepareProject:        prepareProject,
		RefreshProjects:       projects.refresh,
		RunnerSetupDeclared:   projects.runnerSetupDeclared,
		CapacityConfiguration: capacityConfiguration,
		LocalChecks:           localChecks,
		GitHubIntake:          github.FetchIssueSnapshot,
		GitHubDiscovery:       github.DiscoverIssues,
		Problems:              reportProblems,
		IsolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
			current := cfg
			if setupOptions.runtimeConfig != nil {
				current = setupOptions.runtimeConfig()
			}
			return probeRunnerIsolation(ctx, current)
		},
		ProviderReports: providerReports,
		OrganizationID:  tracker.OrganizationID(clientConfig.OrganizationID), NativeProjects: nativeProjects,
		CheckoutRepository: func(name string) string { return projects.repository(ctx, name) },
		Machine: hubclient.Machine{
			ID: tracker.MachineID(machineID), Hostname: hostname, DisplayName: displayName,
			Capabilities: hubMachineCapabilities(cfg), Capacity: capacity, Version: strings.TrimSpace(version),
			WorkspaceCapabilities: workspaceLaneCapabilities(context.Background(), cfg),
			WorkspaceIsolation:    workspacerunner.DefaultSupport().TerminalIsolation(),
		},
		HeartbeatInterval: min(clientConfig.HeartbeatInterval(), providercapacity.MaxAge/2),
		LeaseTTL:          clientConfig.LeaseTTL(),
	})
}

func runnerCheckoutRepository(ctx context.Context, selected globalconfig.Project) string {
	if !runnerCheckoutReady(ctx, selected) {
		return ""
	}
	remote, err := defaultGitRemoteURL(ctx, selected.Workdir)
	if err != nil {
		return ""
	}
	repository, _ := doctorGitHubRepositoryFromRemoteURL(remote)
	return repository
}

func newHubRunnerFleet(cfg globalconfig.Config) (*hubclient.FleetClient, error) {
	settings := cfg.Client.Normalized()
	client, err := hubclient.New(hubclient.Config{URL: settings.URL, IdentityFile: settings.IdentityFile,
		TokenSource: func() string { return os.Getenv(settings.TokenEnvironment) }, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
	if err != nil {
		return nil, err
	}
	projects := make(map[string]tracker.ProjectID, len(settings.NativeProjects))
	for name, id := range settings.NativeProjects {
		projects[name] = tracker.ProjectID(id)
	}
	return hubclient.NewFleetClient(client, tracker.OrganizationID(settings.OrganizationID), projects)
}

func hubMachineCapabilities(cfg globalconfig.Config) map[string]any {
	projects := make([]map[string]string, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		projects = append(projects, map[string]string{"id": project.ID, "pool": project.Pool})
	}
	pools := make([]map[string]any, 0, len(cfg.Global.AgentPools)+1)
	pools = append(pools, map[string]any{"name": "default", "capacity": cfg.Global.MaxConcurrentAgents})
	for _, pool := range cfg.Global.AgentPools {
		pools = append(pools, map[string]any{"name": pool.Name, "capacity": pool.MaxConcurrentAgents, "burst_to": pool.BurstTo})
	}
	return map[string]any{
		"projects": projects,
		"pools":    pools,
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
	}
}

func firstNonBlankString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func nativeProjectsFromConfig(projects map[string]string) map[string]tracker.ProjectID {
	ids := make(map[string]tracker.ProjectID, len(projects))
	for name, id := range projects {
		ids[name] = tracker.ProjectID(id)
	}
	return ids
}
