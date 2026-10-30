package factory

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	case connector.BackendGitLab, connector.BackendJira:
		return nil, fmt.Errorf("%w: %s", ErrBackendNotReady, kind)
	default:
		return nil, fmt.Errorf("%w: %s; use hub_native", ErrUnsupportedBackend, kind)
	}
}

func normalizeKind(kind string) string { return strings.ToLower(strings.TrimSpace(kind)) }
