package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"

	"github.com/digitaldrywood/detent/internal/publication"
)

const graphQLRateLimitProbeQuery = `
query DetentGitHubRateLimitProbe {
  rateLimit { limit used remaining cost resetAt }
}`

const githubCacheTTL = 2 * time.Minute

const (
	graphQLQueryAuthenticate     = "authenticate"
	graphQLQueryCandidateIssues  = "candidate_issues"
	graphQLQueryObservedStatus   = "observed_status"
	graphQLQueryRunningStates    = "running_states"
	graphQLQueryEpicChildren     = "epic_children"
	graphQLQueryIssueParents     = "issue_parents"
	graphQLQueryPullRequests     = "pull_requests"
	graphQLQueryReviewThreads    = "pull_request_review_threads"
	graphQLQueryBlockedReasons   = "blocked_reasons"
	graphQLQueryIssueLookup      = "issue_lookup"
	graphQLQueryProjectItem      = "project_item"
	graphQLQueryProjectMetadata  = "project_metadata"
	graphQLQueryAssignees        = "assignees"
	graphQLQueryCreateComment    = "create_comment"
	graphQLQueryCloseIssue       = "close_issue"
	graphQLQuerySetAssignee      = "set_assignee"
	graphQLQueryRemoveAssignees  = "remove_assignees"
	graphQLQueryUpdateField      = "update_project_field"
	graphQLQueryRemoveItem       = "remove_project_item"
	graphQLQueryAddProjectItem   = "add_project_item"
	graphQLQueryMergeQueue       = "merge_queue"
	graphQLQueryEnqueuePR        = "enqueue_pull_request"
	graphQLQueryDequeuePR        = "dequeue_pull_request"
	graphQLQueryRateLimitProbe   = "rate_limit_probe"
	graphQLQueryLaneSignalStatus = "lane_signal_status"
)

type Config struct {
	Endpoint                string
	APIKey                  string
	GitHubAppID             string
	GitHubAppPrivateKey     string
	GitHubAppPrivateKeyPath string
	GitHubAppInstallationID string

	ProjectSlug string
	Repository  string

	RequiredStatusChecks       []string
	LocalStatus                string
	TokenSource                TokenSource
	HTTPClient                 HTTPClient
	HTTPTransport              HTTPTransportConfig
	RESTMinRemainingReserve    int
	GraphQLMinRemainingReserve int
	RESTFanoutMaxRequests      int
	RESTDebugLogging           bool
	UnstartedThreshold         time.Duration
	DisableConditionalRequests bool
	Logger                     *slog.Logger
	Now                        func() time.Time
	LookupEnv                  func(string) string
	GHToken                    GHTokenFunc

	Publication publication.Policy
}

type Connector struct {
	localStatus         string
	triggerLabelDir     string
	branchMergePolicies map[string]branchMergePolicySnapshot
	client              *Client
	repository          pullRequestRepo
	requiredChecks      []string
	unstartedThreshold  time.Duration
	pullRequests        *pullRequestStatusCache
	prHydration         *pullRequestHydrationCircuitBreaker
	prHydrationCursor   map[string]string
	prDiffFingerprints  map[pullRequestDiffFingerprintCacheKey]string
	logger              *slog.Logger
	now                 func() time.Time
	mu                  sync.RWMutex
	writeMu             sync.Mutex
	instanceLogin       string

	publication             publication.Policy
	publicationMu           sync.Mutex
	publicationVisibilities map[string]publication.Visibility
}

type RepositoryMergeSettings struct {
	AllowAutoMerge   bool `json:"allow_auto_merge"`
	AllowMergeCommit bool `json:"allow_merge_commit"`
	AllowSquashMerge bool `json:"allow_squash_merge"`
	AllowRebaseMerge bool `json:"allow_rebase_merge"`
}

func (c *Connector) RepositoryMergeSettings(ctx context.Context, repository string) (RepositoryMergeSettings, error) {
	repo, ok := pullRequestRepoFromName(repository)
	if !ok {
		return RepositoryMergeSettings{}, fmt.Errorf("invalid GitHub repository %q", repository)
	}
	var settings RepositoryMergeSettings
	if err := c.client.REST(ctx, "GET", "/repos/"+repo.Owner+"/"+repo.Name, nil, &settings); err != nil {
		return RepositoryMergeSettings{}, err
	}
	return settings, nil
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

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	unstartedThreshold := cfg.UnstartedThreshold
	if unstartedThreshold <= 0 {
		unstartedThreshold = defaultUnstartedCheckThreshold
	}

	client, err := NewClient(ClientConfig{
		Endpoint:                   cfg.Endpoint,
		Project:                    cfg.ProjectSlug,
		TokenSource:                tokenSource,
		HTTPClient:                 httpClient,
		GraphQLMinRemainingReserve: int64(cfg.GraphQLMinRemainingReserve),
		RESTPolicy: RESTBudgetPolicy{
			MinRemainingReserve: int64(cfg.RESTMinRemainingReserve),
			FanoutMaxRequests:   int64(cfg.RESTFanoutMaxRequests),
		},
		RESTDebugLogging:           cfg.RESTDebugLogging,
		DisableConditionalRequests: cfg.DisableConditionalRequests,
		Logger:                     logger,
	})
	if err != nil {
		return nil, err
	}
	client.unscopedRESTScope.ProjectID = strings.TrimSpace(cfg.ProjectSlug)

	repository, _ := pullRequestRepoFromName(cfg.Repository)

	publicationPolicy := cfg.Publication
	publicationPolicy.DestinationRepository = strings.TrimSpace(cfg.Repository)
	return &Connector{
		localStatus:        strings.TrimSpace(cfg.LocalStatus),
		client:             client,
		repository:         repository,
		requiredChecks:     normalizeRequiredStatusChecks(cfg.RequiredStatusChecks),
		unstartedThreshold: unstartedThreshold,
		pullRequests:       newPullRequestStatusCache(githubCacheTTL, cfg.Now),
		prHydration:        newPullRequestHydrationCircuitBreaker(cfg.Now),
		prHydrationCursor:  map[string]string{},
		prDiffFingerprints: map[pullRequestDiffFingerprintCacheKey]string{},
		logger:             logger,
		now:                now,

		publication:             publicationPolicy,
		publicationVisibilities: map[string]publication.Visibility{},
	}, nil
}

func (c *Connector) Name() string {
	return connector.BackendGitHub.String()
}

func (c *Connector) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	return c.client.GraphQLRateLimit()
}

func (c *Connector) GraphQLRateLimitStatus() string {
	return c.client.GraphQLRateLimitStatus()
}

func (c *Connector) ProbeGraphQLRateLimit(ctx context.Context) (connector.GraphQLRateLimit, error) {
	var response struct {
		RateLimit *struct{} `json:"rateLimit"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryRateLimitProbe, graphQLRateLimitProbeQuery, nil, &response); err != nil {
		return connector.GraphQLRateLimit{}, err
	}
	if response.RateLimit == nil {
		return connector.GraphQLRateLimit{}, ErrInvalidResponse
	}
	rateLimit, ok := c.client.GraphQLRateLimit()
	if !ok {
		return connector.GraphQLRateLimit{}, ErrInvalidResponse
	}
	c.client.clearGraphQLRateLimitStatus()
	return rateLimit, nil
}

func (c *Connector) ProbeRESTRateLimit(ctx context.Context, minimumRemaining int64) (connector.RESTRateLimit, error) {
	path := "/user"
	if c.repository.Owner != "" && c.repository.Name != "" {
		path = "/repos/" + c.repository.Owner + "/" + c.repository.Name + "/issues?per_page=1&state=all"
	}
	return c.client.probeRESTRecovery(ctx, minimumRemaining, path)
}

func (c *Connector) AuthHealth() (connector.AuthHealth, bool) {
	if c == nil || c.client == nil {
		return connector.AuthHealth{}, false
	}
	return c.client.AuthHealth()
}

func (c *Connector) ResetGraphQLRateLimitUsage() {
	c.client.ResetGraphQLRateLimitUsage()
}

func (c *Connector) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	return c.client.FlushGraphQLRateLimitUsage()
}

func (c *Connector) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	return c.client.FlushRESTRateLimitUsage()
}

func (c *Connector) RESTRateLimitStatus() connector.RESTRateLimitUsage {
	return c.client.RESTRateLimitStatus()
}

func (c *Connector) LiveConnections() int {
	if c == nil || c.client == nil {
		return 0
	}
	return c.client.LiveConnections()
}

func (c *Connector) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

func (c *Connector) Authenticate(ctx context.Context) error {
	var response struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.client.GraphQLWithType(ctx, graphQLQueryAuthenticate, "query { viewer { login } rateLimit { limit used remaining cost resetAt } }",

		nil, &response); err != nil {
		return fmt.Errorf("authenticate github integration: %w",

			err)
	}
	if strings.
		TrimSpace(response.Viewer.Login) ==
		"" {
		return ErrAuthenticationFailed
	}
	c.mu.Lock()
	c.instanceLogin = strings.TrimSpace(response.Viewer.Login)
	c.
		mu.Unlock()
	return nil
}

func (c *Connector) InstanceLogin() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.instanceLogin
}

var _ connector.Authenticator = (*Connector)(nil)
var _ connector.Closer = (*Connector)(nil)
var _ connector.PullRequestCommenter = (*Connector)(nil)
var _ connector.PullRequestCommentReader = (*Connector)(nil)
var _ connector.PullRequestDiffFingerprintReader = (*Connector)(nil)
var _ connector.PullRequestHydrator = (*Connector)(nil)
var _ connector.PullRequestReviewThreadHydrator = (*Connector)(nil)
var _ connector.PullRequestMerger = (*Connector)(nil)
var _ connector.PullRequestMergeQueue = (*Connector)(nil)
