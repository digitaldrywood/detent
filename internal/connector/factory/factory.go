package factory

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/githublocal"
	linearconnector "github.com/digitaldrywood/detent/internal/connector/linear"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/connector/memory"
)

var (
	ErrBackendNotReady    = errors.New("connector backend not ready")
	ErrUnsupportedBackend = errors.New("unsupported connector backend")
)

type Config struct {
	Kind                        string
	Memory                      memory.Config
	LocalSQLite                 local.Config
	Endpoint                    string
	APIKey                      string
	GitHubTokenRefresh          githubconnector.TokenRefreshFunc
	HTTPMaxIdleConns            int
	HTTPMaxIdleConnsPerHost     int
	HTTPIdleConnTimeoutMS       int
	GitHubRESTMinReserve        int
	GitHubRESTFanoutMaxRequests int
	GitHubRESTDebugLogging      bool
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
	TerminalStates              []string
	StateMap                    map[string]string
	PriorityMap                 map[string]*int
	RequiredStatusChecks        []string
	Logger                      *slog.Logger
}

func NewFromConfig(cfg Config) (connector.Connector, error) {
	kind := normalizeKind(cfg.Kind)

	switch connector.Backend(kind) {
	case connector.BackendMemory:
		return memory.New(cfg.Memory), nil
	case connector.BackendLocalSQLite:
		return local.New(cfg.LocalSQLite)
	case connector.BackendLinear:
		return linearconnector.NewConnector(linearconnector.Config{
			Endpoint: cfg.Endpoint,
			APIKey:   cfg.APIKey,
			StateMap: cfg.StateMap,
			Logger:   cfg.Logger,
		})
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
			RESTFanoutMaxRequests:      cfg.GitHubRESTFanoutMaxRequests,
			RESTDebugLogging:           cfg.GitHubRESTDebugLogging,
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
			TerminalStates:             cfg.TerminalStates,
			StateMap:                   cfg.StateMap,
			PriorityMap:                cfg.PriorityMap,
			RequiredStatusChecks:       cfg.RequiredStatusChecks,
			Logger:                     cfg.Logger,
		})
	case connector.BackendGitHubLocal:
		var tokenSource githubconnector.TokenSource
		if strings.TrimSpace(cfg.APIKey) != "" && cfg.GitHubTokenRefresh != nil && !cfg.hasGitHubAppCredentials() {
			tokenSource = githubconnector.NewRefreshableTokenSource(cfg.APIKey, cfg.GitHubTokenRefresh)
		}
		localCfg := cfg.LocalSQLite
		if len(localCfg.ActiveStates) == 0 {
			localCfg.ActiveStates = cloneStrings(cfg.ActiveStates)
		}
		if len(localCfg.ObservedStates) == 0 {
			localCfg.ObservedStates = cloneStrings(cfg.ObservedStates)
		}
		if len(localCfg.TerminalStates) == 0 {
			localCfg.TerminalStates = cloneStrings(cfg.TerminalStates)
		}
		return githublocal.New(githublocal.Config{
			GitHub: githubconnector.Config{
				Endpoint:    cfg.Endpoint,
				APIKey:      cfg.APIKey,
				TokenSource: tokenSource,
				HTTPTransport: githubconnector.HTTPTransportConfig{
					MaxIdleConns:        cfg.HTTPMaxIdleConns,
					MaxIdleConnsPerHost: cfg.HTTPMaxIdleConnsPerHost,
					IdleConnTimeout:     time.Duration(cfg.HTTPIdleConnTimeoutMS) * time.Millisecond,
				},
				RESTMinRemainingReserve:    cfg.GitHubRESTMinReserve,
				RESTFanoutMaxRequests:      cfg.GitHubRESTFanoutMaxRequests,
				RESTDebugLogging:           cfg.GitHubRESTDebugLogging,
				DisableConditionalRequests: conditionalRequestsDisabled(cfg.ConditionalRequests),
				GitHubAppID:                cfg.GitHubAppID,
				GitHubAppPrivateKey:        cfg.GitHubAppPrivateKey,
				GitHubAppPrivateKeyPath:    cfg.GitHubAppPrivateKeyPath,
				GitHubAppInstallationID:    cfg.GitHubAppInstallationID,
				DependencySource:           cfg.DependencySource,
				Repository:                 cfg.Repository,
				StatusField:                cfg.StatusField,
				StatusLabelPrefix:          cfg.StatusLabelPrefix,
				ActiveStates:               cfg.ActiveStates,
				ObservedStates:             cfg.ObservedStates,
				TerminalStates:             cfg.TerminalStates,
				StateMap:                   cfg.StateMap,
				PriorityMap:                cfg.PriorityMap,
				RequiredStatusChecks:       cfg.RequiredStatusChecks,
				Logger:                     cfg.Logger,
			},
			Local:          localCfg,
			Repository:     cfg.Repository,
			InitialState:   firstNonBlank(cfg.ActiveStates),
			ActiveStates:   cfg.ActiveStates,
			TerminalStates: cfg.TerminalStates,
		})
	case connector.BackendGitLab, connector.BackendJira:
		return nil, fmt.Errorf("%w: %s", ErrBackendNotReady, kind)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedBackend, kind)
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
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if normalized == "" {
		return connector.BackendMemory.String()
	}
	return normalized
}

func cloneStrings(values []string) []string {
	return append([]string(nil), values...)
}

func firstNonBlank(values []string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
