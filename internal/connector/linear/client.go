package linear

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
)

const (
	DefaultGraphQLEndpoint = "https://api.linear.app/graphql"
	maxErrorBodyBytes      = 1000
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ClientConfig struct {
	Endpoint   string
	APIKey     string
	HTTPClient HTTPClient
	Logger     *slog.Logger
}

type Client struct {
	endpoint   string
	apiKey     string
	httpClient HTTPClient
	logger     *slog.Logger
}

func NewClient(cfg ClientConfig) (*Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultGraphQLEndpoint
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Client{
		endpoint:   endpoint,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		httpClient: httpClient,
		logger:     logger,
	}, nil
}

func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	if strings.TrimSpace(c.apiKey) == "" {
		return ErrMissingToken
	}

	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}

	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return fmt.Errorf("encode linear graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, &body)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEndpoint, err)
	}
	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	c.logger.DebugContext(ctx, "linear graphql request", "operation", firstLine(query), "variables_present", len(variables) > 0)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("%w: %w", ErrTransient, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrTransient, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(raw)),
			Err:        ErrUnexpectedStatus,
		}
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []GraphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if len(envelope.Errors) > 0 {
		return &GraphQLErrorList{Errors: envelope.Errors, Err: ErrGraphQLErrors}
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
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%w: %s", ErrInvalidEndpoint, endpoint)
	}
	return nil
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
