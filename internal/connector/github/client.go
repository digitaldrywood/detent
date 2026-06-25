package github

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	DefaultGraphQLEndpoint = "https://api.github.com/graphql"
	gitHubAPIVersion       = "2026-03-10"
	maxErrorBodyBytes      = 1000
)

const (
	restRateLimitKindPrimaryExhausted   = "primary_exhausted"
	restRateLimitKindSecondaryThrottled = "secondary_throttled"
)

var defaultRESTBackoffs = newRESTBackoffRegistry()

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ClientConfig struct {
	Endpoint    string
	TokenSource TokenSource
	HTTPClient  HTTPClient
	RESTPolicy  RESTBudgetPolicy
	Logger      *slog.Logger
}

type RESTBudgetPolicy struct {
	MinRemainingReserve int64
	FanoutMaxRequests   int64
}

type Client struct {
	endpoint         string
	restEndpoint     string
	tokenSource      TokenSource
	httpClient       HTTPClient
	restPolicy       RESTBudgetPolicy
	logger           *slog.Logger
	mu               sync.RWMutex
	rateLimit        connector.GraphQLRateLimit
	queryCosts       map[string]connector.GraphQLQueryCost
	hasRateLimit     bool
	restRateLimit    connector.RESTRateLimit
	restRequests     map[string]connector.RESTEndpointUsage
	restBackoffUntil time.Time
	restBackoffKey   string
	restBackoffs     *restBackoffRegistry
	hasRestRateLimit bool
	authHealth       connector.AuthHealth
	hasAuthHealth    bool
}

type restProbeResult struct {
	StatusCode int
	Headers    http.Header
	Body       string
	FullBody   string
}

func NewClient(cfg ClientConfig) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultGraphQLEndpoint
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	restEndpoint, err := restEndpointFromGraphQLEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.TokenSource == nil {
		return nil, ErrMissingToken
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = NewPooledHTTPClient(HTTPTransportConfig{})
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Client{
		endpoint:     endpoint,
		restEndpoint: restEndpoint,
		tokenSource:  cfg.TokenSource,
		httpClient:   httpClient,
		restPolicy:   normalizeRESTBudgetPolicy(cfg.RESTPolicy),
		logger:       logger,
		restBackoffs: defaultRESTBackoffs,
	}, nil
}

func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	return c.GraphQLWithType(ctx, "", query, variables, out)
}

func (c *Client) GraphQLWithType(ctx context.Context, queryType string, query string, variables map[string]any, out any) error {
	return c.graphQLWithType(ctx, queryType, query, variables, out, true)
}

func (c *Client) graphQLWithType(ctx context.Context, queryType string, query string, variables map[string]any, out any, allowTokenRefresh bool) error {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("resolve github token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}

	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return fmt.Errorf("encode github graphql request: %w", err)
	}

	queryType = graphQLQueryType(queryType, query)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, &body)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)

	operation := firstLine(query)
	c.logger.DebugContext(ctx, "github graphql request", "operation", operation, "live_connections", c.LiveConnections())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		if err := drainAndClose(resp.Body); err != nil {
			c.logger.DebugContext(ctx, "github graphql response body drain failed", "operation", operation, "error", err)
		}
	}()

	c.logger.DebugContext(ctx, "github graphql response", "operation", operation, "status", resp.StatusCode, "live_connections", c.LiveConnections())

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}
	receivedAt := time.Now()
	headerRateLimit := c.recordRateLimitFromHeaders(resp.Header, receivedAt)

	if resp.StatusCode != http.StatusOK {
		err := classifyStatusAt(resp.StatusCode, resp.Header, raw, receivedAt)
		if c.refreshAfterAuthFailure(ctx, err, allowTokenRefresh) {
			return c.graphQLWithType(ctx, queryType, query, variables, out, false)
		}
		return err
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if !c.recordRateLimitFromData(envelope.Data, queryType, receivedAt) {
		c.recordGraphQLQueryCostFromHeaders(queryType, headerRateLimit)
	}
	if len(envelope.Errors) > 0 {
		err := classifyGraphQLErrors(envelope.Errors)
		if c.refreshAfterAuthFailure(ctx, err, allowTokenRefresh) {
			return c.graphQLWithType(ctx, queryType, query, variables, out, false)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}

	return nil
}

func (c *Client) REST(ctx context.Context, method string, path string, body any, out any) error {
	_, err := c.rest(ctx, method, path, body, out)
	return err
}

func (c *Client) restProbe(ctx context.Context, method string, path string, body any) (restProbeResult, error) {
	return c.restProbeWithTokenRefresh(ctx, method, path, body, true)
}

func (c *Client) restProbeWithTokenRefresh(ctx context.Context, method string, path string, body any, allowTokenRefresh bool) (restProbeResult, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return restProbeResult{}, fmt.Errorf("resolve github token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return restProbeResult{}, ErrMissingToken
	}
	backoffKey := c.restSharedBackoffKey(token)
	c.rememberRESTBackoffKey(backoffKey)

	var requestBody io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return restProbeResult{}, fmt.Errorf("encode github rest request: %w", err)
		}
		requestBody = &encoded
	}

	url, err := c.restURL(path)
	if err != nil {
		return restProbeResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return restProbeResult{}, fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return restProbeResult{}, ctxErr
		}
		return restProbeResult{}, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		if err := drainAndClose(resp.Body); err != nil {
			c.logger.DebugContext(ctx, "github rest probe response body drain failed", "method", method, "path", path, "error", err)
		}
	}()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return restProbeResult{}, fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}
	receivedAt := time.Now()
	c.recordRESTRateLimitFromHeaders(backoffKey, method, path, resp.StatusCode, resp.Header, receivedAt)
	result := restProbeResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       summarizeBody(raw),
		FullBody:   string(bytes.TrimSpace(raw)),
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		err := classifyStatusAt(resp.StatusCode, resp.Header, raw, receivedAt)
		if c.refreshAfterAuthFailure(ctx, err, allowTokenRefresh) {
			return c.restProbeWithTokenRefresh(ctx, method, path, body, false)
		}
	}
	return result, nil
}

func (c *Client) rest(ctx context.Context, method string, path string, body any, out any) (http.Header, error) {
	return c.restWithTokenRefresh(ctx, method, path, body, out, true)
}

func (c *Client) restWithTokenRefresh(ctx context.Context, method string, path string, body any, out any, allowTokenRefresh bool) (http.Header, error) {
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve github token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrMissingToken
	}
	backoffKey := c.restSharedBackoffKey(token)
	c.rememberRESTBackoffKey(backoffKey)
	if err := c.restBackoffError(backoffKey, time.Now()); err != nil {
		return nil, err
	}
	if err := c.restBudgetPolicyError(method, path); err != nil {
		return nil, err
	}

	var requestBody io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return nil, fmt.Errorf("encode github rest request: %w", err)
		}
		requestBody = &encoded
	}

	url, err := c.restURL(path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.logger.DebugContext(ctx, "github rest request", "method", method, "path", path, "live_connections", c.LiveConnections())
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		if err := drainAndClose(resp.Body); err != nil {
			c.logger.DebugContext(ctx, "github rest response body drain failed", "method", method, "path", path, "error", err)
		}
	}()

	c.logger.DebugContext(ctx, "github rest response", "method", method, "path", path, "status", resp.StatusCode, "live_connections", c.LiveConnections())
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}
	receivedAt := time.Now()
	c.recordRESTRateLimitFromHeaders(backoffKey, method, path, resp.StatusCode, resp.Header, receivedAt)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := classifyStatusAt(resp.StatusCode, resp.Header, raw, receivedAt)
		if c.refreshAfterAuthFailure(ctx, err, allowTokenRefresh) {
			return c.restWithTokenRefresh(ctx, method, path, body, out, false)
		}
		return nil, err
	}
	headers := resp.Header.Clone()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return headers, nil
	}
	if len(raw) == 0 {
		return nil, ErrInvalidResponse
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	return headers, nil
}

func (c *Client) AuthHealth() (connector.AuthHealth, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.authHealth, c.hasAuthHealth
}

func (c *Client) refreshAfterAuthFailure(ctx context.Context, err error, allowTokenRefresh bool) bool {
	if !errors.Is(err, ErrAuthenticationFailed) {
		return false
	}
	c.recordAuthFailure(err, time.Now())
	if !allowTokenRefresh {
		return false
	}
	refresher, ok := c.tokenSource.(RefreshableTokenSource)
	if !ok {
		return false
	}
	if _, refreshErr := refresher.RefreshToken(ctx); refreshErr != nil {
		c.logger.WarnContext(ctx, "github token refresh failed", "error", refreshErr)
		return false
	}
	c.recordAuthRecovered(time.Now())
	return true
}

func (c *Client) recordAuthFailure(err error, at time.Time) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.authHealth.Status = connector.AuthStatusStale
	c.authHealth.LastError = err.Error()
	c.authHealth.LastErrorAt = at
	c.authHealth.LastRecoveredAt = time.Time{}
	c.hasAuthHealth = true
}

func (c *Client) recordAuthRecovered(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.authHealth.Status = connector.AuthStatusRecovered
	c.authHealth.LastRecoveredAt = at
	c.hasAuthHealth = true
}

func (c *Client) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.rateLimit, c.hasRateLimit
}

func (c *Client) ResetGraphQLRateLimitUsage() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.queryCosts = nil
}

func (c *Client) FlushGraphQLRateLimitUsage() connector.GraphQLRateLimitUsage {
	c.mu.Lock()
	defer c.mu.Unlock()

	usage := connector.GraphQLRateLimitUsage{
		RateLimit:    c.rateLimit,
		HasRateLimit: c.hasRateLimit,
		QueryCosts:   sortedGraphQLQueryCosts(c.queryCosts),
	}
	for _, cost := range usage.QueryCosts {
		usage.TotalQueries += cost.Count
		usage.TotalCost += cost.Cost
	}
	c.queryCosts = nil
	return usage
}

func (c *Client) FlushRESTRateLimitUsage() connector.RESTRateLimitUsage {
	c.mu.Lock()
	defer c.mu.Unlock()

	backoffUntil := c.restBackoffUntil
	if c.restBackoffs != nil && c.restBackoffKey != "" {
		if shared := c.restBackoffs.until(c.restBackoffKey, time.Now()); shared.After(backoffUntil) {
			backoffUntil = shared
		}
	}
	usage := connector.RESTRateLimitUsage{
		RateLimit:    c.restRateLimit,
		HasRateLimit: c.hasRestRateLimit,
		Requests:     sortedRESTEndpointUsages(c.restRequests),
		BackoffUntil: backoffUntil,
	}
	for _, request := range usage.Requests {
		usage.TotalRequests += request.Count
		if request.RateLimited {
			usage.RateLimited = true
		}
	}
	c.restRequests = nil
	return usage
}

func (c *Client) restBackoffError(backoffKey string, now time.Time) error {
	c.mu.RLock()
	backoffUntil := c.restBackoffUntil
	c.mu.RUnlock()
	if c.restBackoffs != nil && backoffKey != "" {
		if shared := c.restBackoffs.until(backoffKey, now); shared.After(backoffUntil) {
			backoffUntil = shared
		}
	}
	if backoffUntil.IsZero() || !backoffUntil.After(now) {
		return nil
	}
	return &StatusError{
		StatusCode:    http.StatusTooManyRequests,
		Err:           ErrRateLimited,
		RateLimitKind: restRateLimitKindPrimaryExhausted,
		RetryAfter:    backoffUntil.Sub(now),
	}
}

func normalizeRESTBudgetPolicy(policy RESTBudgetPolicy) RESTBudgetPolicy {
	if policy.MinRemainingReserve < 0 {
		policy.MinRemainingReserve = 0
	}
	if policy.FanoutMaxRequests < 0 {
		policy.FanoutMaxRequests = 0
	}
	return policy
}

func (c *Client) restBudgetPolicyError(method string, path string) error {
	family := restEndpointFamily(method, path)
	if !restFanoutEndpointFamily(family) {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.restPolicy.FanoutMaxRequests > 0 && c.restFanoutRequestCountLocked() >= c.restPolicy.FanoutMaxRequests {
		c.recordRESTBudgetThrottleLocked(method, path, family)
		return &StatusError{
			StatusCode: http.StatusTooManyRequests,
			Body:       "REST fanout request cap reached for " + family,
			Err:        ErrRESTBudgetReserved,
		}
	}
	if c.restPolicy.MinRemainingReserve > 0 &&
		c.hasRestRateLimit &&
		c.restRateLimit.Limit > 0 &&
		c.restRateLimit.Remaining <= c.restPolicy.MinRemainingReserve {
		c.recordRESTBudgetThrottleLocked(method, path, family)
		return &StatusError{
			StatusCode: http.StatusTooManyRequests,
			Body:       "REST remaining budget is reserved for shared GitHub work",
			Err:        ErrRESTBudgetReserved,
		}
	}
	return nil
}

func (c *Client) restFanoutRequestCountLocked() int64 {
	var count int64
	for family, request := range c.restRequests {
		if restFanoutEndpointFamily(family) {
			count += request.Count
		}
	}
	return count
}

func (c *Client) recordRESTBudgetThrottleLocked(method string, path string, family string) {
	if c.restRequests == nil {
		c.restRequests = make(map[string]connector.RESTEndpointUsage)
	}
	request := c.restRequests[family]
	request.EndpointFamily = family
	request.Count++
	request.RateLimited = true
	request.LastStatus = http.StatusTooManyRequests
	if c.hasRestRateLimit {
		request.Limit = c.restRateLimit.Limit
		request.Used = c.restRateLimit.Used
		request.Remaining = c.restRateLimit.Remaining
		request.Resource = c.restRateLimit.Resource
		request.ResetAt = c.restRateLimit.ResetAt
		request.RetryAfter = c.restRateLimit.RetryAfter
	}
	c.restRequests[family] = request
	c.logger.Warn(
		"github rest budget preserved",
		"method", strings.ToUpper(strings.TrimSpace(method)),
		"path", path,
		"endpoint_family", family,
		"remaining", c.restRateLimit.Remaining,
		"reserve", c.restPolicy.MinRemainingReserve,
		"fanout_cap", c.restPolicy.FanoutMaxRequests,
	)
}

func (c *Client) restSharedBackoffKey(token string) string {
	sum := sha256.Sum256([]byte(c.restEndpoint + "\x00" + token))
	return c.restEndpoint + "\x00" + string(sum[:])
}

func (c *Client) rememberRESTBackoffKey(backoffKey string) {
	if strings.TrimSpace(backoffKey) == "" {
		return
	}
	c.mu.Lock()
	c.restBackoffKey = backoffKey
	c.mu.Unlock()
}

func (c *Client) recordRESTRateLimitFromHeaders(backoffKey string, method string, path string, status int, headers http.Header, now time.Time) {
	limit, hasLimit := int64Header(headers, "X-RateLimit-Limit")
	used, hasUsed := int64Header(headers, "X-RateLimit-Used")
	remaining, hasRemaining := int64Header(headers, "X-RateLimit-Remaining")
	reset, hasReset := int64Header(headers, "X-RateLimit-Reset")
	retryAfter, hasRetryAfter := parseRetryAfter(headers.Get("Retry-After"), now)
	resource := strings.TrimSpace(headers.Get("X-RateLimit-Resource"))
	rateLimited := restStatusRateLimited(status, headers)
	family := restEndpointFamily(method, path)
	sharedBackoff := rateLimited && restShouldApplySharedBackoff(family, remaining, hasRemaining)

	var resetAt time.Time
	if hasReset {
		resetAt = time.Unix(reset, 0).UTC()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if backoffKey != "" {
		c.restBackoffKey = backoffKey
	}
	snapshot := c.restRateLimit
	hasSnapshot := false
	if hasLimit {
		snapshot.Limit = limit
		hasSnapshot = true
	}
	if hasUsed {
		snapshot.Used = used
		hasSnapshot = true
	}
	if hasRemaining {
		snapshot.Remaining = remaining
		hasSnapshot = true
	}
	if hasReset {
		snapshot.ResetAt = resetAt
		hasSnapshot = true
	}
	if resource != "" {
		snapshot.Resource = resource
		hasSnapshot = true
	}
	if hasRetryAfter && sharedBackoff {
		snapshot.RetryAfter = retryAfter
		hasSnapshot = true
	} else if hasSnapshot {
		snapshot.RetryAfter = 0
	}
	if hasSnapshot {
		snapshot.UpdatedAt = now
		c.restRateLimit = snapshot
		c.hasRestRateLimit = true
	}

	if c.restRequests == nil {
		c.restRequests = make(map[string]connector.RESTEndpointUsage)
	}
	request := c.restRequests[family]
	request.EndpointFamily = family
	request.Count++
	request.LastStatus = status
	request.RateLimited = request.RateLimited || rateLimited
	if hasLimit {
		request.Limit = limit
	}
	if hasUsed {
		request.Used = used
	}
	if hasRemaining {
		request.Remaining = remaining
	}
	if hasReset {
		request.ResetAt = resetAt
	}
	if resource != "" {
		request.Resource = resource
	}
	if hasRetryAfter {
		request.RetryAfter = retryAfter
	}
	c.restRequests[family] = request

	if sharedBackoff {
		backoffUntil := restBackoffUntil(now, retryAfter, hasRetryAfter, resetAt, hasReset, remaining, hasRemaining)
		if c.restBackoffUntil.IsZero() || backoffUntil.After(c.restBackoffUntil) {
			c.restBackoffUntil = backoffUntil
		}
		if c.restBackoffs != nil && backoffKey != "" {
			c.restBackoffs.set(backoffKey, backoffUntil)
		}
	}
}

func (c *Client) recordRateLimitFromData(data json.RawMessage, queryType string, now time.Time) bool {
	if len(data) == 0 {
		return false
	}

	var envelope struct {
		RateLimit *struct {
			Limit     int64  `json:"limit"`
			Used      int64  `json:"used"`
			Remaining int64  `json:"remaining"`
			Cost      int64  `json:"cost"`
			ResetAt   string `json:"resetAt"`
		} `json:"rateLimit"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.RateLimit == nil {
		return false
	}

	var resetAt time.Time
	if value := strings.TrimSpace(envelope.RateLimit.ResetAt); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err == nil {
			resetAt = parsed
		}
	}
	c.setRateLimit(connector.GraphQLRateLimit{
		Limit:     envelope.RateLimit.Limit,
		Used:      envelope.RateLimit.Used,
		Remaining: envelope.RateLimit.Remaining,
		Cost:      envelope.RateLimit.Cost,
		ResetAt:   resetAt,
		UpdatedAt: now,
	})
	c.addGraphQLQueryCost(queryType, envelope.RateLimit.Cost)
	return true
}

type graphQLHeaderRateLimit struct {
	Previous           connector.GraphQLRateLimit
	Current            connector.GraphQLRateLimit
	HasPrevious        bool
	HasCurrent         bool
	HasPrimarySnapshot bool
}

func (c *Client) recordRateLimitFromHeaders(headers http.Header, now time.Time) graphQLHeaderRateLimit {
	limit, hasLimit := int64Header(headers, "X-RateLimit-Limit")
	used, hasUsed := int64Header(headers, "X-RateLimit-Used")
	remaining, hasRemaining := int64Header(headers, "X-RateLimit-Remaining")
	reset, hasReset := int64Header(headers, "X-RateLimit-Reset")
	retryAfter, hasRetryAfter := parseRetryAfter(headers.Get("Retry-After"), now)

	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.rateLimit
	hasPrevious := c.hasRateLimit
	snapshot := previous
	hasSnapshot := false
	hasPrimarySnapshot := false
	if hasLimit {
		snapshot.Limit = limit
		hasSnapshot = true
		hasPrimarySnapshot = true
	}
	if hasUsed {
		snapshot.Used = used
		hasSnapshot = true
		hasPrimarySnapshot = true
	}
	if hasRemaining {
		snapshot.Remaining = remaining
		hasSnapshot = true
		hasPrimarySnapshot = true
	}
	if hasReset {
		snapshot.ResetAt = time.Unix(reset, 0).UTC()
		hasSnapshot = true
		hasPrimarySnapshot = true
	}
	if hasPrimarySnapshot {
		snapshot.Cost = 0
	}
	if hasRetryAfter {
		snapshot.RetryAfter = retryAfter
		hasSnapshot = true
	} else if hasSnapshot {
		snapshot.RetryAfter = 0
	}

	if hasSnapshot && !stalePrimaryRateLimitSnapshot(previous, snapshot, hasPrevious, hasPrimarySnapshot) {
		snapshot.UpdatedAt = now
		c.rateLimit = snapshot
		c.hasRateLimit = true
	}
	return graphQLHeaderRateLimit{
		Previous:           previous,
		Current:            snapshot,
		HasPrevious:        hasPrevious,
		HasCurrent:         hasSnapshot,
		HasPrimarySnapshot: hasPrimarySnapshot,
	}
}

func stalePrimaryRateLimitSnapshot(
	previous connector.GraphQLRateLimit,
	snapshot connector.GraphQLRateLimit,
	hasPrevious bool,
	hasPrimarySnapshot bool,
) bool {
	if !hasPrevious || !hasPrimarySnapshot {
		return false
	}
	if previous.Limit <= 0 || snapshot.Limit <= 0 {
		return false
	}
	if !previous.ResetAt.IsZero() && !snapshot.ResetAt.IsZero() && !previous.ResetAt.Equal(snapshot.ResetAt) {
		return false
	}
	return snapshot.Used < previous.Used
}

func (c *Client) LiveConnections() int {
	if c == nil || c.httpClient == nil {
		return 0
	}
	stats, ok := c.httpClient.(interface {
		LiveConnections() int
	})
	if !ok {
		return 0
	}
	return stats.LiveConnections()
}

func (c *Client) Close() error {
	if c == nil || c.httpClient == nil {
		return nil
	}
	if closer, ok := c.httpClient.(interface {
		Close() error
	}); ok {
		return closer.Close()
	}
	if closer, ok := c.httpClient.(interface {
		CloseIdleConnections()
	}); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

func (c *Client) setRateLimit(snapshot connector.GraphQLRateLimit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rateLimit = snapshot
	c.hasRateLimit = true
}

func (c *Client) addGraphQLQueryCost(queryType string, cost int64) {
	queryType = strings.TrimSpace(queryType)
	if queryType == "" || cost < 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.queryCosts == nil {
		c.queryCosts = make(map[string]connector.GraphQLQueryCost)
	}
	current := c.queryCosts[queryType]
	current.QueryType = queryType
	current.Count++
	current.Cost += cost
	c.queryCosts[queryType] = current
}

func (c *Client) recordGraphQLQueryCostFromHeaders(queryType string, snapshot graphQLHeaderRateLimit) {
	if !snapshot.HasCurrent || !snapshot.HasPrevious || !snapshot.HasPrimarySnapshot {
		return
	}
	if snapshot.Current.Limit <= 0 || snapshot.Previous.Limit <= 0 {
		return
	}
	if !snapshot.Current.ResetAt.IsZero() && !snapshot.Previous.ResetAt.IsZero() && !snapshot.Current.ResetAt.Equal(snapshot.Previous.ResetAt) {
		return
	}

	cost := snapshot.Current.Used - snapshot.Previous.Used
	if cost < 0 {
		cost = snapshot.Previous.Remaining - snapshot.Current.Remaining
	}
	if cost < 0 {
		cost = 0
	}
	c.addGraphQLQueryCost(queryType, cost)
}

func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%w: %s", ErrInvalidEndpoint, endpoint)
	}
	return nil
}

func restEndpointFromGraphQLEndpoint(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}

	basePath := strings.TrimRight(parsed.Path, "/")
	if parsed.Host == "api.github.com" {
		basePath = ""
	} else if before, ok := strings.CutSuffix(basePath, "/api/graphql"); ok {
		basePath = before + "/api/v3"
	} else if before, ok := strings.CutSuffix(basePath, "/graphql"); ok {
		basePath = before
	}

	parsed.Path = strings.TrimRight(basePath, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *Client) restURL(path string) (string, error) {
	parsed, err := url.Parse(c.restEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ErrInvalidEndpoint
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	relative, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + relative.Path
	parsed.RawQuery = relative.RawQuery
	return parsed.String(), nil
}

func (c *Client) nextRESTPage(headers http.Header) (string, error) {
	link := nextRESTLink(headers)
	if link == "" {
		return "", nil
	}
	return c.restPath(link)
}

func nextRESTLink(headers http.Header) string {
	for _, value := range headers.Values("Link") {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "<") {
				continue
			}
			end := strings.Index(part, ">")
			if end <= 1 {
				continue
			}
			if linkHasRelNext(part[end+1:]) {
				return strings.TrimSpace(part[1:end])
			}
		}
	}
	return ""
}

func linkHasRelNext(params string) bool {
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
			continue
		}
		if strings.EqualFold(strings.Trim(strings.TrimSpace(value), `"`), "next") {
			return true
		}
	}
	return false
}

func (c *Client) restPath(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	if !parsed.IsAbs() {
		if parsed.Path == "" {
			return "", ErrInvalidEndpoint
		}
		return requestPath(parsed), nil
	}

	base, err := url.Parse(c.restEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	if !strings.EqualFold(parsed.Scheme, base.Scheme) || !strings.EqualFold(parsed.Host, base.Host) {
		return "", fmt.Errorf("%w: unexpected rest page host %s", ErrInvalidEndpoint, parsed.Host)
	}

	path := parsed.Path
	basePath := strings.TrimRight(base.Path, "/")
	if basePath != "" {
		switch {
		case path == basePath:
			path = "/"
		case strings.HasPrefix(path, basePath+"/"):
			path = strings.TrimPrefix(path, basePath)
		default:
			return "", fmt.Errorf("%w: unexpected rest page path %s", ErrInvalidEndpoint, path)
		}
	}
	parsed.Path = path
	return requestPath(parsed), nil
}

func requestPath(value *url.URL) string {
	path := value.EscapedPath()
	if path == "" {
		path = "/"
	}
	if value.RawQuery != "" {
		path += "?" + value.RawQuery
	}
	return path
}

func classifyStatus(status int, headers http.Header, body []byte) error {
	return classifyStatusAt(status, headers, body, time.Now())
}

func classifyStatusAt(status int, headers http.Header, body []byte, now time.Time) error {
	base := ErrUnexpectedStatus
	switch {
	case status == http.StatusUnauthorized:
		base = ErrAuthenticationFailed
	case status == http.StatusForbidden:
		if strings.TrimSpace(headers.Get("Retry-After")) != "" || headers.Get("X-RateLimit-Remaining") == "0" {
			base = ErrRateLimited
		} else {
			base = ErrAuthenticationFailed
		}
	case status == http.StatusNotFound:
		base = ErrNotFound
	case status == http.StatusTooManyRequests:
		base = ErrRateLimited
	case status >= http.StatusInternalServerError:
		base = ErrTransient
	}

	statusErr := &StatusError{
		StatusCode: status,
		Body:       summarizeBody(body),
		Err:        base,
	}
	if errors.Is(base, ErrRateLimited) {
		statusErr.RateLimitKind = restRateLimitKindFromHeaders(status, headers)
		statusErr.RetryAfter, _ = parseRetryAfter(headers.Get("Retry-After"), now)
		if reset, ok := int64Header(headers, "X-RateLimit-Reset"); ok {
			statusErr.ResetAt = time.Unix(reset, 0).UTC()
		}
	}
	return statusErr
}

func restStatusRateLimited(status int, headers http.Header) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return strings.TrimSpace(headers.Get("Retry-After")) != "" || headers.Get("X-RateLimit-Remaining") == "0"
	default:
		return false
	}
}

func restRateLimitKindFromHeaders(status int, headers http.Header) string {
	if !restStatusRateLimited(status, headers) {
		return ""
	}
	if remaining, ok := int64Header(headers, "X-RateLimit-Remaining"); ok && remaining <= 0 {
		return restRateLimitKindPrimaryExhausted
	}
	return restRateLimitKindSecondaryThrottled
}

func restShouldApplySharedBackoff(family string, remaining int64, hasRemaining bool) bool {
	if !restFanoutEndpointFamily(family) {
		return true
	}
	return hasRemaining && remaining <= 0
}

func restBackoffUntil(
	now time.Time,
	retryAfter time.Duration,
	hasRetryAfter bool,
	resetAt time.Time,
	hasReset bool,
	remaining int64,
	hasRemaining bool,
) time.Time {
	if hasRetryAfter {
		return now.Add(retryAfter)
	}
	if hasRemaining && remaining == 0 && hasReset && resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Minute)
}

func int64Header(headers http.Header, name string) (int64, bool) {
	value := strings.TrimSpace(headers.Get(name))
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}

	resetAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if resetAt.Before(now) {
		return 0, true
	}
	return resetAt.Sub(now), true
}

func classifyGraphQLErrors(errors []GraphQLError) error {
	base := ErrGraphQLErrors
	for _, err := range errors {
		message := strings.ToLower(err.Message)
		errorType := strings.ToUpper(err.Type)
		switch {
		case errorType == "RATE_LIMITED" || strings.Contains(message, "rate limit"):
			base = ErrRateLimited
		case errorType == "NOT_FOUND" || strings.Contains(message, "not found"):
			base = ErrNotFound
		case strings.Contains(message, "authentication") || strings.Contains(message, "bad credentials"):
			base = ErrAuthenticationFailed
		}
	}

	return &GraphQLErrorList{
		Errors: errors,
		Err:    base,
	}
}

func summarizeBody(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) <= maxErrorBodyBytes {
		return string(body)
	}
	return string(body[:maxErrorBodyBytes]) + "...<truncated>"
}

func firstLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func graphQLQueryType(queryType string, query string) string {
	queryType = strings.TrimSpace(queryType)
	if queryType != "" {
		return queryType
	}

	operation := firstLine(query)
	parts := strings.Fields(operation)
	if len(parts) >= 2 {
		name := strings.TrimSpace(parts[1])
		if index := strings.Index(name, "("); index >= 0 {
			name = name[:index]
		}
		name = strings.Trim(name, "{}")
		if name != "" {
			return name
		}
	}
	return "graphql"
}

func sortedGraphQLQueryCosts(costs map[string]connector.GraphQLQueryCost) []connector.GraphQLQueryCost {
	if len(costs) == 0 {
		return nil
	}

	out := make([]connector.GraphQLQueryCost, 0, len(costs))
	for _, cost := range costs {
		out = append(out, cost)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].QueryType < out[j].QueryType
	})
	return out
}

func sortedRESTEndpointUsages(usages map[string]connector.RESTEndpointUsage) []connector.RESTEndpointUsage {
	if len(usages) == 0 {
		return nil
	}

	out := make([]connector.RESTEndpointUsage, 0, len(usages))
	for _, usage := range usages {
		out = append(out, usage)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].RateLimited != out[j].RateLimited {
			return !out[i].RateLimited
		}
		return out[i].EndpointFamily < out[j].EndpointFamily
	})
	return out
}

type restBackoffRegistry struct {
	mu     sync.RWMutex
	untils map[string]time.Time
}

func newRESTBackoffRegistry() *restBackoffRegistry {
	return &restBackoffRegistry{untils: map[string]time.Time{}}
}

func (r *restBackoffRegistry) until(key string, now time.Time) time.Time {
	if r == nil || strings.TrimSpace(key) == "" {
		return time.Time{}
	}
	r.mu.RLock()
	until := r.untils[key]
	r.mu.RUnlock()
	if until.IsZero() || until.After(now) {
		return until
	}
	r.mu.Lock()
	if current := r.untils[key]; !current.IsZero() && !current.After(now) {
		delete(r.untils, key)
	}
	r.mu.Unlock()
	return time.Time{}
}

func (r *restBackoffRegistry) set(key string, until time.Time) {
	if r == nil || strings.TrimSpace(key) == "" || until.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.untils[key]; current.IsZero() || until.After(current) {
		r.untils[key] = until
	}
}

func restEndpointFamily(method string, path string) string {
	if strings.ToUpper(strings.TrimSpace(method)) != http.MethodGet {
		return "mutations"
	}

	parsed, err := url.Parse(strings.TrimSpace(path))
	if err != nil {
		return "other"
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) >= 2 && segments[0] == "search" && segments[1] == "issues" {
		return "search"
	}
	if len(segments) < 4 || segments[0] != "repos" {
		return "other"
	}
	switch {
	case len(segments) == 4 && segments[3] == "issues" && parsed.Query().Get("labels") != "":
		return "label issues"
	case len(segments) == 5 && segments[3] == "issues" && segments[4] == "comments":
		return "issue comments"
	case len(segments) == 6 && segments[3] == "issues" && segments[5] == "comments":
		return "issue comments"
	case len(segments) == 5 && segments[3] == "issues":
		return "issue reads"
	case len(segments) == 4 && segments[3] == "pulls":
		return "pull requests"
	case len(segments) == 5 && segments[3] == "pulls":
		return "pull requests"
	case len(segments) == 6 && segments[3] == "pulls" && segments[5] == "reviews":
		return "reviews"
	case len(segments) == 6 && segments[3] == "commits" && segments[5] == "check-runs":
		return "check runs"
	case len(segments) == 6 && segments[3] == "commits" && segments[5] == "statuses":
		return "commit statuses"
	default:
		return "other"
	}
}

func restFanoutEndpointFamily(family string) bool {
	switch family {
	case "pull requests", "check runs", "commit statuses", "reviews":
		return true
	default:
		return false
	}
}
