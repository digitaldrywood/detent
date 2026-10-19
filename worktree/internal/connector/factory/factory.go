package factory

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/publication"
)

var (
	ErrBackendNotReady    = errors.New("connector backend not ready")
	ErrUnsupportedBackend = errors.New("unsupported connector backend")
)

type Config struct {
	Kind string

	Endpoint                    string
	APIKey                      string
	GitHubTokenRefresh          githubconnector.TokenRefreshFunc
	HTTPMaxIdleConns            int
	HTTPMaxIdleConnsPerHost     int
	HTTPIdleConnTimeoutMS       int
	GitHubRESTMinReserve        int
	GitHubGraphQLMinReserve     int
	GitHubRESTFanoutMaxRequests int
	GitHubRESTDebugLogging      bool
	GitHubUnstartedSeconds      int
	ConditionalRequests         *bool
	GitHubAppID                 string
	GitHubAppPrivateKey         string
	GitHubAppPrivateKeyPath     string
	GitHubAppInstallationID     string
	GitHubStatusSource          string
	DependencySource            string
	ProjectSlug                 string
	Repository                  string
	StatusField                 string
	StatusLabelPrefix           string
	ActiveStates                []string
	ObservedStates              []string
	PlanStop                    string
	TerminalStates              []string
	StateMap                    map[string]string
	PriorityMap                 map[string]*int
	RequiredStatusChecks        []string
	LocalStatus                 string
	Logger                      *slog.Logger
	Publication                 publication.Policy
}

func NewFromConfig(cfg Config) (connector.Connector, error) {
	kind := normalizeKind(cfg.Kind)

	switch connector.Backend(kind) {
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
			RESTMinRemainingReserve:    cfg.GitHubRESTMinReserve,
			GraphQLMinRemainingReserve: cfg.GitHubGraphQLMinReserve,
			RESTFanoutMaxRequests:      cfg.GitHubRESTFanoutMaxRequests,
			RESTDebugLogging:           cfg.GitHubRESTDebugLogging,
			UnstartedThreshold:         time.Duration(cfg.GitHubUnstartedSeconds) * time.Second,
			DisableConditionalRequests: conditionalRequestsDisabled(cfg.ConditionalRequests),
			GitHubAppID:                cfg.GitHubAppID,
			GitHubAppPrivateKey:        cfg.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath:    cfg.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID:    cfg.GitHubAppInstallationID,
			GitHubStatusSource:         cfg.GitHubStatusSource,
			DependencySource:           cfg.DependencySource,
			ProjectSlug:                cfg.ProjectSlug,
			Repository:                 cfg.Repository,
			StatusField:                cfg.StatusField,
			StatusLabelPrefix:          cfg.StatusLabelPrefix,
			ActiveStates:               cfg.ActiveStates,
			ObservedStates:             cfg.ObservedStates,
			PlanStop:                   cfg.PlanStop,
			TerminalStates:             cfg.TerminalStates,
			StateMap:                   cfg.StateMap,
			PriorityMap:                cfg.PriorityMap,
			RequiredStatusChecks:       cfg.RequiredStatusChecks,
			LocalStatus:                cfg.LocalStatus,
			Logger:                     cfg.Logger,
			Publication:                cfg.Publication,
		})
	case connector.BackendGitLab, connector.BackendJira:
		return nil, fmt.Errorf("%w: %s", ErrBackendNotReady, kind)
	default:
		return nil, fmt.Errorf("%w: %s; use hub_native", ErrUnsupportedBackend, kind)
	}
}

func conditionalRequestsDisabled(enabled *bool) bool {
	return enabled != nil && !*enabled
}

func (cfg Config) hasGitHubAppCredentials() bool {
	return strings.TrimSpace(cfg.GitHubAppID) != "" &&
		strings.TrimSpace(cfg.GitHubAppInstallationID) != "" &&
		(strings.TrimSpace(cfg.GitHubAppPrivateKey) != "" ||
			strings.TrimSpace(cfg.GitHubAppPrivateKeyPath) != "")
}

func normalizeKind(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}
