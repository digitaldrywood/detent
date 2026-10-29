package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func providerServicePath(cfg globalconfig.Config, base, home string) (string, error) {
	workflow := workflowconfig.Default().WithAgentDefaults(cfg.Global.Agents, cfg.Global.Budget)
	path := base
	for _, backend := range workflow.AgentBackendConfigs() {
		if backend.Disabled {
			continue
		}
		fields := doctorWorkflowCommandFields(backend.Command)
		if len(fields) == 0 {
			return "", fmt.Errorf("provider CLI for backend %s is empty; configure its command before installing the service", backend.ID)
		}
		binary := fields[0]
		directories := filepath.SplitList(path)
		candidates := append(slices.Clone(directories), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"))
		found := ""
		if filepath.IsAbs(binary) {
			candidates = []string{filepath.Dir(binary)}
			binary = filepath.Base(binary)
		}
		for _, directory := range candidates {
			if !filepath.IsAbs(directory) {
				continue
			}
			info, err := os.Stat(filepath.Join(directory, binary))
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				found = directory
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("provider CLI %q for backend %s is missing from the service PATH; install it and export PATH with its bin directory, then retry runner service registration with detent hub runner register --service", binary, backend.ID)
		}
		if !slices.Contains(directories, found) {
			if path != "" {
				path += string(os.PathListSeparator)
			}
			path += found
		}
	}
	return path, nil
}

type runnerHeartbeatService struct {
	ServiceRunner
	cfg  globalconfig.Config
	read func(context.Context) (runnerauth.Fleet, error)
	id   string
}

func newRunnerHeartbeatService(runner ServiceRunner, cfg globalconfig.Config) (ServiceRunner, error) {
	client, err := hubclient.New(hubclient.Config{URL: cfg.Client.URL, IdentityFile: cfg.Client.IdentityFile})
	if err != nil {
		return nil, err
	}
	identity, err := runnerauth.Load(cfg.Client.IdentityFile)
	if err != nil {
		return nil, err
	}
	fleet, err := hubclient.NewFleetClient(client, identity.Identity.OrganizationID, nil)
	if err != nil {
		return nil, err
	}
	return runnerHeartbeatService{ServiceRunner: runner, cfg: cfg, read: fleet.Fleet, id: identity.Identity.RunnerID}, nil
}

func (r runnerHeartbeatService) Start(ctx context.Context, opts servicepkg.StartOptions) (servicepkg.StartResult, error) {
	before, err := r.read(ctx)
	if err != nil {
		return servicepkg.StartResult{}, fmt.Errorf("read runner before service start: %w", err)
	}
	var heartbeat time.Time
	for _, runner := range before.Runners {
		if runner.RunnerID == r.id {
			heartbeat = runner.LastHeartbeatAt
		}
	}
	result, err := r.ServiceRunner.Start(ctx, opts)
	if err != nil || result.Action == servicepkg.ActionNeedsInstall || result.Manager.Name == servicepkg.ManagerManual {
		return result, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := waitRunnerProviderHeartbeat(checkCtx, r.read, r.id, heartbeat); err != nil {
		return result, fmt.Errorf("runner service started but readiness check failed: %w; check provider CLI installation, sign-in and service PATH, and logs in %s; after fixing, restart the runner service for %s", err, filepath.Join(filepath.Dir(r.cfg.Path), "logs"), shellQuote(r.cfg.Path))
	}
	return result, nil
}

func waitRunnerProviderHeartbeat(ctx context.Context, read func(context.Context) (runnerauth.Fleet, error), id string, after time.Time) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := errors.New("no new runner heartbeat with a provider was reported to the Hub")
	for {
		fleet, err := read(ctx)
		if err != nil {
			return fmt.Errorf("read runner heartbeat: %w", err)
		}
		for _, runner := range fleet.Runners {
			if runner.RunnerID != id {
				continue
			}
			if runner.LastHeartbeatAt.After(after) && len(runner.ProviderCapacity) > 0 {
				return nil
			}
			if runner.LastHeartbeatAt.After(after) {
				last = errors.New("runner heartbeat reported no providers")
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(last, ctx.Err())
		case <-ticker.C:
		}
	}
}
