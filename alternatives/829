package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestDashboardReadClientStateScoping(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
				if got := request.URL.Query().Get("projection"); got != serviceapi.StateProjection {
					t.Errorf("projection = %q", got)
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
		Limit:         stateCollectionLimit,
		MaxBytes:      serviceapi.StateResponseBytes,
		ValueMaxBytes: serviceapi.StateValueBytes,
		Truncated:     true,
		Collections: []StateCollectionTruncation{
			{Path: "/refresh/sources", Omitted: 1, Total: 101, Returned: 100},
			{Path: "/running", Omitted: 3, Total: 103, Returned: 100},
		},
	}
	if !reflect.DeepEqual(state.Truncation, wantTruncation) {
		t.Fatalf("truncation = %#v, want %#v", state.Truncation, wantTruncation)
	}
}

func TestDashboardReadClientStateProblemsMatchExplain(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
		{
			name:   "legacy service ignoring projection remains transport bounded",
			status: http.StatusOK, body: `{"large":"` + strings.Repeat("x", dashboardResponseBodyMax) + `"}`,
			wantCode: errorCodeGeneral, wantExit: ExitGeneral,
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
			if err == nil {
				t.Fatal("expected state read failure")
			}
			problem := ProblemForError(classifyStateReadError(err))
			if problem.Code != tt.wantCode || problem.ExitCode != tt.wantExit {
				t.Fatalf("problem = %#v, want code %q exit %d", problem, tt.wantCode, tt.wantExit)
			}
		})
	}
}

func TestStateCommandOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()

	tests := []struct {
		name          string
		stdoutTTY     bool
		args          []string
		wantPretty    []string
		wantJSONBuild bool
		large         bool
	}{
		{name: "JSON projection", args: []string{"--project", "detent"}, wantJSONBuild: true},
		{name: "large fleet over HTTP", large: true},
		{name: "large project over HTTP", args: []string{"--project", "detent"}, large: true},
		{name: "large pretty project", args: []string{"--project", "detent"}, large: true, stdoutTTY: true, wantPretty: []string{"Running: 103", "Truncated: true", "Omitted /running/0/issue_title: byte_limit"}},
		{name: "pretty projection", stdoutTTY: true, args: []string{"--project", "detent"}, wantPretty: []string{"Status: running", "Generated at: 2026-08-08T03:00:00Z", "Degraded: true", "Refresh status: degraded", "Enrichment status: ready", "Enrichment completed snapshot: 2026-08-08T03:00:00Z", "Running: 2", "Ready: 3", "Waiting: 4", "Blocked: 1", "Memory PSI some avg60: 0% / 10% threshold (admitting)", "I/O PSI full avg10: 63.64% / 5% threshold (limited to 1 agent for 5m0s)", "CPU PSI some avg10: 91.2% / 80% threshold (holding dispatch)", "Truncated: false"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var handler http.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/projects/detent/state" {
					t.Errorf("path = %q", request.URL.Path)
				}
				_ = json.NewEncoder(writer).Encode(stateFixture())
			})
			if tt.large {
				// Catches reading the unbounded state before projection, including
				// nested arrays and a single entry larger than the transport cap.
				snapshots := hub.New[telemetry.Snapshot]()
				refresh := telemetry.Refresh{Status: telemetry.RefreshStatus("degraded"), Sources: make([]telemetry.RefreshSource, 103)}
				refresh.Sources[0].Degraded = true
				snapshot := telemetry.Snapshot{
					GeneratedAt: time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC),
					Refresh:     refresh, Counts: telemetry.Counts{Running: 103},
					Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "detent"}, Counts: telemetry.Counts{Running: 103}, Refresh: refresh}},
					Running:  make([]telemetry.Running, 104),
				}
				for i := range snapshot.Running {
					snapshot.Running[i].Issue = telemetry.Issue{ID: strconv.Itoa(i), Identifier: "detent#" + strconv.Itoa(i), ProjectID: "detent"}
				}
				// This other project must be filtered before collection totals are taken.
				snapshot.Running[103].ProjectID = "other"
				snapshot.Running[0].Title = strings.Repeat("large entry ", 1<<18)
				if err := snapshots.Publish(snapshot); err != nil {
					t.Fatal(err)
				}
				backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "state.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := backend.Close(); err != nil {
						t.Error(err)
					}
				})
				dashboard, err := web.NewServer(web.Config{LookupEnv: func(string) string { return "" }}, web.Dependencies{Hub: snapshots, Store: backend, Registry: project.NewRegistry(), Connector: memory.New(memory.Config{})})
				if err != nil {
					t.Fatal(err)
				}
				handler = dashboard.Handler()
				path := "/api/v1/state"
				if len(tt.args) > 0 {
					path = "/api/v1/projects/detent/state"
				}
				full := httptest.NewRecorder()
				handler.ServeHTTP(full, httptest.NewRequest(http.MethodGet, path, nil))
				if full.Code != http.StatusOK || full.Body.Len() <= dashboardResponseBodyMax {
					t.Fatalf("full state: status=%d bytes=%d", full.Code, full.Body.Len())
				}
				var unbounded map[string]any
				if err := decodeDashboardJSON(bytes.NewReader(full.Body.Bytes()), &unbounded); err == nil || !strings.Contains(err.Error(), "response exceeds 1048576 bytes") {
					t.Fatalf("unbounded state read = %v, want transport limit failure", err)
				}
				t.Logf("unbounded state reproduces transport failure: %d bytes", full.Body.Len())
			}
			server := httptest.NewServer(handler)
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
			for _, key := range []string{"generated_at", "refresh", "enrichment", "running", "io_pressure", "cpu_pressure", "truncation"} {
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
			if tt.large {
				if stdout.Len() > dashboardResponseBodyMax {
					t.Fatalf("bounded output has %d bytes", stdout.Len())
				}
				var truncation StateTruncation
				if err := json.Unmarshal(object["truncation"], &truncation); err != nil {
					t.Fatal(err)
				}
				if !truncation.Truncated {
					t.Fatal("large state silently presented as complete")
				}
				var rows []map[string]any
				if err := json.Unmarshal(object["running"], &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 100 {
					t.Fatalf("running rows = %d", len(rows))
				}
				for i, row := range rows {
					if row["issue_identifier"] != "detent#"+strconv.Itoa(i) {
						t.Fatalf("running[%d] = %v", i, row["issue_identifier"])
					}
				}
				var generatedAt string
				if err := json.Unmarshal(object["generated_at"], &generatedAt); err != nil {
					t.Fatal(err)
				}
				if generatedAt != "2026-08-08T03:00:00Z" {
					t.Fatalf("generated_at = %q", generatedAt)
				}
				var counts struct {
					Running int `json:"running"`
				}
				if err := json.Unmarshal(object["counts"], &counts); err != nil {
					t.Fatal(err)
				}
				wantRunning := 104
				if len(tt.args) > 0 {
					wantRunning = 103
				}
				if counts.Running != wantRunning {
					t.Fatalf("original counts = %v", counts)
				}
				for _, want := range []StateCollectionTruncation{{Path: "/running", Total: wantRunning, Returned: 100, Omitted: wantRunning - 100}, {Path: "/refresh/sources", Total: 103, Returned: 100, Omitted: 3}} {
					found := false
					for _, got := range truncation.Collections {
						if got == want {
							found = true
						}
					}
					if !found {
						t.Fatalf("missing truncation %+v in %+v", want, truncation.Collections)
					}
				}
				foundTitle := false
				for _, field := range truncation.OmittedFields {
					if field.Path == "/running/0/issue_title" && field.Reason == "byte_limit" {
						foundTitle = true
					}
				}
				if !foundTitle {
					t.Fatalf("missing oversized title omission: %+v", truncation)
				}
				if _, ok := rows[0]["issue_title"]; ok {
					t.Fatal("oversized title retained")
				}
				var refresh map[string]any
				if err := json.Unmarshal(object["refresh"], &refresh); err != nil {
					t.Fatal(err)
				}
				if refresh["status"] != "degraded" || len(refresh["sources"].([]any)) != 100 {
					t.Fatalf("refresh = %+v", refresh)
				}
				var enrichment map[string]any
				if err := json.Unmarshal(object["enrichment"], &enrichment); err != nil {
					t.Fatal(err)
				}
				if enrichment["status"] != "omitted" || enrichment["degraded_reason"] == "" {
					t.Fatalf("enrichment = %+v", enrichment)
				}
				t.Logf("bounded command output: %d bytes, scoped running total %d", stdout.Len(), counts.Running)
			}
		})
	}
}

func TestWriteStatePrettyReportsEnrichment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		enrichment map[string]any
		degraded   bool
		want       []string
	}{
		{
			name:       "ready",
			enrichment: map[string]any{"status": "ready", "completed_snapshot_generated_at": "2026-08-08T03:00:00Z"},
			want:       []string{"Enrichment status: ready", "Enrichment completed snapshot: 2026-08-08T03:00:00Z"},
		},
		{
			name:       "pending",
			enrichment: map[string]any{"status": "pending", "completed_snapshot_generated_at": "2026-08-08T02:59:00Z", "degraded_reason": "refresh in progress"},
			degraded:   true,
			want:       []string{"Enrichment status: pending", "Enrichment completed snapshot: 2026-08-08T02:59:00Z", "Enrichment reason: refresh in progress"},
		},
		{
			name:       "omitted",
			enrichment: map[string]any{"status": "omitted", "degraded_reason": "no completed enrichment available"},
			degraded:   true,
			want:       []string{"Enrichment status: omitted", "Enrichment reason: no completed enrichment available"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := DashboardState{payload: map[string]any{
				"generated_at": "2026-08-08T03:00:00Z",
				"status":       "running",
				"enrichment":   tt.enrichment,
			}}
			var output bytes.Buffer
			if err := writeStatePretty(&output, state); err != nil {
				t.Fatalf("writeStatePretty() error = %v", err)
			}
			for _, want := range append([]string{fmt.Sprintf("Degraded: %t", tt.degraded)}, tt.want...) {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("output missing %q:\n%s", want, output.String())
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
		"enrichment":   map[string]any{"status": "ready", "completed_snapshot_generated_at": "2026-08-08T03:00:00Z"},
		"refresh": map[string]any{
			"status":              "degraded",
			"last_refresh_at":     "2026-08-08T02:59:30Z",
			"stale_after_seconds": 60,
			"sources": []any{
				map[string]any{"name": "candidates", "degraded": true},
			},
		},
		"counts": map[string]any{"running": 2, "retrying": 1, "ready": 3, "waiting": 4, "blocked": 1},
		"memory_pressure": map[string]any{
			"supported": true, "some": map[string]any{"avg60": 0}, "some_avg60_max": 10, "dispatch_held": false,
		},
		"io_pressure": map[string]any{
			"supported": true, "full": map[string]any{"avg10": 63.64}, "full_avg10_max": 5, "dispatch_held": false,
			"capacity_constrained": true, "degraded_max_concurrent_agents": 1, "effective_max_concurrent_agents": 1, "constrained_for_ms": 300000,
		},
		"cpu_pressure": map[string]any{
			"supported": true, "some": map[string]any{"avg10": 91.2}, "some_avg10_max": 80, "dispatch_held": true,
		},
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
