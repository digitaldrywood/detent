package cli

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type RestartRequest struct {
	mu     sync.RWMutex
	binary string
}

func NewRestartRequest() *RestartRequest {
	return &RestartRequest{}
}

func (r *RestartRequest) Binary() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.binary
}

func (r *RestartRequest) set(binary string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.binary = strings.TrimSpace(binary)
}

func newRuntimeUpdateScheduler(cfg BootConfig, logger *slog.Logger) (*detentupdate.Scheduler, error) {
	interval := time.Duration(cfg.Global.Update.NormalizedCheckIntervalHours()) * time.Hour
	schedulerConfig := detentupdate.SchedulerConfig{
		Enabled:          cfg.Global.Update.AutoCheckEnabled,
		AutoApplyEnabled: cfg.Global.Update.AutoApplyEnabled,
		CheckInterval:    interval,
		Logger:           logger,
	}
	executable, err := os.Executable()
	if err != nil {
		if !schedulerConfig.Enabled {
			return detentupdate.NewScheduler(schedulerConfig)
		}
		return nil, fmt.Errorf("resolve executable for automatic update checks: %w", err)
	}
	schedulerConfig.LastAppliedVersion = detentupdate.InstalledReleaseVersion(detentupdate.DetectionOptions{
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
	})
	if !schedulerConfig.Enabled {
		return detentupdate.NewScheduler(schedulerConfig)
	}
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = strings.TrimSpace(cfg.Build.Version)
	}
	schedulerConfig.Updater = detentupdate.NewService(detentupdate.Config{
		CurrentVersion: version,
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Client: detentupdate.NewGitHubClient(detentupdate.GitHubClientConfig{
			Token: strings.TrimSpace(cfg.Runtime.GitHubToken.Value),
		}),
	})
	schedulerConfig.RequestRestart = func(binary string) bool {
		if strings.TrimSpace(binary) == "" {
			binary = executable
		}
		return requestUpdateRestart(cfg.Shutdown, cfg.Restart, binary)
	}
	return detentupdate.NewScheduler(schedulerConfig)
}

func requestUpdateRestart(controller *ShutdownController, restart *RestartRequest, binary string) bool {
	if controller == nil || restart == nil || strings.TrimSpace(binary) == "" {
		return false
	}
	return controller.RequestDrainIfIdle(func() {
		restart.set(binary)
	})
}
