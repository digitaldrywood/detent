package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/memory"
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
	GitHubTokenRefresh      githubconnector.TokenRefreshFunc
	HTTPMaxIdleConns        int
	HTTPMaxIdleConnsPerHost int
	HTTPIdleConnTimeoutMS   int
	GitHubAppID             string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID string
	GitHubStatusSource      string
	ProjectSlug             string
	Repository              string
	StatusField             string
	StatusLabelPrefix       string
	ActiveStates            []string
	ObservedStates          []string
	TerminalStates          []string
	StateMap                map[string]string
	PriorityMap             map[string]*int
}

func NewFromConfig(cfg Config) (connector.Connector, error) {
	kind := normalizeKind(cfg.Kind)

	switch connector.Backend(kind) {
	case connector.BackendMemory:
		return memory.New(cfg.Memory), nil
	case connector.BackendLinear:
		return unimplementedConnector{name: kind}, nil
	case connector.BackendGitHub:
		var tokenSource githubconnector.TokenSource
		if strings.TrimSpace(cfg.APIKey) != "" && cfg.GitHubTokenRefresh != nil && !cfg.hasGitHubAppCredentials() {
			tokenSource = githubconnector.NewRefreshableTokenSource(cfg.APIKey, cfg.GitHubTokenRefresh)
		}
		return githubconnector.NewConnector(githubconnector.Config{
			Endpoint:    cfg.Endpoint,
			APIKey:      cfg.APIKey,
			TokenSource: tokenSource,
			HTTPTransport: githubconnector.HTTPTransportConfig{
				MaxIdleConns:        cfg.HTTPMaxIdleConns,
				MaxIdleConnsPerHost: cfg.HTTPMaxIdleConnsPerHost,
				IdleConnTimeout:     time.Duration(cfg.HTTPIdleConnTimeoutMS) * time.Millisecond,
			},
			GitHubAppID:             cfg.GitHubAppID,
			GitHubAppPrivateKey:     cfg.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID: cfg.GitHubAppInstallationID,
			GitHubStatusSource:      cfg.GitHubStatusSource,
			ProjectSlug:             cfg.ProjectSlug,
			Repository:              cfg.Repository,
			StatusField:             cfg.StatusField,
			StatusLabelPrefix:       cfg.StatusLabelPrefix,
			ActiveStates:            cfg.ActiveStates,
			ObservedStates:          cfg.ObservedStates,
			TerminalStates:          cfg.TerminalStates,
			StateMap:                cfg.StateMap,
			PriorityMap:             cfg.PriorityMap,
		})
	case connector.BackendGitLab, connector.BackendJira:
		return nil, fmt.Errorf("%w: %s", ErrBackendNotReady, kind)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedBackend, kind)
	}
}

func (cfg Config) hasGitHubAppCredentials() bool {
	return strings.TrimSpace(cfg.GitHubAppID) != "" &&
		strings.TrimSpace(cfg.GitHubAppInstallationID) != "" &&
		(strings.TrimSpace(cfg.GitHubAppPrivateKey) != "" ||
			strings.TrimSpace(cfg.GitHubAppPrivateKeyPath) != "")
}

type unimplementedConnector struct {
	name string
}

var _ connector.Connector = unimplementedConnector{}

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

func (unimplementedConnector) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (unimplementedConnector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

func normalizeKind(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if normalized == "" {
		return connector.BackendMemory.String()
	}
	return normalized
}
