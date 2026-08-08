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
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	dashboardReadTimeout     = 10 * time.Second
	dashboardResponseBodyMax = 1 << 20
)

type dashboardHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type DashboardReadClient struct {
	baseURL    *url.URL
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
}

func (e *DashboardTransportError) Error() string {
	if e == nil {
		return ""
	}
	if e.Timeout {
		return "dashboard API request timed out"
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
	boot, err := resolveDashboardBoot(ctx, configPath, host, port, portSet, opts)
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
		credential: credential,
		http:       dashboardHTTPClientFunc(opts.httpDo),
		timeout:    dashboardReadTimeout,
	}, nil
}

func (c *DashboardReadClient) ExplainIssue(ctx context.Context, projectID string, reference string) (explain.IssueExplanation, error) {
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
	if c == nil || c.baseURL == nil || c.http == nil {
		return explain.IssueExplanation{}, errors.New("dashboard API client is not configured")
	}

	requestURL := *c.baseURL
	requestURL.Path = "/api/v1/projects/" + projectID + "/issues/explanation"
	requestURL.RawPath = "/api/v1/projects/" + url.PathEscape(projectID) + "/issues/explanation"
	query := requestURL.Query()
	query.Set("reference", reference)
	query.Set("schema", strconv.Itoa(explain.SchemaVersion))
	requestURL.RawQuery = query.Encode()

	timeout := c.timeout
	if timeout <= 0 {
		timeout = dashboardReadTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return explain.IssueExplanation{}, fmt.Errorf("create dashboard API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if c.credential != "" {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}

	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return explain.IssueExplanation{}, ctx.Err()
		}
		return explain.IssueExplanation{}, &DashboardTransportError{
			Timeout: errors.Is(requestContext.Err(), context.DeadlineExceeded),
			Err:     err,
		}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return explain.IssueExplanation{}, decodeDashboardResponseError(response)
	}

	var result explain.IssueExplanation
	decoder := json.NewDecoder(io.LimitReader(response.Body, dashboardResponseBodyMax))
	if err := decoder.Decode(&result); err != nil {
		return explain.IssueExplanation{}, fmt.Errorf("decode dashboard API response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return explain.IssueExplanation{}, fmt.Errorf("decode dashboard API response: %w", err)
	}
	if result.Schema != explain.SchemaVersion {
		return explain.IssueExplanation{}, &DashboardResponseError{
			StatusCode: response.StatusCode,
			Code:       "version_conflict",
			Message:    fmt.Sprintf("dashboard returned unsupported issue explanation schema %d", result.Schema),
		}
	}
	return result, nil
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
