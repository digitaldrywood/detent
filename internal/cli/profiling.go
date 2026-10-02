package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"

	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/profiling"
)

func readProfilingConfig(path string) (profiling.Config, error) {
	if path == "" {
		return profiling.Config{}.Normalized(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return profiling.Config{}, err
	}
	var config struct {
		Profiling profiling.Config `yaml:"profiling"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return profiling.Config{}, fmt.Errorf("read profiling config: %w", err)
	}
	return config.Profiling.Normalized(), config.Profiling.Validate()
}

func startWatchedProfiling(ctx context.Context, cfg profiling.Config, path, dataDir, process string, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	service := profiling.New(ctx, dataDir, process, logger)
	if err := service.Apply(cfg); err != nil {
		logger.Warn("configure profiling", "error", err)
	}
	done := make(chan struct{})
	if path == "" {
		close(done)
	} else {
		go func() {
			defer close(done)
			watcher, err := configwatcher.NewFile(path, readProfilingConfig, configwatcher.WithFileLogger(logger))
			if err != nil {
				logger.Warn("create profiling config watcher", "error", err)
				return
			}
			updates, err := watcher.Watch(ctx)
			if err != nil {
				logger.Warn("watch profiling config", "error", err)
				return
			}
			for update := range updates {
				if update.Err != nil {
					logger.Warn("reload profiling config", "error", update.Err)
					continue
				}
				if err := service.Apply(update.Value); err != nil && ctx.Err() == nil {
					logger.Warn("reload profiling", "error", err)
				}
			}
		}()
	}
	return func() {
		cancel()
		<-done
		service.Close()
	}
}
