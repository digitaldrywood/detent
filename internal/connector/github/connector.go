package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const authenticateQuery = `
query DetentGitHubAuthenticate($projectId: ID!) {
  viewer { login }
  node(id: $projectId) {
    __typename
    ... on ProjectV2 { id }
  }
  rateLimit { limit used remaining cost resetAt }
}`

const (
	graphQLQueryAuthenticate    = "authenticate"
	graphQLQueryCandidateIssues = "candidate_issues"
	graphQLQueryObservedStatus  = "observed_status"
	graphQLQueryRunningStates   = "running_states"
	graphQLQueryEpicChildren    = "epic_children"
	graphQLQueryIssueParents    = "issue_parents"
	graphQLQueryPullRequests    = "pull_requests"
	graphQLQueryBlockedReasons  = "blocked_reasons"
	graphQLQueryIssueLookup     = "issue_lookup"
	graphQLQueryProjectItem     = "project_item"
	graphQLQueryProjectMetadata = "project_metadata"
	graphQLQueryAssignees       = "assignees"
	graphQLQueryCreateComment   = "create_comment"
	graphQLQueryCloseIssue      = "close_issue"
	graphQLQuerySetAssignee     = "set_assignee"
	graphQLQueryRemoveAssignees = "remove_assignees"
	graphQLQueryUpdateField     = "update_project_field"
)

type Config struct {
	Endpoint                string
	APIKey                  string
	GitHubAppID             string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID string
	ProjectSlug             string
	ActiveStates            []string
	ObservedStates          []string
	TerminalStates          []string
	StateMap                map[string]string
	PriorityMap             map[string]*int
	TokenSource             TokenSource
	HTTPClient              HTTPClient
	HTTPTransport           HTTPTransportConfig
	Logger                  *slog.Logger
	Now                     func() time.Time
	LookupEnv               func(string) string
	GHToken                 GHTokenFunc
}

type Connector struct {
	client         *Client
	projectID      string
	activeStates   []string
	observedStates []string
	terminalStates []string
	stateMap       map[string]string
	priorityMap    map[string]*int
	statusCache    *statusCache
	projectCache   *projectCache
	mu             sync.RWMutex
	instanceLogin  string
}

func NewConnector(cfg Config) (*Connector, error) {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = NewPooledHTTPClient(cfg.HTTPTransport)
	}

	tokenSource := cfg.TokenSource
	if tokenSource == nil {
		tokenSource = NewTokenResolver(TokenResolverConfig{
			Endpoint:                cfg.Endpoint,
			APIKey:                  cfg.APIKey,
			GitHubAppID:             cfg.GitHubAppID,
			GitHubAppPrivateKey:     cfg.GitHubAppPrivateKey,
			GitHubAppPrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
			GitHubAppInstallationID: cfg.GitHubAppInstallationID,
			HTTPClient:              httpClient,
			Now:                     cfg.Now,
			LookupEnv:               cfg.LookupEnv,
			GHToken:                 cfg.GHToken,
		})
	}

	client, err := NewClient(ClientConfig{
		Endpoint:    cfg.Endpoint,
		TokenSource: tokenSource,
		HTTPClient:  httpClient,
		Logger:      cfg.Logger,
	})
	if err != nil {
		return nil, err
	}

	return &Connector{
		client:         client,
		projectID:      strings.TrimSpace(cfg.ProjectSlug),
		activeStates:   normalizeStateList(cfg.ActiveStates, []string{"Todo", "In Progress"}),
		observedStates: normalizeStateList(cfg.ObservedStates, nil),
		terminalStates: normalizeStateList(cfg.TerminalStates, []string{"Done", "Cancelled", "Canceled", "Closed"}),
		stateMap:       cloneStateMap(cfg.StateMap),
		priorityMap:    clonePriorityMapWithDefault(cfg.PriorityMap),
		statusCache:    newStatusCache(githubCacheTTL, cfg.Now),
		projectCache:   newProjectCache(githubCacheTTL, cfg.Now),
	}, nil
}

func (c *Connector) Name() string {
	return connector.BackendGitHub.String()
}

func (c *Connector) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	return c.client.GraphQLRateLimit()
}

func (c *Connector) ResetGraphQLRateLimitUsage() {
	c.client.ResetGraphQLRateLimitUsage()
}

func (c *Connector) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	return c.client.FlushGraphQLRateLimitUsage()
}

func (c *Connector) LiveConnections() int {
	if c == nil || c.client == nil {
		return 0
	}
	return c.client.LiveConnections()
}

func (c *Connector) Authenticate(ctx context.Context) error {
	if c.projectID == "" {
		return ErrMissingProject
	}

	var response struct {
		Viewer *struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Node *struct {
			TypeName string `json:"__typename"`
			ID       string `json:"id"`
		} `json:"node"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryAuthenticate, authenticateQuery, map[string]any{"projectId": c.projectID}, &response); err != nil {
		return fmt.Errorf("authenticate github connector: %w", err)
	}
	if response.Viewer == nil || strings.TrimSpace(response.Viewer.Login) == "" {
		return ErrAuthenticationFailed
	}
	if response.Node == nil || response.Node.TypeName != "ProjectV2" || strings.TrimSpace(response.Node.ID) == "" {
		return ErrProjectNotFound
	}
	login := strings.TrimSpace(response.Viewer.Login)
	c.mu.Lock()
	c.instanceLogin = login
	c.mu.Unlock()

	return nil
}

func (c *Connector) InstanceLogin() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.instanceLogin
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.Authenticator = (*Connector)(nil)
var _ connector.InstanceIdentifier = (*Connector)(nil)
var _ connector.IssueChildrenResolver = (*Connector)(nil)
var _ connector.IssueCloser = (*Connector)(nil)
var _ connector.IssueParentResolver = (*Connector)(nil)
var _ connector.IssueReferenceResolver = (*Connector)(nil)
var _ connector.Provisioner = (*Connector)(nil)
var _ connector.RateLimitReporter = (*Connector)(nil)
var _ connector.GraphQLRateLimitUsageReporter = (*Connector)(nil)
