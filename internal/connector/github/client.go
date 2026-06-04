package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	DefaultGraphQLEndpoint = "https://api.github.com/graphql"
	gitHubAPIVersion       = "2022-11-28"
	maxErrorBodyBytes      = 1000
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ClientConfig struct {
	Endpoint    string
	TokenSource TokenSource
	HTTPClient  HTTPClient
	Logger      *slog.Logger
}

type Client struct {
	endpoint     string
	restEndpoint string
	tokenSource  TokenSource
	httpClient   HTTPClient
	logger       *slog.Logger
	mu           sync.RWMutex
	rateLimit    connector.GraphQLRateLimit
	hasRateLimit bool
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
		logger:       logger,
	}, nil
}

func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
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
	c.recordRateLimitFromHeaders(resp.Header, time.Now())

	if resp.StatusCode != http.StatusOK {
		return classifyStatus(resp.StatusCode, resp.Header, raw)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	c.recordRateLimitFromData(envelope.Data, time.Now())
	if len(envelope.Errors) > 0 {
		return classifyGraphQLErrors(envelope.Errors)
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
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("resolve github token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissingToken
	}

	var requestBody io.Reader
	if body != nil {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			return fmt.Errorf("encode github rest request: %w", err)
		}
		requestBody = &encoded
	}

	url, err := c.restURL(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, requestBody)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
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
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		if err := drainAndClose(resp.Body); err != nil {
			c.logger.DebugContext(ctx, "github rest response body drain failed", "method", method, "path", path, "error", err)
		}
	}()

	c.logger.DebugContext(ctx, "github rest response", "method", method, "path", path, "status", resp.StatusCode, "live_connections", c.LiveConnections())
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return classifyStatus(resp.StatusCode, resp.Header, raw)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if len(raw) == 0 {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	return nil
}

func (c *Client) GraphQLRateLimit() (connector.GraphQLRateLimit, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.rateLimit, c.hasRateLimit
}

func (c *Client) recordRateLimitFromData(data json.RawMessage, now time.Time) {
	if len(data) == 0 {
		return
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
		return
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
}

func (c *Client) recordRateLimitFromHeaders(headers http.Header, now time.Time) {
	var snapshot connector.GraphQLRateLimit
	if current, ok := c.GraphQLRateLimit(); ok {
		snapshot = current
	}

	hasSnapshot := false
	if value, ok := int64Header(headers, "X-RateLimit-Limit"); ok {
		snapshot.Limit = value
		hasSnapshot = true
	}
	if value, ok := int64Header(headers, "X-RateLimit-Used"); ok {
		snapshot.Used = value
		hasSnapshot = true
	}
	if value, ok := int64Header(headers, "X-RateLimit-Remaining"); ok {
		snapshot.Remaining = value
		hasSnapshot = true
	}
	if value, ok := int64Header(headers, "X-RateLimit-Reset"); ok {
		snapshot.ResetAt = time.Unix(value, 0).UTC()
		hasSnapshot = true
	}
	if retryAfter, ok := parseRetryAfter(headers.Get("Retry-After"), now); ok {
		snapshot.RetryAfter = retryAfter
		hasSnapshot = true
	} else if hasSnapshot {
		snapshot.RetryAfter = 0
	}

	if hasSnapshot {
		snapshot.UpdatedAt = now
		c.setRateLimit(snapshot)
	}
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

func (c *Client) setRateLimit(snapshot connector.GraphQLRateLimit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rateLimit = snapshot
	c.hasRateLimit = true
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
	} else if strings.HasSuffix(basePath, "/api/graphql") {
		basePath = strings.TrimSuffix(basePath, "/api/graphql") + "/api/v3"
	} else if strings.HasSuffix(basePath, "/graphql") {
		basePath = strings.TrimSuffix(basePath, "/graphql")
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

func classifyStatus(status int, headers http.Header, body []byte) error {
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

	return &StatusError{
		StatusCode: status,
		Body:       summarizeBody(body),
		Err:        base,
	}
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
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
