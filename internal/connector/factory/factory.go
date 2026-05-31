package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/symphony-go/internal/connector"
	githubconnector "github.com/digitaldrywood/symphony-go/internal/connector/github"
	"github.com/digitaldrywood/symphony-go/internal/connector/memory"
)

var (
	ErrBackendNotReady    = errors.New("connector backend not ready")
	ErrUnsupportedBackend = errors.New("unsupported connector backend")
)

type Config struct {
	Kind                    string
	Memory                  memory.Config
	Endpoint                string
	APIKey                  string
	GitHubAppID             string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID string
	ProjectSlug             string
}

func NewFromConfig(cfg Config) (connector.Connector, error) {
	kind := normalizeKind(cfg.Kind)

	switch connector.Backend(kind) {
	case connector.BackendMemory:
		return memory.New(cfg.Memory), nil
	case connector.BackendLinear:
		return unimplementedConnector{name: kind}, nil
	case connector.BackendGitHub:
		return githubconnector.NewConnector(githubconnector.Config{
			Endpoint:                cfg.Endpoint,
			APIKey:                  cfg.APIKey,
			GitHubAppID:             cfg.GitHubAppID,
			GitHubAppPrivateKey:     cfg.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID: cfg.GitHubAppInstallationID,
			ProjectSlug:             cfg.ProjectSlug,
		})
	case connector.BackendGitLab, connector.BackendJira:
		return nil, fmt.Errorf("%w: %s", ErrBackendNotReady, kind)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedBackend, kind)
	}
}

type unimplementedConnector struct {
	name string
}

func (c unimplementedConnector) Name() string {
	return c.name
}

func (unimplementedConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (unimplementedConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (unimplementedConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (unimplementedConnector) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (unimplementedConnector) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func normalizeKind(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if normalized == "" {
		return connector.BackendMemory.String()
	}
	return normalized
}
