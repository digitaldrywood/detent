package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	dashboardReadTimeout     = 10 * time.Second
	dashboardResponseBodyMax = 1 << 20
	stateCollectionLimit     = 100
)

type dashboardHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type DashboardReadClient struct {
	baseURL    *url.URL
	address    dashboardAddress
	credential string
	http       dashboardHTTPClient
	timeout    time.Duration
}

type dashboardAPIProblem struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type DashboardResponseError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *DashboardResponseError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return fmt.Sprintf("dashboard API returned HTTP %d", e.StatusCode)
}

type DashboardTransportError struct {
	Timeout bool
	Err     error
	Address dashboardAddress
}

type DashboardState struct {
	payload    map[string]any
	Truncation StateTruncation
}

type StateTruncation struct {
	Limit       int                         `json:"limit"`
	Truncated   bool                        `json:"truncated"`
	Collections []StateCollectionTruncation `json:"collections"`
}

type StateCollectionTruncation struct {
	Path    string `json:"path"`
	Omitted int    `json:"omitted"`
}

func (e *DashboardTransportError) Error() string {
	if e == nil {
		return ""
	}
	if e.Timeout {
		if detail := e.Address.String(); detail != "" {
			return "dashboard API request to " + detail + " timed out"
		}
		return "dashboard API request timed out"
	}
	if detail := e.Address.String(); detail != "" {
		return "dashboard API at " + detail + " is unreachable"
	}
	return "dashboard API is unreachable"
}

func (e *DashboardTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func newDashboardReadClient(
	ctx context.Context,
	configPath string,
	host string,
	port int,
	portSet bool,
	opts options,
) (*DashboardReadClient, error) {
	boot, address, err := resolveDashboardBoot(ctx, configPath, host, port, portSet, opts)
	if err != nil {
		return nil, err
	}
	baseURL, err := url.Parse("http://" + dashboardServerAddr(boot))
	if err != nil {
		return nil, fmt.Errorf("resolve dashboard API URL: %w", err)
	}
	credential := strings.TrimSpace(opts.lookupEnv("DETENT_API_TOKEN"))
	if credential == "" {
		credential = strings.TrimSpace(boot.Global.APIToken)
	}
	if opts.httpDo == nil {
		return nil, errors.New("dashboard API HTTP client is not configured")
	}
	return &DashboardReadClient{
		baseURL:    baseURL,
		address:    address,
		credential: credential,
		http:       dashboardHTTPClientFunc(opts.httpDo),
		timeout:    dashboardReadTimeout,
	}, nil
}

func (c *DashboardReadClient) ExplainIssue(ctx context.Context, projectID string, reference string) (explain.IssueExplanation, error) {
	return c.issueExplanation(ctx, http.MethodGet, projectID, reference)
}

func (c *DashboardReadClient) AcknowledgeIssueParks(ctx context.Context, projectID string, reference string) (explain.IssueExplanation, error) {
	return c.issueExplanation(ctx, http.MethodPost, projectID, reference)
}

func (c *DashboardReadClient) issueExplanation(ctx context.Context, method string, projectID string, reference string) (explain.IssueExplanation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return explain.IssueExplanation{}, err
	}
	projectID = strings.TrimSpace(projectID)
	reference = strings.TrimSpace(reference)
	if projectID == "" || reference == "" {
		return explain.IssueExplanation{}, ValidationError("--project and issue reference are required")
	}
	if c == nil || c.baseURL == nil {
		return explain.IssueExplanation{}, errors.New("dashboard API client is not configured")
	}
	requestURL := *c.baseURL
	requestURL.Path = "/api/v1/projects/" + projectID + "/issues/explanation"
	requestURL.RawPath = "/api/v1/projects/" + url.PathEscape(projectID) + "/issues/explanation"
	query := requestURL.Query()
	query.Set("reference", reference)
	query.Set("schema", strconv.Itoa(explain.SchemaVersion))
	requestURL.RawQuery = query.Encode()

	var result explain.IssueExplanation
	statusCode, err := c.requestJSON(ctx, method, requestURL, &result)
	if err != nil {
		return explain.IssueExplanation{}, err
	}
	if result.Schema != explain.SchemaVersion {
		return explain.IssueExplanation{}, &DashboardResponseError{
			StatusCode: statusCode,
			Code:       "version_conflict",
			Message:    fmt.Sprintf("dashboard returned unsupported issue explanation schema %d", result.Schema),
		}
	}
	return result, nil
}

func (c *DashboardReadClient) State(ctx context.Context, projectID string) (DashboardState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DashboardState{}, err
	}
	if c == nil || c.baseURL == nil {
		return DashboardState{}, errors.New("dashboard API client is not configured")
	}

	requestURL := *c.baseURL
	projectID = strings.TrimSpace(projectID)
	requestURL.Path = "/api/v1/state"
	requestURL.RawPath = ""
	if projectID != "" {
		requestURL.Path = "/api/v1/projects/" + projectID + "/state"
		requestURL.RawPath = "/api/v1/projects/" + url.PathEscape(projectID) + "/state"
	}

	payload := map[string]any{}
	if _, err := c.readJSON(ctx, requestURL, &payload); err != nil {
		return DashboardState{}, err
	}
	delete(payload, "board_issues")
	truncation := StateTruncation{Limit: stateCollectionLimit, Collections: []StateCollectionTruncation{}}
	truncateStateCollections(payload, "", &truncation)
	truncation.Truncated = len(truncation.Collections) > 0
	return DashboardState{payload: payload, Truncation: truncation}, nil
}

func (s DashboardState) MarshalJSON() ([]byte, error) {
	payload := make(map[string]any, len(s.payload)+1)
	for key, value := range s.payload {
		payload[key] = value
	}
	payload["truncation"] = s.Truncation
	return json.Marshal(payload)
}

func (s DashboardState) field(name string) any {
	return s.payload[name]
}

func (c *DashboardReadClient) readJSON(ctx context.Context, requestURL url.URL, result any) (int, error) {
	return c.requestJSON(ctx, http.MethodGet, requestURL, result)
}

func (c *DashboardReadClient) requestJSON(ctx context.Context, method string, requestURL url.URL, result any) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if c == nil || c.baseURL == nil || c.http == nil {
		return 0, errors.New("dashboard API client is not configured")
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = dashboardReadTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, requestURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("create dashboard API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &DashboardTransportError{
			Timeout: errors.Is(requestContext.Err(), context.DeadlineExceeded),
			Err:     err,
			Address: c.address,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response.StatusCode, decodeDashboardResponseError(response)
	}
	if err := decodeDashboardJSON(response.Body, result); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

func decodeDashboardJSON(reader io.Reader, result any) error {
	body, err := io.ReadAll(io.LimitReader(reader, dashboardResponseBodyMax+1))
	if err != nil {
		return fmt.Errorf("read dashboard API response: %w", err)
	}
	if len(body) > dashboardResponseBodyMax {
		return fmt.Errorf("read dashboard API response: response exceeds %d bytes", dashboardResponseBodyMax)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode dashboard API response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("decode dashboard API response: %w", err)
	}
	return nil
}

func truncateStateCollections(value any, path string, truncation *StateTruncation) any {
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return typed
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			typed[key] = truncateStateCollections(typed[key], path+"/"+jsonPointerToken(key), truncation)
		}
		return typed
	case []any:
		if len(typed) > stateCollectionLimit {
			truncation.Collections = append(truncation.Collections, StateCollectionTruncation{
				Path:    path,
				Omitted: len(typed) - stateCollectionLimit,
			})
			typed = typed[:stateCollectionLimit]
		}
		for index := range typed {
			typed[index] = truncateStateCollections(typed[index], path+"/"+strconv.Itoa(index), truncation)
		}
		return typed
	}
	return value
}

func jsonPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func (c *DashboardReadClient) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return operatortool.Result{}, err
	}
	call.Name = strings.TrimSpace(call.Name)
	if _, ok := operatortool.Lookup(call.Name); !ok {
		return operatortool.Result{}, fmt.Errorf("%w %q", operatortool.ErrUnknownTool, call.Name)
	}
	if c == nil || c.baseURL == nil || c.http == nil {
		return operatortool.Result{}, errors.New("dashboard API client is not configured")
	}
	if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
		call.Arguments = json.RawMessage(`{}`)
	}
	if len(call.Arguments) > operatortool.MaxArgumentBytes {
		return operatortool.Result{}, fmt.Errorf("operator tool arguments exceed %d bytes", operatortool.MaxArgumentBytes)
	}

	requestURL := *c.baseURL
	requestURL.Path = "/api/v1/operator-tools/" + call.Name
	requestURL.RawPath = "/api/v1/operator-tools/" + url.PathEscape(call.Name)
	timeout := c.timeout
	if timeout <= 0 {
		timeout = dashboardReadTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, requestURL.String(), bytes.NewReader(call.Arguments))
	if err != nil {
		return operatortool.Result{}, fmt.Errorf("create dashboard API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return operatortool.Result{}, ctx.Err()
		}
		return operatortool.Result{}, &DashboardTransportError{
			Timeout: errors.Is(requestContext.Err(), context.DeadlineExceeded),
			Err:     err,
			Address: c.address,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return operatortool.Result{}, decodeDashboardResponseError(response)
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, operatortool.MaxResultBytes+1))
	if err != nil {
		return operatortool.Result{}, fmt.Errorf("read dashboard API response: %w", err)
	}
	if len(content) > operatortool.MaxResultBytes {
		return operatortool.Result{}, fmt.Errorf("dashboard API response exceeds %d bytes", operatortool.MaxResultBytes)
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(trimmed) {
		return operatortool.Result{}, errors.New("decode dashboard API response: response is not a JSON object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("response is not a JSON object")
		}
		return operatortool.Result{}, fmt.Errorf("decode dashboard API response: %w", err)
	}
	return operatortool.Result{Content: content}, nil
}

func decodeDashboardResponseError(response *http.Response) error {
	problem := dashboardAPIProblem{}
	err := json.NewDecoder(io.LimitReader(response.Body, dashboardResponseBodyMax)).Decode(&problem)
	code := strings.TrimSpace(problem.Error.Code)
	message := strings.TrimSpace(problem.Error.Message)
	if err != nil || code == "" {
		code = "http_error"
	}
	if message == "" {
		message = fmt.Sprintf("dashboard API returned HTTP %d", response.StatusCode)
	}
	return &DashboardResponseError{StatusCode: response.StatusCode, Code: code, Message: message}
}

type dashboardHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f dashboardHTTPClientFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}
