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
	"strings"
	"time"
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
	endpoint    string
	tokenSource TokenSource
	httpClient  HTTPClient
	logger      *slog.Logger
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
		if headers.Get("X-RateLimit-Remaining") == "0" {
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
