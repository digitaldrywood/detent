package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/serviceapi"
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
			if got := state.field("generated_at"); got != "2026-08-08T03:00:00Z" {
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
				if got := state.field("error").(map[string]any)["code"]; got != "snapshot_unavailable" {
					t.Fatalf("degraded error code = %q", got)
				}
				return
			}
			if err == nil {
				t.Fatal("expected state read failure")
			}
			problem := ProblemForError(classifyDashboardReadError(err))
			if problem.Code != tt.wantCode || problem.ExitCode != tt.wantExit {
				t.Fatalf("problem = %#v, want code %q exit %d", problem, tt.wantCode, tt.wantExit)
			}
		})
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
