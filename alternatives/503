package cli

import (
	"context"
	"slices"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func readLocalConfigurationFile(ctx context.Context, path string, opts ...globalconfig.Option) (globalconfig.Config, error) {
	cfg, err := readRunnerConfiguration(path, opts...)
	if err != nil {
		return cfg, err
	}
	return resolveRunnerProjectsReadOnly(ctx, cfg)
}

func readRunnerRuntimeConfig(ctx context.Context, path string, opts ...globalconfig.Option) (globalconfig.Config, error) {
	cfg, err := readRunnerConfiguration(path, opts...)
	if err != nil {
		return cfg, err
	}
	return resolveRunnerProjects(ctx, cfg)
}

func readRunnerConfiguration(path string, opts ...globalconfig.Option) (globalconfig.Config, error) {
	cfg, err := globalconfig.Read(path, opts...)
	if err == nil {
		return cfg, nil
	}
	runnerOpts := append(slices.Clone(opts), globalconfig.WithMissingProjectPaths())
	runner, runnerErr := globalconfig.Read(path, runnerOpts...)
	if runnerErr == nil && runner.Client.IdentityFile != "" {
		return runner, nil
	}
	return cfg, err
}
