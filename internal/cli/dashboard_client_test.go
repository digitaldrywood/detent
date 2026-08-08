package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
)

func TestDashboardReadClientExplainIssueReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reference string
	}{
		{name: "issue ID", reference: "I_kwDOSskuwc8AAAABL5igFw"},
		{name: "canonical identifier", reference: "digitaldrywood/detent#1643"},
		{name: "URL with reserved query and fragment", reference: "https://github.com/digitaldrywood/detent/issues/1643?notification=thread#event/one"},
		{name: "hash number", reference: "#1643"},
		{name: "reserved query characters", reference: "issue?/path#fragment&next=value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := dashboardExplanationFixture(false)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", request.Method)
				}
				if request.URL.Path != "/api/v1/projects/detent/issues/explanation" {
					t.Errorf("path = %q", request.URL.Path)
				}
				if got := request.URL.Query().Get("reference"); got != tt.reference {
					t.Errorf("reference = %q, want %q", got, tt.reference)
				}
				if got := request.URL.Query().Get("schema"); got != strconv.Itoa(explain.SchemaVersion) {
					t.Errorf("schema = %q", got)
				}
				if got := request.Header.Get("Accept"); got != "application/json" {
					t.Errorf("Accept = %q", got)
				}
				writer.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(writer).Encode(want); err != nil {
					t.Errorf("Encode() error = %v", err)
				}
			}))
			t.Cleanup(server.Close)

			client := dashboardClientForServer(t, server, "")
			got, err := client.ExplainIssue(t.Context(), " detent ", " "+tt.reference+" ")
			if err != nil {
				t.Fatalf("ExplainIssue() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %#v, want %#v", got, want)
			}
		})
	}
}

func TestDashboardReadClientCredentialPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		environment string
		configured  string
		wantHeader  string
	}{
		{name: "environment wins", environment: "environment-token", configured: "config-token", wantHeader: "Bearer environment-token"},
		{name: "config fallback", configured: "config-token", wantHeader: "Bearer config-token"},
		{name: "peer trust without credential", wantHeader: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if got := request.Header.Get("Authorization"); got != tt.wantHeader {
					t.Errorf("Authorization = %q, want %q", got, tt.wantHeader)
				}
				_ = json.NewEncoder(writer).Encode(dashboardExplanationFixture(false))
			}))
			t.Cleanup(server.Close)
			parsed, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			port, err := strconv.Atoi(parsed.Port())
			if err != nil {
				t.Fatalf("Atoi() error = %v", err)
			}
			opts := dashboardClientOptions(server.Client().Do, tt.environment, tt.configured)
			client, err := newDashboardReadClient(t.Context(), "/config/global.yaml", "0.0.0.0", port, true, opts)
			if err != nil {
				t.Fatalf("newDashboardReadClient() error = %v", err)
			}
			if _, err := client.ExplainIssue(t.Context(), "detent", "#1643"); err != nil {
				t.Fatalf("ExplainIssue() error = %v", err)
			}
		})
	}
}

func TestDashboardReadClientSuppliedBadCredentialIsNotMasked(t *testing.T) {
	t.Parallel()

	const badToken = "supplied-bad-token"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer "+badToken {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":{"code":"unauthorized","message":"Valid API token is required"}}`)
			return
		}
		_ = json.NewEncoder(writer).Encode(dashboardExplanationFixture(false))
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}
	opts := dashboardClientOptions(server.Client().Do, badToken, "valid-config-token")
	client, err := newDashboardReadClient(t.Context(), "/config/global.yaml", parsed.Hostname(), port, true, opts)
	if err != nil {
		t.Fatalf("newDashboardReadClient() error = %v", err)
	}
	_, err = client.ExplainIssue(t.Context(), "detent", "#1643")
	problem := ProblemForError(classifyDashboardReadError(err))
	if problem.Code != errorCodeDashboardUnauthorized || problem.ExitCode != ExitAuth {
		t.Fatalf("problem = %#v", problem)
	}
	if strings.Contains(problem.Detail, badToken) || strings.Contains(problem.SuggestedFix, badToken) {
		t.Fatalf("problem leaked credential: %#v", problem)
	}
}

func TestDashboardReadClientTransportAndCancellation(t *testing.T) {
	t.Parallel()

	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := dashboardClientForServer(t, server, "")
		server.Close()
		_, err := client.ExplainIssue(t.Context(), "detent", "#1643")
		var transport *DashboardTransportError
		if !errors.As(err, &transport) || transport.Timeout {
			t.Fatalf("error = %#v, want unreachable transport error", err)
		}
		problem := ProblemForError(classifyDashboardReadError(err))
		if problem.Code != errorCodeDashboardUnreachable || problem.ExitCode != ExitGeneral {
			t.Fatalf("problem = %#v", problem)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		client := &DashboardReadClient{
			baseURL: &url.URL{Scheme: "http", Host: "127.0.0.1:1"},
			http: dashboardHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}),
			timeout: time.Millisecond,
		}
		_, err := client.ExplainIssue(t.Context(), "detent", "#1643")
		var transport *DashboardTransportError
		if !errors.As(err, &transport) || !transport.Timeout {
			t.Fatalf("error = %#v, want timeout transport error", err)
		}
		problem := ProblemForError(classifyDashboardReadError(err))
		if problem.Code != errorCodeDashboardTimeout || problem.ExitCode != ExitGeneral {
			t.Fatalf("problem = %#v", problem)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		client := &DashboardReadClient{}
		_, err := client.ExplainIssue(ctx, "detent", "#1643")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func TestDashboardReadClientRejectsUnsupportedSuccessfulModel(t *testing.T) {
	t.Parallel()

	result := dashboardExplanationFixture(false)
	result.Schema = explain.SchemaVersion + 1
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(result)
	}))
	t.Cleanup(server.Close)
	client := dashboardClientForServer(t, server, "")
	_, err := client.ExplainIssue(t.Context(), "detent", "#1643")
	problem := ProblemForError(classifyDashboardReadError(err))
	if problem.Code != errorCodeUnsupportedModelVersion || problem.ExitCode != ExitGeneral {
		t.Fatalf("problem = %#v", problem)
	}
}

func TestClassifyDashboardReadProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantCode   string
		wantExit   int
		wantDetail string
	}{
		{name: "unauthorized", err: &DashboardResponseError{StatusCode: http.StatusUnauthorized, Code: "unauthorized", Message: "bad credential"}, wantCode: errorCodeDashboardUnauthorized, wantExit: ExitAuth, wantDetail: "bad credential"},
		{name: "forbidden", err: &DashboardResponseError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "scope denied"}, wantCode: errorCodeDashboardForbidden, wantExit: ExitAuth, wantDetail: "scope denied"},
		{name: "ambiguous", err: &DashboardResponseError{StatusCode: http.StatusConflict, Code: "ambiguous_reference", Message: "ambiguous"}, wantCode: errorCodeAmbiguousReference, wantExit: ExitValidation, wantDetail: "ambiguous"},
		{name: "not found", err: &DashboardResponseError{StatusCode: http.StatusNotFound, Code: "issue_not_found", Message: "missing"}, wantCode: errorCodeIssueNotFound, wantExit: ExitNotFoundOrConfig, wantDetail: "missing"},
		{name: "unsupported model", err: &DashboardResponseError{StatusCode: http.StatusConflict, Code: "version_conflict", Message: "unsupported"}, wantCode: errorCodeUnsupportedModelVersion, wantExit: ExitGeneral, wantDetail: "unsupported"},
		{name: "runtime unavailable", err: &DashboardResponseError{StatusCode: http.StatusServiceUnavailable, Code: "runtime_unavailable", Message: "not ready"}, wantCode: errorCodeRuntimeUnavailable, wantExit: ExitGeneral, wantDetail: "not ready"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problem := ProblemForError(classifyDashboardReadError(tt.err))
			if problem.Code != tt.wantCode || problem.ExitCode != tt.wantExit || problem.Detail != tt.wantDetail {
				t.Fatalf("problem = %#v", problem)
			}
		})
	}
}

func dashboardClientForServer(t *testing.T, server *httptest.Server, credential string) *DashboardReadClient {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return &DashboardReadClient{baseURL: parsed, credential: credential, http: server.Client(), timeout: time.Second}
}

func dashboardClientOptions(do func(*http.Request) (*http.Response, error), environmentToken string, configToken string) options {
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: "/config/global.yaml", Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.read = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{APIToken: configToken}, nil
	}
	opts.lookupEnv = func(key string) string {
		if key == "DETENT_API_TOKEN" {
			return environmentToken
		}
		return ""
	}
	opts.httpDo = do
	return opts
}

func dashboardExplanationFixture(degraded bool) explain.IssueExplanation {
	return explain.IssueExplanation{
		Schema:       explain.SchemaVersion,
		Found:        true,
		ObservedAt:   time.Date(2026, 8, 7, 20, 0, 0, 0, time.UTC),
		Identity:     explain.Identity{ProjectID: "detent", IssueID: "issue-1643", Identifier: "digitaldrywood/detent#1643", Number: 1643, Title: "Add issue explain command"},
		CurrentLane:  explain.Lane{Name: "In Progress", Freshness: explain.SourceLive, Degraded: degraded},
		Eligibility:  explain.Eligibility{State: explain.EligibilityEligible, Refusals: []explain.EligibilityDecision{}, Source: explain.SourceAvailable},
		Sessions:     explain.Sessions{Source: explain.SourceAvailable},
		RequiredGate: explain.Gate{State: explain.GatePending, SourceState: explain.SourceAvailable, Failures: []string{}, Running: []string{}},
		Sources:      []explain.SourceStatus{{Name: "snapshot", State: explain.SourceLive}},
		Evidence:     []explain.EvidenceReference{},
	}
}

func TestIssueCommandOutputAndScoping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		stdoutTTY  bool
		wantError  string
		wantPretty []string
	}{
		{name: "project required", args: []string{"#1643", "--explain"}, wantError: "--project is required"},
		{name: "operation required", args: []string{"#1643", "--project", "detent"}, wantError: "issue operation is required"},
		{name: "pretty projection", args: []string{"#1643", "--explain", "--project", "detent"}, stdoutTTY: true, wantPretty: []string{"Issue: digitaldrywood/detent#1643", "Lane: In Progress", "Lane degraded: true", "Source snapshot: unavailable (not_published)"}},
		{name: "JSON DTO", args: []string{"#1643", "--explain", "--project", "detent"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			want := dashboardExplanationFixture(true)
			want.Sources[0] = explain.SourceStatus{Name: "snapshot", State: explain.SourceUnavailable, Code: "not_published"}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_ = json.NewEncoder(writer).Encode(want)
			}))
			t.Cleanup(server.Close)
			parsed, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			port, err := strconv.Atoi(parsed.Port())
			if err != nil {
				t.Fatalf("Atoi() error = %v", err)
			}
			opts := dashboardClientOptions(server.Client().Do, "", "")
			opts.read = func(string) (globalconfig.Config, error) {
				return globalconfig.Config{Port: &port}, nil
			}
			configPath := "/config/global.yaml"
			host := parsed.Hostname()
			cmd := newIssueCommand(&configPath, &host, &port, opts)
			cmd.SilenceUsage = true
			cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
				lookupEnv: opts.lookupEnv,
				stdoutTTY: func() bool { return tt.stdoutTTY },
			}))
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			err = cmd.Execute()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Execute() error = %v, want %q", err, tt.wantError)
				}
				if stdout.Len() != 0 {
					t.Fatalf("stdout = %q, want empty", stdout.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			for _, expected := range tt.wantPretty {
				if !strings.Contains(stdout.String(), expected) {
					t.Fatalf("stdout missing %q:\n%s", expected, stdout.String())
				}
			}
			if tt.stdoutTTY {
				return
			}
			var got explain.IssueExplanation
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("Unmarshal() error = %v; stdout = %s", err, stdout.String())
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("JSON result = %#v, want exact DTO %#v", got, want)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &object); err != nil {
				t.Fatalf("Unmarshal(map) error = %v", err)
			}
			if _, wrapped := object["data"]; wrapped {
				t.Fatalf("JSON output has wrapper: %s", stdout.String())
			}
		})
	}
}

func TestIssueCommandHelpCoversScopingAndJSON(t *testing.T) {
	t.Parallel()

	cmd := NewRootCommand(t.Context(), WithStdoutTTY(func() bool { return true }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"issue", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"detent issue '#1643' --explain --project detent", "--format json", "--project string", "running Detent service"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, stdout.String())
		}
	}
}
