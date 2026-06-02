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
	if cfg.TokenSource == nil {
		return nil, ErrMissingToken
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Client{
		endpoint:    endpoint,
		tokenSource: cfg.TokenSource,
		httpClient:  httpClient,
		logger:      logger,
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

	c.logger.DebugContext(ctx, "github graphql request", "operation", firstLine(query))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

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
	hasSnapshot := false
	if current, ok := c.GraphQLRateLimit(); ok {
		snapshot = current
		hasSnapshot = true
	}

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
	}

	if hasSnapshot {
		snapshot.UpdatedAt = now
		c.setRateLimit(snapshot)
	}
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
