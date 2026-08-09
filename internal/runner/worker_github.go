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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const workerGitHubRateLimitBodyMaxBytes = 64 * 1024

var (
	ErrWorkerGitHubCredentialShared = errors.New("worker github credential shares the orchestrator credential")
	ErrWorkerGitHubRESTReserved     = errors.New("worker github REST budget reached reserved headroom")
	ErrWorkerGitHubBudgetMonitor    = errors.New("worker github REST budget monitor failed")
	workerGitHubEnvNamePattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type workerGitHubHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type workerGitHubPolicy struct {
	Enabled            bool
	Token              string
	OrchestratorToken  string
	CredentialIdentity string
	RateLimitURL       string
	GraphQLURL         string
	MinRemaining       int64
	PollInterval       time.Duration
	HTTPClient         workerGitHubHTTPClient
	Poll               <-chan time.Time
	Logger             *slog.Logger
	ProjectID          string
	IssueIdentifier    string
}

type workerGitHubBudget struct {
	Limit      int64
	Used       int64
	Remaining  int64
	ResetAt    time.Time
	ObservedAt time.Time
}

func (r *Runner) workerGitHubPolicy(cfg config.Config, issueIdentifier string) (workerGitHubPolicy, error) {
	return newWorkerGitHubPolicy(cfg, r.projectID, issueIdentifier, r.lookupEnv, nil, r.logger)
}

func workerGitHubPolicyName(policy workerGitHubPolicy) string {
	if policy.Enabled {
		return "dedicated"
	}
	return "isolated_disabled"
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

func newWorkerGitHubPolicy(cfg config.Config, projectID string, issueIdentifier string, lookupEnv func(string) string, httpClient workerGitHubHTTPClient, logger *slog.Logger) (workerGitHubPolicy, error) {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
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
			MinRemaining:    int64(cfg.Worker.GitHubRESTMinReserve),
			PollInterval:    time.Duration(cfg.Worker.GitHubRESTPollIntervalMS) * time.Millisecond,
			HTTPClient:      httpClient,
			Logger:          logger,
			ProjectID:       strings.TrimSpace(projectID),
			IssueIdentifier: strings.TrimSpace(issueIdentifier),
		}, nil
	}

	workerToken, err := resolveWorkerGitHubSecret(rawWorkerToken, lookupEnv)
	if err != nil {
		return workerGitHubPolicy{}, err
	}
	orchestratorToken, err := resolveWorkerGitHubSecret(strings.TrimSpace(cfg.Tracker.APIKey), lookupEnv)
	if err != nil {
		return workerGitHubPolicy{}, fmt.Errorf("resolve orchestrator github credential for worker isolation: %w", err)
	}
	if orchestratorToken == "" {
		orchestratorToken = strings.TrimSpace(lookupEnv("GITHUB_TOKEN"))
	}
	if orchestratorToken != "" && workerToken == orchestratorToken {
		return workerGitHubPolicy{}, ErrWorkerGitHubCredentialShared
	}

	rateLimitURL, graphQLURL, endpointIdentity, err := workerGitHubEndpoints(cfg.Tracker.Endpoint)
	if err != nil {
		return workerGitHubPolicy{}, err
	}
	sum := sha256.Sum256([]byte(endpointIdentity + "\x00" + workerToken))
	return workerGitHubPolicy{
		Enabled:            true,
		Token:              workerToken,
		OrchestratorToken:  orchestratorToken,
		CredentialIdentity: fmt.Sprintf("github-rest:%x", sum[:6]),
		RateLimitURL:       rateLimitURL,
		GraphQLURL:         graphQLURL,
		MinRemaining:       int64(cfg.Worker.GitHubRESTMinReserve),
		PollInterval:       time.Duration(cfg.Worker.GitHubRESTPollIntervalMS) * time.Millisecond,
		HTTPClient:         httpClient,
		Logger:             logger,
		ProjectID:          strings.TrimSpace(projectID),
		IssueIdentifier:    strings.TrimSpace(issueIdentifier),
	}, nil
}

func resolveWorkerGitHubSecret(value string, lookupEnv func(string) string) (string, error) {
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
	if err := policy.verifyDistinctPrincipal(ctx); err != nil {
		return ctx, func() error { return nil }, err
	}
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

func (p workerGitHubPolicy) verifyDistinctPrincipal(ctx context.Context) error {
	workerID, err := p.authenticatedUserID(ctx, p.Token)
	if err != nil {
		return fmt.Errorf("%w: verify worker credential principal: %w", ErrWorkerGitHubBudgetMonitor, err)
	}
	if strings.TrimSpace(p.OrchestratorToken) == "" {
		return nil
	}
	orchestratorID, err := p.authenticatedUserID(ctx, p.OrchestratorToken)
	if err != nil {
		return fmt.Errorf("%w: verify orchestrator credential principal: %w", ErrWorkerGitHubBudgetMonitor, err)
	}
	if workerID == orchestratorID {
		return ErrWorkerGitHubCredentialShared
	}
	return nil
}

func (p workerGitHubPolicy) authenticatedUserID(ctx context.Context, token string) (int64, error) {
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

func (p workerGitHubPolicy) observe(budget workerGitHubBudget, onUpdate AgentUpdateHandler) error {
	p.Logger.Info(
		"github rest budget summary",
		"consumer", telemetry.RESTConsumerWorker,
		"project_id", p.ProjectID,
		"issue_identifier", p.IssueIdentifier,
		"credential_identity", p.CredentialIdentity,
		"remaining", budget.Remaining,
		"used", budget.Used,
		"limit", budget.Limit,
		"reserve", p.MinRemaining,
		"reset_at", budget.ResetAt,
	)
	if onUpdate == nil {
		return nil
	}
	resetAt := budget.ResetAt
	observedAt := budget.ObservedAt
	return onUpdate(AgentUpdate{
		Type: AgentUpdateRateLimits,
		RateLimits: &telemetry.RateLimits{GitHubRESTBudgets: []telemetry.RESTBudget{{
			Consumer:            telemetry.RESTConsumerWorker,
			CredentialIdentity:  p.CredentialIdentity,
			EndpointFamily:      "worker credential",
			Resource:            "core",
			Remaining:           budget.Remaining,
			Used:                budget.Used,
			Limit:               budget.Limit,
			MinRemainingReserve: p.MinRemaining,
			ResetAt:             &resetAt,
			ObservedAt:          &observedAt,
		}}},
	})
}

func (p workerGitHubPolicy) reserveError(budget workerGitHubBudget) error {
	if p.MinRemaining <= 0 || budget.Remaining > p.MinRemaining {
		return nil
	}
	p.Logger.Warn(
		"worker github rest budget reached reserved headroom",
		"consumer", telemetry.RESTConsumerWorker,
		"project_id", p.ProjectID,
		"issue_identifier", p.IssueIdentifier,
		"credential_identity", p.CredentialIdentity,
		"remaining", budget.Remaining,
		"orchestrator_reserved_headroom", p.MinRemaining,
		"limit", budget.Limit,
		"reset_at", budget.ResetAt,
	)
	return fmt.Errorf("%w: remaining=%s reserve=%s reset_at=%s", ErrWorkerGitHubRESTReserved, strconv.FormatInt(budget.Remaining, 10), strconv.FormatInt(p.MinRemaining, 10), budget.ResetAt.Format(time.RFC3339))
}
