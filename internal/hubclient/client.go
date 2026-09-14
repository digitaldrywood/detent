package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const maxResponseBytes = 1 << 20

var (
	ErrNoClaimableWork = errors.New("no Hub work is claimable")
	ErrUnavailable     = errors.New("hub unavailable")
)

type Config struct {
	ArtifactServiceID string
	ArtifactBytes     int64
	URL               string
	IdentityFile      string
	TokenSource       func() string
	HTTPClient        *http.Client
}

type Client struct {
	artifactServiceID string
	artifactBytes     int64
	nativeLeases      sync.Map
	runner            *runnerCredentialSource
	baseURL           *url.URL
	tokenSource       func() string
	httpClient        *http.Client
}

type Machine struct {
	ProviderReports []providercapacity.Report `json:"provider_reports,omitempty"`
	ID              tracker.MachineID         `json:"id"`
	Hostname        string                    `json:"hostname"`
	DisplayName     string                    `json:"display_name,omitempty"`
	Capabilities    map[string]any            `json:"capabilities,omitempty"`
	Capacity        int                       `json:"capacity"`
	Version         string                    `json:"version"`
	LastHeartbeatAt time.Time                 `json:"last_heartbeat_at,omitempty"`
	// WorkspaceCapabilities and WorkspaceIsolation are what this runner can
	// serve for a workspace session. Decisions section 18.1 lets a runner
	// claim a workspace "only if it reports every capability in requires with
	// a fresh heartbeat", and the hub reads the report from the same row it
	// stamps the heartbeat on, so the two must travel together.
	//
	// They are not serialised with the rest of the struct: only the native
	// machine endpoints accept them, every hub endpoint rejects unknown
	// fields, and the v1 register/heartbeat pair would start failing for a
	// runner that has nothing to do with workspaces. The native requests pick
	// them up explicitly through workspaceReport instead.
	WorkspaceCapabilities workspacesession.Capabilities `json:"-"`
	WorkspaceIsolation    string                        `json:"-"`
}

// workspaceReport is what the native machine endpoints send for the workspace
// claim gate, or nil when this runner serves no surface.
//
// Reporting nothing is deliberate rather than a shortcut: the hub leaves a
// stored report alone when a request omits it, so a runner built without the
// workspace lane keeps behaving exactly as it did before it learned the
// fields existed.
func (m Machine) workspaceReport() (*workspacesession.Capabilities, string) {
	if m.WorkspaceCapabilities == (workspacesession.Capabilities{}) {
		return nil, ""
	}
	capabilities := m.WorkspaceCapabilities
	return &capabilities, m.WorkspaceIsolation
}

type MachineHeartbeat struct {
	DisplayName  *string         `json:"display_name,omitempty"`
	Capabilities *map[string]any `json:"capabilities,omitempty"`
	Capacity     *int            `json:"capacity,omitempty"`
	Version      *string         `json:"version,omitempty"`
}

type WorkItem struct {
	tracker.WorkItem
	Body string `json:"body"`
}

type ClaimRequest struct {
	PolicyID      string            `json:"policy_id"`
	MachineID     tracker.MachineID `json:"machine_id"`
	SessionID     string            `json:"session_id"`
	TTLSeconds    int64             `json:"ttl_seconds"`
	Repositories  []string          `json:"repositories,omitempty"`
	WorkflowState []string          `json:"workflow_states,omitempty"`
	Authors       []string          `json:"authors,omitempty"`
	Assignees     []string          `json:"assignees,omitempty"`
	LabelInclude  []string          `json:"label_include,omitempty"`
	LabelExclude  []string          `json:"label_exclude,omitempty"`
}

type LeaseRequest struct {
	FencingToken tracker.FencingToken `json:"fencing_token"`
	TTLSeconds   int64                `json:"ttl_seconds,omitempty"`
	Reason       string               `json:"reason,omitempty"`
}

type APIError struct {
	Status          int
	Code            string
	Message         string
	CurrentRevision tracker.Revision
}

func (e *APIError) Error() string {
	if e == nil {
		return "Hub request failed"
	}
	if e.Code == "" {
		return fmt.Sprintf("Hub request failed with status %d", e.Status)
	}
	return fmt.Sprintf("Hub request failed with status %d (%s): %s", e.Status, e.Code, e.Message)
}

func New(config Config) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
		return nil, errors.New("hub URL must be an absolute HTTP or HTTPS URL")
	}
	if config.TokenSource == nil && config.IdentityFile == "" {
		return nil, errors.New("hub token source is required")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	client := &Client{baseURL: baseURL, tokenSource: config.TokenSource, httpClient: httpClient, artifactServiceID: config.ArtifactServiceID, artifactBytes: config.ArtifactBytes}
	if config.IdentityFile != "" {
		file, err := runnerauth.Load(config.IdentityFile)
		if err != nil {
			return nil, err
		}
		if file.HubURL != baseURL.String() {
			return nil, errors.New("runner identity belongs to a different Hub")
		}
		client.runner = &runnerCredentialSource{path: config.IdentityFile}
		transport := *httpClient
		transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client.httpClient = &transport
	}
	return client, nil
}

// clientWithTimeout returns an HTTP client whose own Timeout cannot cut a
// request short of the deadline the caller asked for. http.Client.Timeout is a
// hard cap that a longer context deadline cannot lift, so a long poll needs a
// copy rather than the caller's client. The copy shares the transport, so it
// shares the connection pool.
func clientWithTimeout(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	if client.Timeout <= 0 || client.Timeout >= timeout {
		return client
	}
	copied := *client
	copied.Timeout = timeout
	return &copied
}

func (c *Client) RegisterMachine(ctx context.Context, machine Machine) (Machine, error) {
	var response Machine
	err := c.request(ctx, http.MethodPost, "/api/v1/machines/register", machine, &response)
	return response, err
}

func (c *Client) HeartbeatMachine(ctx context.Context, id tracker.MachineID, heartbeat MachineHeartbeat) (Machine, error) {
	var response Machine
	err := c.request(ctx, http.MethodPost, "/api/v1/machines/"+url.PathEscape(string(id))+"/heartbeat", heartbeat, &response)
	return response, err
}

func (c *Client) Claim(ctx context.Context, request ClaimRequest) (tracker.Lease, error) {
	var lease tracker.Lease
	err := c.request(ctx, http.MethodPost, "/api/v1/claims", request, &lease)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Code == "no_claimable_work" {
		return tracker.Lease{}, ErrNoClaimableWork
	}
	return lease, err
}

func (c *Client) WorkItem(ctx context.Context, id tracker.WorkItemID) (WorkItem, error) {
	var response WorkItem
	err := c.request(ctx, http.MethodGet, "/api/v1/work-items/"+strconv.FormatInt(int64(id), 10), nil, &response)
	return response, err
}

func (c *Client) Renew(ctx context.Context, lease tracker.Lease, ttl time.Duration) (tracker.Lease, error) {
	var renewed tracker.Lease
	err := c.request(ctx, http.MethodPost, "/api/v1/leases/"+url.PathEscape(string(lease.ID))+"/renew", LeaseRequest{
		FencingToken: lease.FencingToken,
		TTLSeconds:   int64(ttl / time.Second),
	}, &renewed)
	return renewed, err
}

func (c *Client) Release(ctx context.Context, lease tracker.Lease, reason string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/leases/"+url.PathEscape(string(lease.ID))+"/release", LeaseRequest{
		FencingToken: lease.FencingToken,
		Reason:       strings.TrimSpace(reason),
	}, nil)
}

// request performs one Hub request with the client's own headers.
func (c *Client) request(ctx context.Context, method string, path string, input any, output any) error {
	return c.requestWithHeaders(ctx, method, path, nil, input, output)
}

// requestWithHeaders performs one Hub request, adding the caller's headers for
// endpoints that carry the owner tuple outside the body. The client's own
// Authorization, Accept and Content-Type always win.
func (c *Client) requestWithHeaders(ctx context.Context, method string, path string, headers http.Header, input any, output any) error {
	return c.requestWithDeadline(ctx, 0, method, path, headers, input, output)
}

// requestWithDeadline performs one Hub request that carries its own deadline
// instead of the client's default request timeout. A long poll is the caller
// that needs it: the server is expected to hold the request open for the wait
// it was asked for, which is longer than any ordinary Hub call and longer than
// the configured default. Passing zero keeps the client's own timeout.
func (c *Client) requestWithDeadline(ctx context.Context, timeout time.Duration, method string, path string, headers http.Header, input any, output any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	httpClient := c.httpClient
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		httpClient = clientWithTimeout(httpClient, timeout)
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Hub request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *c.baseURL
	requestPath, requestQuery, _ := strings.Cut(path, "?")
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + requestPath
	endpoint.RawQuery = requestQuery
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("build Hub request: %w", err)
	}
	token, err := c.bearerToken(ctx)
	if err != nil {
		return err
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response: %w", ErrUnavailable, err)
	}
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("%w: response exceeds %d bytes", ErrUnavailable, maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiErr := &APIError{Status: response.StatusCode}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, apiErr); err != nil {
				return fmt.Errorf("%w: decode error response: %w", ErrUnavailable, err)
			}
		}
		if response.StatusCode >= 500 {
			return errors.Join(ErrUnavailable, apiErr)
		}
		return apiErr
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return fmt.Errorf("%w: decode response: %w", ErrUnavailable, err)
	}
	return nil
}

func (e *APIError) UnmarshalJSON(data []byte) error {
	var value struct {
		Code            string           `json:"code"`
		Message         string           `json:"message"`
		CurrentRevision tracker.Revision `json:"current_revision,string"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	e.Code = value.Code
	e.Message = value.Message
	e.CurrentRevision = value.CurrentRevision
	return nil
}

// download fetches a blob from the Hub with the client's own credential. It
// is separate from request because a response body here is a file, not JSON:
// the JSON path caps a response at maxResponseBytes, which no attachment
// would fit under. target must be a Hub path, never an absolute URL: the
// client follows its own base and never a host the Hub names.
// bearerToken resolves this client's Authorization header value. It is a
// method rather than a local so the relay dial, which does not go through the
// JSON request path, presents exactly the same credential and rotation
// behaviour as every other call.
func (c *Client) bearerToken(ctx context.Context) (string, error) {
	var token string
	if c.runner != nil {
		resolved, err := c.runnerToken(ctx)
		if err != nil {
			return "", err
		}
		token = resolved
	} else {
		token = strings.TrimSpace(c.tokenSource())
	}
	if token == "" {
		return "", errors.New("hub token is unavailable")
	}
	return "Bearer " + token, nil
}

func (c *Client) download(ctx context.Context, target string, limit int64) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return nil, "", fmt.Errorf("hub download target %q is not a Hub path", target)
	}
	endpoint := *c.baseURL
	requestPath, requestQuery, _ := strings.Cut(target, "?")
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + requestPath
	endpoint.RawQuery = requestQuery
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("build Hub download: %w", err)
	}
	var token string
	if c.runner != nil {
		token, err = c.runnerToken(ctx)
		if err != nil {
			return nil, "", err
		}
	} else {
		token = strings.TrimSpace(c.tokenSource())
	}
	if token == "" {
		return nil, "", errors.New("hub token is unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read download: %w", ErrUnavailable, err)
	}
	if int64(len(payload)) > limit {
		return nil, "", fmt.Errorf("%w: download exceeds %d bytes", ErrUnavailable, limit)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiErr := &APIError{Status: response.StatusCode}
		if len(payload) > 0 && strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(payload, apiErr); err != nil {
				return nil, "", fmt.Errorf("%w: decode error response: %w", ErrUnavailable, err)
			}
		}
		if response.StatusCode >= 500 {
			return nil, "", errors.Join(ErrUnavailable, apiErr)
		}
		return nil, "", apiErr
	}
	return payload, response.Header.Get("Content-Type"), nil
}
