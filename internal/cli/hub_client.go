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

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func newHubScheduling(cfg globalconfig.Config, version string) (orchestrator.SchedulingSource, error) {
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
	if clientConfig.IdentityFile != "" {
		file, err := runnerauth.Load(clientConfig.IdentityFile)
		if err != nil {
			return nil, err
		}
		if clientConfig.TokenEnvironment != "" || len(clientConfig.NativeProjects) == 0 || string(file.Identity.OrganizationID) != clientConfig.OrganizationID || clientConfig.MachineID != "" && clientConfig.MachineID != string(file.Identity.MachineID) {
			return nil, errors.New("hub runner configuration does not match the enrolled identity")
		}
		for _, id := range clientConfig.NativeProjects {
			if !slices.Contains(file.Identity.ProjectIDs, tracker.ProjectID(id)) {
				return nil, errors.New("hub project is outside runner enrollment")
			}
		}
		machineID = string(file.Identity.MachineID)
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
	var providerReports func() ([]providercapacity.Report, error)
	if clientConfig.ProviderCapacityFile != "" {
		// The collector's file names the models; the backends this runner
		// dispatches on know what each model can do. The catalogue read is
		// cached and runs off the claim path, so a report is published on
		// time whether or not the catalogue has been read yet.
		catalog := newProviderModelCatalog(func(ctx context.Context) (map[string]backendModelCatalog, error) {
			return configuredBackendModelCatalogs(ctx, cfg)
		}, slog.Default())
		providerReports = func() ([]providercapacity.Report, error) {
			reports, err := providercapacity.Load(clientConfig.ProviderCapacityFile)
			if err != nil {
				return nil, err
			}
			return catalog.decorate(reports), nil
		}
	}
	return hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		ProviderReports: providerReports,
		OrganizationID:  tracker.OrganizationID(clientConfig.OrganizationID), NativeProjects: nativeProjects,
		Machine: hubclient.Machine{
			ID: tracker.MachineID(machineID), Hostname: hostname, DisplayName: displayName,
			Capabilities: hubMachineCapabilities(cfg), Capacity: capacity, Version: strings.TrimSpace(version),
			// What this runner can serve for a workspace session (decisions
			// section 18.1). The hub's claim gate reads it from the row it
			// stamps the heartbeat on, so a runner that never reports it is
			// never offered a workspace item, and the lane below is started
			// under exactly the same condition. Isolation is `user` because
			// every channel this runner serves -- files, exec, git and now the
			// terminal -- runs as the runner's own account: this repository
			// ships no container runtime hook, and section 18.3 says a runner
			// that cannot provide container reports user and lets the card stay
			// disabled for organizations that require the other level.
			WorkspaceCapabilities: workspacerunner.Capabilities(workspacerunner.DefaultSupport()),
			WorkspaceIsolation:    workspacesession.IsolationUser,
		},
		HeartbeatInterval: clientConfig.HeartbeatInterval(),
		LeaseTTL:          clientConfig.LeaseTTL(),
	})
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
