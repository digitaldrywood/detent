package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/profiling"
)

func readProfilingConfig(path string) (profiling.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return profiling.Config{}, err
	}
	var file struct {
		Profiling profiling.Config `yaml:"profiling"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return profiling.Config{}, err
	}
	if file.Profiling.Capture.Dir != "" {
		dir := file.Profiling.Capture.Dir
		if strings.HasPrefix(dir, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return profiling.Config{}, err
			}
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~/"))
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(path), dir)
		}
		file.Profiling.Capture.Dir = dir
	}
	return file.Profiling, nil
}

func startHubProfiling(cmd *cobra.Command, lookupEnv func(string) string, hostedPath, databasePath string, logger *slog.Logger) func() {
	path := ""
	if flag := cmd.Flag("config"); flag != nil {
		path = strings.TrimSpace(flag.Value.String())
	}
	if path == "" {
		path = strings.TrimSpace(lookupEnv("CONFIG"))
	}
	if path == "" {
		path = strings.TrimSpace(lookupEnv("DETENT_CONFIG"))
	}
	if path == "" {
		path = hostedPath
	}
	if path == "" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	service := profiling.New(filepath.Join(filepath.Dir(databasePath), "profiles", "hub"), logger)
	apply := func() {
		config, err := readProfilingConfig(path)
		if err != nil {
			logger.Warn("profiling configuration read failed", "path", path, "error", err)
			return
		}
		service.Apply(ctx, config)
	}
	watcher, err := configwatcher.NewFile(path, readProfilingConfig, configwatcher.WithFileLogger(logger))
	var updates <-chan configwatcher.FileUpdate[profiling.Config]
	if err == nil {
		updates, err = watcher.Watch(ctx)
	}
	if err != nil {
		logger.Warn("profiling configuration watch failed", "path", path, "error", err)
	}
	apply()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updates:
				if !ok {
					return
				}
				if update.Err != nil {
					if !errors.Is(update.Err, context.Canceled) {
						logger.Warn("profiling configuration reload failed", "error", update.Err)
					}
					continue
				}
				service.Apply(ctx, update.Value)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		if updates != nil {
			for range updates {
			}
		}
		service.Close()
	}
}
