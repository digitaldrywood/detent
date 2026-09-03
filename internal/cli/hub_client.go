package cli

import (
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubScheduling(cfg globalconfig.Config, version string) (orchestrator.SchedulingSource, error) {
	clientConfig := cfg.Client
	if !clientConfig.Configured() {
		return nil, errors.New("hub client is not configured")
	}
	clientConfig = clientConfig.Normalized()
	token := strings.TrimSpace(os.Getenv(clientConfig.TokenEnvironment))
	if token == "" {
		return nil, errors.New("hub worker token environment variable " + clientConfig.TokenEnvironment + " is empty")
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	machineID := firstNonBlankString(clientConfig.MachineID, cfg.Global.Identity.Name, cfg.InstanceName, hostname)
	displayName := firstNonBlankString(clientConfig.DisplayName, cfg.Global.Identity.Name, cfg.InstanceName, machineID)
	capacity := clientConfig.Capacity
	if capacity <= 0 {
		capacity = cfg.Global.MaxConcurrentAgents
	}
	version = firstNonBlankString(version, "dev")
	client, err := hubclient.New(hubclient.Config{
		URL:         clientConfig.URL,
		TokenSource: func() string { return os.Getenv(clientConfig.TokenEnvironment) },
		HTTPClient:  &http.Client{Timeout: clientConfig.RequestTimeout()},
	})
	if err != nil {
		return nil, err
	}
	return hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		Machine: hubclient.Machine{
			ID: tracker.MachineID(machineID), Hostname: hostname, DisplayName: displayName,
			Capabilities: hubMachineCapabilities(cfg), Capacity: capacity, Version: strings.TrimSpace(version),
		},
		HeartbeatInterval: clientConfig.HeartbeatInterval(),
		LeaseTTL:          clientConfig.LeaseTTL(),
	})
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
