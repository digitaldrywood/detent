package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	workerGitHubRateLimitBodyMaxBytes = 64 * 1024
	workerGitHubProbeTimeout          = 10 * time.Second
	workerGitHubAuthTokenTimeout      = 5 * time.Second
)

var (
	ErrWorkerGitHubSharedReserve = errors.New("worker github shared-budget reserve must be above the orchestrator dispatch floor")
	ErrWorkerGitHubRESTReserved  = errors.New("worker github REST budget reached reserved headroom")
	ErrWorkerGitHubBudgetMonitor = errors.New("worker github REST budget monitor failed")
	workerGitHubEnvNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type workerGitHubCredentialMode string

const (
	workerGitHubCredentialDisabled     workerGitHubCredentialMode = "isolated_disabled"
	workerGitHubCredentialUnclassified workerGitHubCredentialMode = "unclassified"
	workerGitHubCredentialShared       workerGitHubCredentialMode = "shared_budget"
	workerGitHubCredentialDistinct     workerGitHubCredentialMode = "distinct_principal"
)

type workerGitHubHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type workerGitHubProbeContextFactory func(context.Context) (context.Context, context.CancelFunc)

type workerGitHubTokenResolver func(context.Context) (string, error)

type workerGitHubPolicy struct {
	Enabled              bool
	CredentialMode       workerGitHubCredentialMode
	Token                string
	OrchestratorToken    string
	CredentialIdentity   string
	RateLimitURL         string
	GraphQLURL           string
	MinRemaining         int64
	OrchestratorFloor    int64
	PollInterval         time.Duration
	HTTPClient           workerGitHubHTTPClient
	ProbeContext         workerGitHubProbeContextFactory
	Poll                 <-chan time.Time
	Logger               *slog.Logger
	ProjectID            string
	IssueIdentifier      string
	classificationLogged bool
}

type workerGitHubBudget struct {
	Limit      int64
	Used       int64
	Remaining  int64
	ResetAt    time.Time
	ObservedAt time.Time
}

func (r *Runner) workerGitHubPolicy(ctx context.Context, cfg config.Config, issueIdentifier string) (workerGitHubPolicy, error) {
	policy, err := newWorkerGitHubPolicy(ctx, cfg, r.projectID, issueIdentifier, r.lookupEnv, nil, nil, r.logger)
	if err != nil {
		return workerGitHubPolicy{}, err
	}
	return policy.classifyCredential(ctx)
}

func (r *Runner) ProbeGitHubRESTBudget(ctx context.Context, issue connector.Issue) (telemetry.RESTBudget, bool, error) {
	workflow, _, _, _ := r.runtimeSnapshot()
	policy, err := r.workerGitHubPolicy(ctx, workflow.Config, issue.Identifier)
	if err != nil {
		return telemetry.RESTBudget{}, true, err
	}
	if !policy.Enabled {
		return telemetry.RESTBudget{}, false, nil
	}
	budget, err := policy.probe(ctx)
	if err != nil {
		return telemetry.RESTBudget{}, true, err
	}
	return policy.restBudget(budget), true, nil
}

func workerGitHubPolicyName(policy workerGitHubPolicy) string {
	if policy.CredentialMode != "" {
		return string(policy.CredentialMode)
	}
	if policy.Enabled {
		return string(workerGitHubCredentialUnclassified)
	}
	return string(workerGitHubCredentialDisabled)
}

func mergeAgentRateLimits(current *telemetry.RateLimits, incoming *telemetry.RateLimits) *telemetry.RateLimits {
	if incoming == nil {
		return current
	}
	if current == nil {
		cloned := *incoming
		cloned.GitHubRESTBudgets = mergeAgentRESTBudgets(nil, incoming.GitHubRESTBudgets)
		return &cloned
	}
	merged := *current
	if incoming.LimitID != "" {
		merged.LimitID = incoming.LimitID
	}
	if incoming.LimitName != "" {
		merged.LimitName = incoming.LimitName
	}
	if incoming.ReachedType != "" {
		merged.ReachedType = incoming.ReachedType
	}
	if incoming.Primary != nil {
		merged.Primary = incoming.Primary
	}
	if incoming.Secondary != nil {
		merged.Secondary = incoming.Secondary
	}
	if incoming.Credits != nil {
		merged.Credits = incoming.Credits
	}
	if incoming.GitHubGraphQL != nil {
		merged.GitHubGraphQL = incoming.GitHubGraphQL
	}
	if incoming.GitHubREST != nil {
		merged.GitHubREST = incoming.GitHubREST
	}
	merged.GitHubRESTBudgets = mergeAgentRESTBudgets(current.GitHubRESTBudgets, incoming.GitHubRESTBudgets)
	if incoming.GraphQLCost != nil {
		merged.GraphQLCost = incoming.GraphQLCost
	}
	if incoming.RESTUsage != nil {
		merged.RESTUsage = incoming.RESTUsage
	}
	return &merged
}

func mergeAgentRESTBudgets(current []telemetry.RESTBudget, incoming []telemetry.RESTBudget) []telemetry.RESTBudget {
	budgets := make(map[string]telemetry.RESTBudget, len(current)+len(incoming))
	for _, budget := range append(append([]telemetry.RESTBudget(nil), current...), incoming...) {
		consumer := strings.TrimSpace(budget.Consumer)
		if consumer == "" {
			consumer = telemetry.RESTConsumerOrchestrator
		}
		key := consumer + "\x00" + strings.TrimSpace(budget.CredentialIdentity) + "\x00" + strings.TrimSpace(budget.EndpointFamily) + "\x00" + strings.TrimSpace(budget.Resource)
		existing, ok := budgets[key]
		if !ok || budget.ObservedAt == nil || existing.ObservedAt == nil || !budget.ObservedAt.Before(*existing.ObservedAt) {
			budgets[key] = budget
		}
	}
	out := make([]telemetry.RESTBudget, 0, len(budgets))
	for _, budget := range budgets {
		out = append(out, budget)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.TrimSpace(out[i].Consumer) + "\x00" + out[i].CredentialIdentity + "\x00" + out[i].EndpointFamily
		right := strings.TrimSpace(out[j].Consumer) + "\x00" + out[j].CredentialIdentity + "\x00" + out[j].EndpointFamily
		return left < right
	})
	return out
}

func newWorkerGitHubPolicy(ctx context.Context, cfg config.Config, projectID string, issueIdentifier string, lookupEnv func(string) string, resolveGHAuthToken workerGitHubTokenResolver, httpClient workerGitHubHTTPClient, logger *slog.Logger) (workerGitHubPolicy, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	if resolveGHAuthToken == nil {
		resolveGHAuthToken = defaultWorkerGitHubToken
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	rawWorkerToken := strings.TrimSpace(cfg.Worker.GitHubToken)
	if rawWorkerToken == "" {
		return workerGitHubPolicy{
			CredentialMode:    workerGitHubCredentialDisabled,
			MinRemaining:      int64(cfg.Worker.GitHubRESTMinReserve),
			OrchestratorFloor: int64(cfg.Tracker.GitHubRESTMinReserve),
			PollInterval:      time.Duration(cfg.Worker.GitHubRESTPollIntervalMS) * time.Millisecond,
			HTTPClient:        httpClient,
			Logger:            logger,
			ProjectID:         strings.TrimSpace(projectID),
			IssueIdentifier:   strings.TrimSpace(issueIdentifier),
		}, nil
	}

	workerToken, err := resolveWorkerGitHubSecret(ctx, rawWorkerToken, lookupEnv, resolveGHAuthToken)
	if err != nil {
		return workerGitHubPolicy{}, err
	}
	graphQLEndpoint := ""
	orchestratorCredential := ""
	if cfg.Tracker.Kind == config.TrackerGitHub || cfg.Tracker.Kind == config.TrackerGitHubLocal {
		graphQLEndpoint = cfg.Tracker.Endpoint
		orchestratorCredential = cfg.Tracker.APIKey
	}
	orchestratorToken, err := resolveWorkerGitHubSecretReference(strings.TrimSpace(orchestratorCredential), lookupEnv)
	if err != nil {
		return workerGitHubPolicy{}, fmt.Errorf("resolve orchestrator github credential for worker isolation: %w", err)
	}
	rateLimitURL, graphQLURL, endpointIdentity, err := workerGitHubEndpoints(graphQLEndpoint)
	if err != nil {
		return workerGitHubPolicy{}, err
	}
	sum := sha256.Sum256([]byte(endpointIdentity + "\x00" + workerToken))
	credentialMode := workerGitHubCredentialUnclassified
	if orchestratorToken != "" && workerToken == orchestratorToken {
		credentialMode = workerGitHubCredentialShared
	} else if orchestratorToken == "" {
		credentialMode = workerGitHubCredentialDistinct
	}
	return workerGitHubPolicy{
		Enabled:            true,
		CredentialMode:     credentialMode,
		Token:              workerToken,
		OrchestratorToken:  orchestratorToken,
		CredentialIdentity: fmt.Sprintf("github-rest:%x", sum[:6]),
		RateLimitURL:       rateLimitURL,
		GraphQLURL:         graphQLURL,
		MinRemaining:       int64(cfg.Worker.GitHubRESTMinReserve),
		OrchestratorFloor:  int64(cfg.Tracker.GitHubRESTMinReserve),
		PollInterval:       time.Duration(cfg.Worker.GitHubRESTPollIntervalMS) * time.Millisecond,
		HTTPClient:         httpClient,
		ProbeContext:       withWorkerGitHubProbeTimeout,
		Logger:             logger,
		ProjectID:          strings.TrimSpace(projectID),
		IssueIdentifier:    strings.TrimSpace(issueIdentifier),
	}, nil
}

func withWorkerGitHubProbeTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, workerGitHubProbeTimeout)
}

func resolveWorkerGitHubSecret(ctx context.Context, value string, lookupEnv func(string) string, resolveGHAuthToken workerGitHubTokenResolver) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if config.IsGitHubTokenSentinel(value) {
		resolved, err := resolveGHAuthToken(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve worker.github_token via gh auth token: %w", err)
		}
		resolved = strings.TrimSpace(resolved)
		if resolved == "" {
			return "", errors.New("resolve worker.github_token via gh auth token: empty token")
		}
		return resolved, nil
	}
	return resolveWorkerGitHubSecretReference(value, lookupEnv)
}

func resolveWorkerGitHubSecretReference(value string, lookupEnv func(string) string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	name, referenced := strings.CutPrefix(value, "$")
	if !referenced || !workerGitHubEnvNamePattern.MatchString(name) {
		return value, nil
	}
	resolved := strings.TrimSpace(lookupEnv(name))
	if resolved == "" {
		return "", fmt.Errorf("worker github credential environment variable %s is empty", name)
	}
	return resolved, nil
}

func defaultWorkerGitHubToken(ctx context.Context) (string, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("gh was not found on PATH")
	}
	commandCtx, cancel := context.WithTimeout(ctx, workerGitHubAuthTokenTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, path, "auth", "token") // #nosec G204 -- gh path is PATH-resolved and arguments are fixed.
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("gh auth token failed: %w: %s", err, detail)
		}
		return "", fmt.Errorf("gh auth token failed: %w", err)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh auth token returned an empty token")
	}
	return token, nil
}

func workerGitHubEndpoints(graphQLEndpoint string) (string, string, string, error) {
	graphQLEndpoint = strings.TrimSpace(graphQLEndpoint)
	if graphQLEndpoint == "" {
		graphQLEndpoint = "https://api.github.com/graphql"
	}
	parsed, err := url.Parse(graphQLEndpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", "", fmt.Errorf("invalid worker github endpoint %q", graphQLEndpoint)
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	graphQLURL := parsed.String()
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/graphql")
	endpointIdentity := strings.TrimRight(parsed.String(), "/")
	parsed.Path += "/rate_limit"
	rateLimitURL := parsed.String()
	return rateLimitURL, graphQLURL, endpointIdentity, nil
}

func configureWorkerGitHubEnvironment(request *AgentTurnRequest) error {
	if request == nil || strings.TrimSpace(request.TempDir) == "" {
		return nil
	}
	configDir := filepath.Join(request.TempDir, "github-cli")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create worker github config directory: %w", err)
	}
	variables := make(map[string]string, len(request.Environment.Variables)+5)
	for key, value := range request.Environment.Variables {
		variables[key] = value
	}
	variables["GH_CONFIG_DIR"] = configDir
	variables["GH_TOKEN"] = request.workerGitHub.Token
	variables["GITHUB_TOKEN"] = request.workerGitHub.Token
	variables["GH_ENTERPRISE_TOKEN"] = request.workerGitHub.Token
	variables["GITHUB_ENTERPRISE_TOKEN"] = request.workerGitHub.Token
	request.Environment.Variables = variables
	return nil
}

func startWorkerGitHubGovernor(ctx context.Context, policy workerGitHubPolicy, onUpdate AgentUpdateHandler) (context.Context, func() error, error) {
	if !policy.Enabled {
		return ctx, func() error { return nil }, nil
	}
	classified, err := policy.classifyCredential(ctx)
	if err != nil {
		return ctx, func() error { return nil }, err
	}
	policy = classified
	budget, err := policy.probe(ctx)
	if err != nil {
		return ctx, func() error { return nil }, fmt.Errorf("%w: %w", ErrWorkerGitHubBudgetMonitor, err)
	}
	if err := policy.observe(budget, onUpdate); err != nil {
		return ctx, func() error { return nil }, err
	}
	if err := policy.reserveError(budget); err != nil {
		return ctx, func() error { return nil }, err
	}

	governedCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	pollInterval := policy.PollInterval
	if pollInterval <= 0 {
		pollInterval = time.Minute
	}
	poll := policy.Poll
	stopPoll := func() {}
	if poll == nil {
		ticker := time.NewTicker(pollInterval)
		poll = ticker.C
		stopPoll = ticker.Stop
	}
	go func() {
		defer close(done)
		defer stopPoll()
		for {
			select {
			case <-governedCtx.Done():
				return
			case <-poll:
				budget, probeErr := policy.probe(governedCtx)
				if probeErr != nil {
					cancel(fmt.Errorf("%w: %w", ErrWorkerGitHubBudgetMonitor, probeErr))
					return
				}
				if observeErr := policy.observe(budget, onUpdate); observeErr != nil {
					cancel(fmt.Errorf("%w: publish worker github budget: %w", ErrWorkerGitHubBudgetMonitor, observeErr))
					return
				}
				if reserveErr := policy.reserveError(budget); reserveErr != nil {
					cancel(reserveErr)
					return
				}
			}
		}
	}()

	stop := func() error {
		cause := context.Cause(governedCtx)
		cancel(context.Canceled)
		<-done
		if errors.Is(cause, ErrWorkerGitHubRESTReserved) || errors.Is(cause, ErrWorkerGitHubBudgetMonitor) {
			return cause
		}
		return nil
	}
	return governedCtx, stop, nil
}

func (p workerGitHubPolicy) classifyCredential(ctx context.Context) (workerGitHubPolicy, error) {
	if !p.Enabled {
		p.CredentialMode = workerGitHubCredentialDisabled
		return p, nil
	}
	if p.CredentialMode == "" {
		p.CredentialMode = workerGitHubCredentialUnclassified
	}
	if p.CredentialMode == workerGitHubCredentialUnclassified && strings.TrimSpace(p.OrchestratorToken) == "" {
		p.CredentialMode = workerGitHubCredentialDistinct
	}
	if p.CredentialMode == workerGitHubCredentialUnclassified {
		workerID, err := p.authenticatedUserID(ctx, p.Token)
		if err != nil {
			return workerGitHubPolicy{}, fmt.Errorf("%w: verify worker credential principal: %w", ErrWorkerGitHubBudgetMonitor, err)
		}
		orchestratorID, err := p.authenticatedUserID(ctx, p.OrchestratorToken)
		if err != nil {
			return workerGitHubPolicy{}, fmt.Errorf("%w: verify orchestrator credential principal: %w", ErrWorkerGitHubBudgetMonitor, err)
		}
		if workerID == orchestratorID {
			p.CredentialMode = workerGitHubCredentialShared
		} else {
			p.CredentialMode = workerGitHubCredentialDistinct
		}
	}
	if p.CredentialMode != workerGitHubCredentialShared {
		return p, nil
	}
	if p.MinRemaining <= p.OrchestratorFloor {
		return workerGitHubPolicy{}, fmt.Errorf("%w: worker reserve=%d orchestrator floor=%d", ErrWorkerGitHubSharedReserve, p.MinRemaining, p.OrchestratorFloor)
	}
	if !p.classificationLogged {
		if p.Logger == nil {
			p.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		p.Logger.Warn(
			"worker github credential uses shared REST budget",
			"credential_mode", p.CredentialMode,
			"project_id", p.ProjectID,
			"issue_identifier", p.IssueIdentifier,
			"credential_identity", p.CredentialIdentity,
			"usage_attribution", "indeterminate",
			"worker_reserve", p.MinRemaining,
			"orchestrator_dispatch_floor", p.OrchestratorFloor,
		)
		p.classificationLogged = true
	}
	return p, nil
}

func (p workerGitHubPolicy) authenticatedUserID(ctx context.Context, token string) (int64, error) {
	ctx, cancel := p.probeContext(ctx)
	defer cancel()
	body := bytes.NewBufferString(`{"query":"query WorkerCredentialIdentity { viewer { databaseId } }"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.GraphQLURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "detent-worker-github-governor")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workerGitHubRateLimitBodyMaxBytes))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("github authenticated user graphql probe returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data struct {
			Viewer struct {
				DatabaseID int64 `json:"databaseId"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("decode github authenticated user graphql probe: %w", err)
	}
	if len(payload.Errors) > 0 || payload.Data.Viewer.DatabaseID <= 0 {
		return 0, errors.New("github authenticated user graphql probe omitted the user id")
	}
	return payload.Data.Viewer.DatabaseID, nil
}

func (p workerGitHubPolicy) probe(ctx context.Context) (workerGitHubBudget, error) {
	ctx, cancel := p.probeContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.RateLimitURL, nil)
	if err != nil {
		return workerGitHubBudget{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	req.Header.Set("User-Agent", "detent-worker-github-governor")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return workerGitHubBudget{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workerGitHubRateLimitBodyMaxBytes))
	if err != nil {
		return workerGitHubBudget{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return workerGitHubBudget{}, fmt.Errorf("github rate limit probe returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Resources struct {
			Core struct {
				Limit     int64 `json:"limit"`
				Used      int64 `json:"used"`
				Remaining int64 `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return workerGitHubBudget{}, fmt.Errorf("decode github rate limit probe: %w", err)
	}
	if payload.Resources.Core.Limit <= 0 {
		return workerGitHubBudget{}, errors.New("github rate limit probe omitted the core budget")
	}
	return workerGitHubBudget{
		Limit:      payload.Resources.Core.Limit,
		Used:       payload.Resources.Core.Used,
		Remaining:  payload.Resources.Core.Remaining,
		ResetAt:    time.Unix(payload.Resources.Core.Reset, 0).UTC(),
		ObservedAt: time.Now().UTC(),
	}, nil
}

func (p workerGitHubPolicy) probeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.ProbeContext != nil {
		return p.ProbeContext(ctx)
	}
	return withWorkerGitHubProbeTimeout(ctx)
}

func (p workerGitHubPolicy) observe(budget workerGitHubBudget, onUpdate AgentUpdateHandler) error {
	consumer := telemetry.RESTConsumerWorker
	usageAttribution := "worker"
	if p.CredentialMode == workerGitHubCredentialShared {
		consumer = telemetry.RESTConsumerSharedPool
		usageAttribution = "indeterminate"
	}
	p.Logger.Info(
		"github rest budget summary",
		"consumer", consumer,
		"project_id", p.ProjectID,
		"issue_identifier", p.IssueIdentifier,
		"credential_identity", p.CredentialIdentity,
		"remaining", budget.Remaining,
		"used", budget.Used,
		"limit", budget.Limit,
		"reserve", p.MinRemaining,
		"usage_attribution", usageAttribution,
		"reset_at", budget.ResetAt,
	)
	if onUpdate == nil {
		return nil
	}
	return onUpdate(AgentUpdate{
		Type:       AgentUpdateRateLimits,
		RateLimits: &telemetry.RateLimits{GitHubRESTBudgets: []telemetry.RESTBudget{p.restBudget(budget)}},
	})
}

func (p workerGitHubPolicy) restBudget(budget workerGitHubBudget) telemetry.RESTBudget {
	resetAt := budget.ResetAt
	observedAt := budget.ObservedAt
	endpointFamily := "worker credential"
	if p.CredentialMode == workerGitHubCredentialShared {
		endpointFamily = "shared credential pool"
	}
	return telemetry.RESTBudget{
		Consumer:            p.budgetConsumer(),
		CredentialIdentity:  p.CredentialIdentity,
		EndpointFamily:      endpointFamily,
		Resource:            "core",
		Remaining:           budget.Remaining,
		Used:                budget.Used,
		Limit:               budget.Limit,
		MinRemainingReserve: p.MinRemaining,
		ResetAt:             &resetAt,
		ObservedAt:          &observedAt,
	}
}

func (p workerGitHubPolicy) reserveError(budget workerGitHubBudget) error {
	if p.MinRemaining <= 0 || budget.Remaining > p.MinRemaining {
		return nil
	}
	p.Logger.Warn(
		"worker github rest budget reached reserved headroom",
		"consumer", p.budgetConsumer(),
		"project_id", p.ProjectID,
		"issue_identifier", p.IssueIdentifier,
		"credential_identity", p.CredentialIdentity,
		"remaining", budget.Remaining,
		"worker_reserved_headroom", p.MinRemaining,
		"orchestrator_dispatch_floor", p.OrchestratorFloor,
		"limit", budget.Limit,
		"reset_at", budget.ResetAt,
	)
	return fmt.Errorf(
		"%w: consumer=%s credential_identity=%s remaining=%s reserve=%s reset_at=%s",
		ErrWorkerGitHubRESTReserved,
		p.budgetConsumer(),
		p.CredentialIdentity,
		strconv.FormatInt(budget.Remaining, 10),
		strconv.FormatInt(p.MinRemaining, 10),
		budget.ResetAt.Format(time.RFC3339),
	)
}

func (p workerGitHubPolicy) budgetConsumer() string {
	if p.CredentialMode == workerGitHubCredentialShared {
		return telemetry.RESTConsumerSharedPool
	}
	return telemetry.RESTConsumerWorker
}
