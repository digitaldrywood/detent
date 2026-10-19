package project

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	releasepkg "github.com/digitaldrywood/detent/internal/release"
)

type githubReleaseBackend struct {
	config  ghconnector.Config
	tracker connector.Connector
}

func newGitHubReleaseBackend(cfg workflowconfig.Config, tracker connector.Connector) githubReleaseBackend {
	return githubReleaseBackend{tracker: tracker, config: ghconnector.Config{
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
	reader, ok := b.tracker.(connector.IssueCommentReader)
	if !ok {
		return false, errors.New("release tracker cannot read report evidence")
	}
	refs := slices.Clone(report.IssueRefs)
	slices.Sort(refs)
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "wi_") {
			continue
		}
		issues, err := b.tracker.FetchIssueStatesByIDs(ctx, []string{ref})
		if err != nil {
			return false, err
		}
		if len(issues) == 0 {
			continue
		}
		issue := issues[0]
		marker := "<!-- detent-auto-release:" + report.Fingerprint + " -->"
		found, err := releaseReportExists(ctx, reader, issue, marker)
		if err != nil || found {
			return false, err
		}
		body := report.Body
		if !strings.Contains(body, marker) {
			body += "\n\n" + marker
		}
		if err := b.tracker.CreateComment(ctx, issue.ID, body); err != nil {
			found, lookupErr := releaseReportExists(ctx, reader, issue, marker)
			if found && lookupErr == nil {
				return false, nil
			}
			return false, errors.Join(err, lookupErr)
		}
		return true, nil
	}
	return false, errors.New("release report has no originating native work item in this project")
}

func releaseReportExists(ctx context.Context, reader connector.IssueCommentReader, issue connector.Issue, marker string) (bool, error) {
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			return true, nil
		}
	}
	return false, nil
}
