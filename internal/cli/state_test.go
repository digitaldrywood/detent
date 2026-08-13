package cli

import (
	"bytes"
	"encoding/json"
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
)

func TestDashboardReadClientStateScoping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projectID   string
		wantPath    string
		wantEscaped string
	}{
		{name: "fleet", wantPath: "/api/v1/state", wantEscaped: "/api/v1/state"},
		{name: "project", projectID: "detent", wantPath: "/api/v1/projects/detent/state", wantEscaped: "/api/v1/projects/detent/state"},
		{name: "reserved project ID", projectID: " digitaldrywood/detent ", wantPath: "/api/v1/projects/digitaldrywood/detent/state", wantEscaped: "/api/v1/projects/digitaldrywood%2Fdetent/state"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", request.Method)
				}
				if request.URL.Path != tt.wantPath {
					t.Errorf("path = %q, want %q", request.URL.Path, tt.wantPath)
				}
				if request.URL.EscapedPath() != tt.wantEscaped {
					t.Errorf("escaped path = %q, want %q", request.URL.EscapedPath(), tt.wantEscaped)
				}
				if got := request.Header.Get("Accept"); got != "application/json" {
					t.Errorf("Accept = %q", got)
				}
				_ = json.NewEncoder(writer).Encode(stateFixture())
			}))
			t.Cleanup(server.Close)

			state, err := dashboardClientForServer(t, server, "").State(t.Context(), tt.projectID)
			if err != nil {
				t.Fatalf("State() error = %v", err)
			}
			if got := stateString(state.field("generated_at")); got != "2026-08-08T03:00:00Z" {
				t.Fatalf("generated_at = %q", got)
			}
		})
	}
}

func TestDashboardReadClientStateBoundsEveryCollection(t *testing.T) {
	t.Parallel()

	payload := stateFixture()
	payload["running"] = stateRows(103)
	payload["board_issues"] = stateRows(2)
	payload["refresh"].(map[string]any)["sources"] = stateRows(101)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(payload)
	}))
	t.Cleanup(server.Close)

	state, err := dashboardClientForServer(t, server, "").State(t.Context(), "")
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := got["board_issues"]; ok {
		t.Fatalf("state exposes internal board_issues: %s", encoded)
	}
	if rows := got["running"].([]any); len(rows) != stateCollectionLimit {
		t.Fatalf("running length = %d, want %d", len(rows), stateCollectionLimit)
	}
	refresh := got["refresh"].(map[string]any)
	if sources := refresh["sources"].([]any); len(sources) != stateCollectionLimit {
		t.Fatalf("refresh.sources length = %d, want %d", len(sources), stateCollectionLimit)
	}
	wantTruncation := StateTruncation{
		Limit:     stateCollectionLimit,
		Truncated: true,
		Collections: []StateCollectionTruncation{
			{Path: "/refresh/sources", Omitted: 1},
			{Path: "/running", Omitted: 3},
		},
	}
	if !reflect.DeepEqual(state.Truncation, wantTruncation) {
		t.Fatalf("truncation = %#v, want %#v", state.Truncation, wantTruncation)
	}
}

func TestDashboardReadClientStateProblemsMatchExplain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mode     string
		status   int
		body     string
		timeout  time.Duration
		wantCode string
		wantExit int
	}{
		{
			name:     "service unreachable",
			mode:     "unreachable",
			wantCode: errorCodeDashboardUnreachable,
			wantExit: ExitGeneral,
		},
		{
			name:     "timeout",
			mode:     "timeout",
			timeout:  time.Millisecond,
			wantCode: errorCodeDashboardTimeout,
			wantExit: ExitGeneral,
		},
		{
			name:     "unauthorized",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"code":"unauthorized","message":"bad credential"}}`,
			wantCode: errorCodeDashboardUnauthorized,
			wantExit: ExitAuth,
		},
		{
			name:     "forbidden",
			status:   http.StatusForbidden,
			body:     `{"error":{"code":"forbidden","message":"scope denied"}}`,
			wantCode: errorCodeDashboardForbidden,
			wantExit: ExitAuth,
		},
		{
			name:     "project not found",
			status:   http.StatusNotFound,
			body:     `{"error":{"code":"project_not_found","message":"Project not found"}}`,
			wantCode: errorCodeProjectNotFound,
			wantExit: ExitNotFoundOrConfig,
		},
		{
			name:   "degraded 200",
			status: http.StatusOK,
			body:   `{"generated_at":"2026-08-08T03:00:00Z","error":{"code":"snapshot_unavailable","message":"Snapshot unavailable"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if tt.mode == "timeout" {
					<-request.Context().Done()
					return
				}
				writer.WriteHeader(tt.status)
				_, _ = io.WriteString(writer, tt.body)
			}))
			client := dashboardClientForServer(t, server, "")
			if tt.mode == "unreachable" {
				server.Close()
			} else {
				t.Cleanup(server.Close)
			}
			if tt.timeout > 0 {
				client.timeout = tt.timeout
			}
			state, err := client.State(t.Context(), "detent")
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("State() error = %v", err)
				}
				if got := stateNestedString(state.field("error"), "code"); got != "snapshot_unavailable" {
					t.Fatalf("degraded error code = %q", got)
				}
				return
			}
			problem := ProblemForError(classifyStateReadError(err))
			if problem.Code != tt.wantCode || problem.ExitCode != tt.wantExit {
				t.Fatalf("problem = %#v, want code %q exit %d", problem, tt.wantCode, tt.wantExit)
			}
		})
	}
}

func TestStateCommandOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		stdoutTTY     bool
		args          []string
		wantPretty    []string
		wantJSONBuild bool
	}{
		{name: "JSON projection", args: []string{"--project", "detent"}, wantJSONBuild: true},
		{name: "pretty projection", stdoutTTY: true, args: []string{"--project", "detent"}, wantPretty: []string{"Status: running", "Generated at: 2026-08-08T03:00:00Z", "Degraded: true", "Refresh status: degraded", "Running: 2", "Truncated: false"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/projects/detent/state" {
					t.Errorf("path = %q", request.URL.Path)
				}
				_ = json.NewEncoder(writer).Encode(stateFixture())
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
			cmd := newStateCommand(&configPath, &host, &port, opts)
			cmd.SilenceUsage = true
			cmd.SetContext(withCommandOutputOptions(t.Context(), commandOutputOptions{
				lookupEnv: opts.lookupEnv,
				stdoutTTY: func() bool { return tt.stdoutTTY },
			}))
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			for _, want := range tt.wantPretty {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
				}
			}
			if tt.stdoutTTY {
				return
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(stdout.Bytes(), &object); err != nil {
				t.Fatalf("Unmarshal() error = %v; stdout = %s", err, stdout.String())
			}
			for _, key := range []string{"generated_at", "refresh", "running", "truncation"} {
				if _, ok := object[key]; !ok {
					t.Fatalf("JSON output missing %q: %s", key, stdout.String())
				}
			}
			if tt.wantJSONBuild {
				var instance map[string]string
				if err := json.Unmarshal(object["instance"], &instance); err != nil {
					t.Fatalf("Unmarshal(instance) error = %v", err)
				}
				if instance["version"] != "v1.3.0" || instance["commit"] != "abcdef123456" {
					t.Fatalf("instance = %#v, want running build", instance)
				}
			}
		})
	}
}

func TestStateCommandRejectsBlankExplicitProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "empty", args: []string{"--project", ""}},
		{name: "whitespace", args: []string{"--project", "  \t"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := "/config/global.yaml"
			host := "127.0.0.1"
			port := 1
			cmd := newStateCommand(&configPath, &host, &port, options{})
			cmd.SilenceUsage = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "--project must not be blank") {
				t.Fatalf("Execute() error = %v, want blank project validation error", err)
			}
			problem := ProblemForError(err)
			if problem.ExitCode != ExitValidation {
				t.Fatalf("exit code = %d, want %d", problem.ExitCode, ExitValidation)
			}
		})
	}
}

func TestStateCommandHelpDocumentsBoundsAndScoping(t *testing.T) {
	t.Parallel()

	cmd := NewRootCommand(t.Context(), WithStdoutTTY(func() bool { return true }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"state", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"first 100 entries", "JSON Pointer path", "detent state --project detent", "--format json"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, stdout.String())
		}
	}
}

func stateFixture() map[string]any {
	return map[string]any{
		"generated_at": "2026-08-08T03:00:00Z",
		"status":       "running",
		"instance":     map[string]any{"version": "v1.3.0", "commit": "abcdef123456"},
		"refresh": map[string]any{
			"status":              "degraded",
			"last_refresh_at":     "2026-08-08T02:59:30Z",
			"stale_after_seconds": 60,
			"sources": []any{
				map[string]any{"name": "candidates", "degraded": true},
			},
		},
		"counts": map[string]any{"running": 2, "retrying": 1, "blocked": 1},
		"running": []any{
			map[string]any{"issue_identifier": "digitaldrywood/detent#1644"},
			map[string]any{"issue_identifier": "digitaldrywood/detent#1643"},
		},
		"retrying": []any{},
		"blocked":  []any{},
	}
}

func stateRows(count int) []any {
	rows := make([]any, count)
	for index := range rows {
		rows[index] = map[string]any{"index": index}
	}
	return rows
}
