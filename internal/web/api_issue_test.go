package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestAPIIssueResolvesIdleBoardReferences(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{
		ID:         "issue-1640",
		Identifier: "digitaldrywood/detent#1640",
		Number:     1640,
		ProjectID:  "detent",
		URL:        "https://github.com/digitaldrywood/detent/issues/1640",
		State:      "Rework",
	}
	tests := []struct {
		name      string
		reference string
	}{
		{name: "ID", reference: issue.ID},
		{name: "canonical identifier", reference: issue.Identifier},
		{name: "URL", reference: issue.URL},
		{name: "bare number", reference: "1640"},
		{name: "hash number", reference: "#1640"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshots := hub.New[telemetry.Snapshot]()
			if err := snapshots.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			deps := testDeps(t)
			deps.Hub = snapshots
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			payload := requestJSON(t, server, http.MethodGet, "/api/v1/"+url.PathEscape(tt.reference), http.StatusOK)
			if payload["issue_id"] != issue.ID || payload["issue_identifier"] != issue.Identifier {
				t.Fatalf("identity = %#v, want %s / %s", payload, issue.ID, issue.Identifier)
			}
			if payload["lane"] != "Rework" || payload["activity"] != "idle" {
				t.Fatalf("lane/activity = %#v/%#v, want Rework/idle", payload["lane"], payload["activity"])
			}
		})
	}
}

func TestAPIIssuePrecedenceAndScope(t *testing.T) {
	t.Parallel()

	base := telemetry.Issue{
		ID:         "issue-42",
		Identifier: "digitaldrywood/detent#42",
		Number:     42,
		ProjectID:  "detent",
		URL:        "https://github.com/digitaldrywood/detent/issues/42",
	}
	board := base
	board.State = "Rework"
	pipeline := base
	pipeline.State = "Todo"
	running := base
	running.State = "In Progress"
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{board},
		Pipeline:    []telemetry.Issue{pipeline},
		Running:     []telemetry.Running{{Issue: running}},
		Queue:       []telemetry.Queued{{Issue: running}},
		Blocked:     []telemetry.Blocked{{Issue: running}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	deps := testDeps(t)
	deps.Hub = snapshots
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	payload := requestJSON(t, server, http.MethodGet, "/api/v1/%2342", http.StatusOK)
	if payload["lane"] != "Rework" || payload["activity"] != "running" || payload["status"] != "running" {
		t.Fatalf("precedence payload = %#v, want board lane and running activity", payload)
	}
	if payload["retry"] != nil || payload["blocked"] != nil {
		t.Fatalf("runtime precedence payload = %#v, want exactly one running overlay", payload)
	}
}

func TestAPIIssueSurfacesWorkpadBlockerResolution(t *testing.T) {
	t.Parallel()

	openRef := telemetry.BlockedRef{Identifier: "gopherguides/corp#492", State: "In Progress", TrackerState: "open", Source: "workpad"}
	closedRef := telemetry.BlockedRef{Identifier: "gopherguides/corp#491", State: "Done", TrackerState: "closed", Source: "workpad"}
	tests := []struct {
		name     string
		refs     []telemetry.BlockedRef
		wantJSON []string
	}{
		{name: "open ref is live", refs: []telemetry.BlockedRef{openRef}, wantJSON: []string{`"tracker_state":"open"`}},
		{name: "closed ref is resolved", refs: []telemetry.BlockedRef{closedRef}, wantJSON: []string{`"tracker_state":"closed"`}},
		{name: "mixed refs retain human hold", refs: []telemetry.BlockedRef{closedRef, openRef}, wantJSON: []string{`"tracker_state":"closed"`, `"tracker_state":"open"`, `"recovery_reason":"human_action"`, `"recovery_remedy":"approve the remaining deployment"`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := telemetry.Issue{ID: "issue-486", Identifier: "gopherguides/corp#486", Number: 486, ProjectID: "corp", State: "Blocked", BlockedBy: tt.refs}
			snapshots := hub.New[telemetry.Snapshot]()
			if err := snapshots.Publish(telemetry.Snapshot{Blocked: []telemetry.Blocked{{
				Issue:               issue,
				Error:               "stale Workpad blocker reason",
				RecoveryAction:      "hold",
				RecoveryReason:      "human_action",
				RecoveryRemedy:      "approve the remaining deployment",
				NeedsHumanAttention: true,
			}}}); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			deps := testDeps(t)
			deps.Hub = snapshots
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			for _, path := range []string{"/api/v1/issue-486", "/api/v1/state"} {
				payload := requestJSON(t, server, http.MethodGet, path, http.StatusOK)
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatalf("json.Marshal() error = %v", err)
				}
				body := string(encoded)
				for _, want := range tt.wantJSON {
					if !strings.Contains(body, want) {
						t.Fatalf("GET %s missing %q: %s", path, want, body)
					}
				}
			}
		})
	}
}

func TestAPIIssueProjectCollision(t *testing.T) {
	t.Parallel()

	issues := []telemetry.Issue{
		{ID: "alpha-42", Identifier: "example/alpha#42", Number: 42, ProjectID: "alpha", State: "Todo"},
		{ID: "beta-42", Identifier: "example/beta#42", Number: 42, ProjectID: "beta", State: "Rework"},
	}
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{BoardIssues: issues}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	deps := testDeps(t)
	deps.Hub = snapshots
	server, err := web.NewServer(web.Config{}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ambiguous := requestJSON(t, server, http.MethodGet, "/api/v1/%2342", http.StatusConflict)
	if nestedString(t, ambiguous, "error", "code") != "ambiguous_issue_reference" {
		t.Fatalf("ambiguity payload = %#v", ambiguous)
	}
	scoped := requestJSON(t, server, http.MethodGet, "/api/v1/%2342?project=beta", http.StatusOK)
	if scoped["issue_id"] != "beta-42" || scoped["project_id"] != "beta" {
		t.Fatalf("scoped payload = %#v, want beta issue", scoped)
	}
}

func TestAPIIssueScopeAndUnavailableSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		snapshot   *telemetry.Snapshot
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name: "completed is excluded",
			snapshot: &telemetry.Snapshot{Completed: []telemetry.Completed{{Issue: telemetry.Issue{
				ID: "completed-1", Identifier: "example/repo#1", Number: 1, ProjectID: "detent", State: "Done",
			}}}},
			path:       "/api/v1/completed-1",
			wantStatus: http.StatusNotFound,
			wantCode:   "issue_not_found",
		},
		{
			name: "drift-only is excluded",
			snapshot: &telemetry.Snapshot{TrackerDrift: telemetry.TrackerDrift{
				UntrackedOpen: []telemetry.Issue{{ID: "drift-only", ProjectID: "detent", State: "Todo"}},
			}},
			path:       "/api/v1/drift-only",
			wantStatus: http.StatusNotFound,
			wantCode:   "issue_not_found",
		},
		{
			name:       "snapshot unavailable",
			path:       "/api/v1/issue-1",
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "snapshot_unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshots := hub.New[telemetry.Snapshot]()
			if tt.snapshot != nil {
				if err := snapshots.Publish(*tt.snapshot); err != nil {
					t.Fatalf("Publish() error = %v", err)
				}
			}
			deps := testDeps(t)
			deps.Hub = snapshots
			server, err := web.NewServer(web.Config{}, deps)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			payload := requestJSON(t, server, http.MethodGet, tt.path, tt.wantStatus)
			if got := nestedString(t, payload, "error", "code"); got != tt.wantCode {
				t.Fatalf("error code = %q, want %q; payload = %#v", got, tt.wantCode, payload)
			}
		})
	}
}

func TestAPIIssueExplicitRoutesPrecedeCatchAll(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	payload := requestJSON(t, server, http.MethodGet, "/api/v1/state", http.StatusOK)
	if _, ok := payload["generated_at"]; !ok || payload["issue_id"] != nil {
		t.Fatalf("state route payload = %#v, want explicit state handler", payload)
	}
}
