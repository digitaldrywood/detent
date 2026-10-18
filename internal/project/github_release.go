package project

import (
	"context"
	"errors"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
)

type githubReleaseBackend struct {
	config ghconnector.Config
}

func newGitHubReleaseBackend(cfg workflowconfig.Config) githubReleaseBackend {
	return githubReleaseBackend{config: ghconnector.Config{
		Endpoint:                   cfg.Tracker.Endpoint,
		APIKey:                     cfg.Tracker.APIKey,
		Repository:                 cfg.Tracker.Repository,
		GitHubAppID:                cfg.Tracker.GitHubAppID,
		GitHubAppPrivateKey:        cfg.Tracker.GitHubAppPrivateKey,
		GitHubAppPrivateKeyPath:    cfg.Tracker.GitHubAppPrivateKeyPath,
		GitHubAppInstallationID:    cfg.Tracker.GitHubAppInstallationID,
		RequiredStatusChecks:       cfg.Gate.RequiredStatusChecks,
		Publication:                cfg.Tracker.Publication,
		RESTMinRemainingReserve:    cfg.Tracker.GitHubRESTMinReserve,
		GraphQLMinRemainingReserve: cfg.Tracker.GitHubGraphQLMinReserve,
		RESTFanoutMaxRequests:      cfg.Tracker.GitHubRESTFanoutMaxRequests,
		RESTDebugLogging:           cfg.Tracker.GitHubRESTDebugLogging,

		HTTPTransport: ghconnector.HTTPTransportConfig{
			MaxIdleConns:        cfg.Tracker.HTTPMaxIdleConns,
			MaxIdleConnsPerHost: cfg.Tracker.HTTPMaxIdleConnsPerHost,
			IdleConnTimeout:     time.Duration(cfg.Tracker.HTTPIdleConnTimeoutMS) * time.Millisecond,
		},
	}}
}

func withGitHubRelease[T any](cfg ghconnector.Config, call func(*ghconnector.Connector) (T, error)) (result T, err error) {
	client, err := ghconnector.NewConnector(cfg)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	return call(client)
}

func (b githubReleaseBackend) Inspect(ctx context.Context) (releasepkg.Repository, error) {
	return withGitHubRelease(b.config, func(client *ghconnector.Connector) (releasepkg.Repository, error) {
		return client.Inspect(ctx)
	})
}

func (b githubReleaseBackend) CreateTag(ctx context.Context, tag releasepkg.Tag) error {
	_, err := withGitHubRelease(b.config, func(client *ghconnector.Connector) (struct{}, error) {
		return struct{}{}, client.CreateTag(ctx, tag)
	})
	return err
}

func (b githubReleaseBackend) ReleaseWorkflow(ctx context.Context, tag string) (releasepkg.WorkflowRun, bool, error) {
	type observation struct {
		run   releasepkg.WorkflowRun
		found bool
	}
	result, err := withGitHubRelease(b.config, func(client *ghconnector.Connector) (observation, error) {
		run, found, err := client.ReleaseWorkflow(ctx, tag)
		return observation{run: run, found: found}, err
	})
	return result.run, result.found, err
}

func (b githubReleaseBackend) RerunFailedChecks(ctx context.Context, checks []releasepkg.Check) error {
	_, err := withGitHubRelease(b.config, func(client *ghconnector.Connector) (struct{}, error) {
		return struct{}{}, client.RerunFailedChecks(ctx, checks)
	})
	return err
}

func (b githubReleaseBackend) EnsureReleaseReport(ctx context.Context, report releasepkg.Report) (bool, error) {
	return withGitHubRelease(b.config, func(client *ghconnector.Connector) (bool, error) {
		return client.EnsureReleaseReport(ctx, report)
	})
}
