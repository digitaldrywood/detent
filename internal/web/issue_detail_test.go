package web_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestLocalIssueDetailRendersFullSpecificationAndHistory(t *testing.T) {
	t.Parallel()

	server := newLocalIssueDetailTestServer(t)
	body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/video/issues/wi-detail", http.StatusOK)
	for _, want := range []string{
		"Readable local issue",
		"Final acceptance marker must remain visible after the preview boundary.",
		"&lt;unsafe&gt; content must remain escaped.",
		"video-assets",
		"render_status",
		"ready",
		"Operator requested a tighter final cut.",
		"State changed",
		"In Progress",
		"Fields updated",
		"render_status → ready",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue detail missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<unsafe>") {
		t.Fatalf("issue detail rendered unsafe description markup:\n%s", body)
	}
}

func TestLocalBoardCardLinksToInternalIssueDetail(t *testing.T) {
	t.Parallel()

	server := newLocalIssueDetailTestServer(t)
	body := requestHTML(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=video&issue=wi-detail&scope=project", http.StatusOK)
	for _, want := range []string{
		"Readable local issue",
		`href="http://detent.example/projects/video/issues/wi-detail"`,
		"Open issue",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("local board sheet missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"Open on GitHub", "https://github.com/example/repository"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("local board sheet contains %q:\n%s", unwanted, body)
		}
	}
}

func TestAIDebugPromptRoutesReturnSelfContainedScopes(t *testing.T) {
	t.Parallel()

	server := newLocalIssueDetailTestServer(t)
	tests := []struct {
		name     string
		path     string
		want     []string
		dontWant []string
	}{
		{
			name: "issue",
			path: "/api/v1/ai-debug?scope=issue&project=video&issue=wi-detail",
			want: []string{"## Issue evidence", "## Project evidence", "## Fleet evidence", `"identifier": "wi-detail"`, `"cause": "No blocked cause is recorded."`, "Do not request tool access"},
		},
		{
			name:     "project",
			path:     "/api/v1/ai-debug?scope=project&project=video",
			want:     []string{"## Project evidence", "## Fleet evidence", `"id": "video"`, "configuration_or_workflow_issue_destination_repository", `"drift_status": "comparison failed: workflow path must not be blank"`},
			dontWant: []string{"## Issue evidence"},
		},
		{
			name:     "fleet",
			path:     "/api/v1/ai-debug?scope=fleet",
			want:     []string{"## Fleet evidence", `"scope": "fleet"`, "github_budgets_by_endpoint_family"},
			dontWant: []string{"## Issue evidence", "## Project evidence"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := requestHTML(t, server.Handler(), http.MethodGet, tt.path, http.StatusOK)
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Fatalf("AI Debug prompt missing %q:\n%s", want, body)
				}
			}
			for _, dontWant := range tt.dontWant {
				if strings.Contains(body, dontWant) {
					t.Fatalf("AI Debug prompt contains %q:\n%s", dontWant, body)
				}
			}
		})
	}
}

func newLocalIssueDetailTestServer(t *testing.T) *web.Server {
	t.Helper()

	ctx := context.Background()
	createdAt := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(30 * time.Minute)
	issue := connector.NewIssue()
	issue.ID = "wi-detail"
	issue.Identifier = "wi-detail"
	issue.Number = 2
	issue.Title = "Readable local issue"
	issue.Description = strings.Repeat("Detailed requirement remains part of the specification. ", 8) + "\n\nFinal acceptance marker must remain visible after the preview boundary.\n\n<unsafe> content must remain escaped."
	issue.State = "Todo"
	issue.URL = "https://github.com/example/repository"
	issue.Labels = []string{"video-assets"}
	issue.Fields = map[string]string{"render_status": "queued"}
	issue.CreatedAt = &createdAt
	issue.UpdatedAt = &updatedAt
	issue.StageUpdatedAt = &updatedAt

	conn, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "work-items.db"),
		ProjectID:      "video",
		Issues:         []connector.Issue{issue},
		ActiveStates:   []string{"Todo", "In Progress"},
		ObservedStates: []string{"Backlog", "Blocked"},
		TerminalStates: []string{"Done"},
		Now:            func() time.Time { return updatedAt },
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := conn.CreateComment(ctx, issue.ID, "Operator requested a tighter final cut."); err != nil {
		t.Fatalf("CreateComment() error = %v", err)
	}
	if err := conn.UpdateIssueState(ctx, issue.ID, "In Progress"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}
	if err := conn.SetField(ctx, issue.ID, "render_status", "ready"); err != nil {
		t.Fatalf("SetField() error = %v", err)
	}

	workflow := workflowconfig.Default()
	workflow.Tracker.Kind = workflowconfig.TrackerLocalSQLite
	workflow.Tracker.LocalSQLite.Path = filepath.Join(t.TempDir(), "unused.db")
	workflow.Tracker.LocalSQLite.ProjectID = "video"
	workflow.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	workflow.Tracker.ObservedStates = []string{"Backlog", "Blocked"}
	workflow.Tracker.TerminalStates = []string{"Done"}
	trackedProject, err := project.New(project.Config{
		Project:  globalconfig.Project{ID: "video"},
		Workflow: workflowconfig.Workflow{Config: workflow, Prompt: "Work the issue."},
	}, project.Dependencies{Connector: conn})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}

	registry := project.NewRegistry()
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
	snapshotHub := hub.New[telemetry.Snapshot]()
	if err := snapshotHub.Publish(telemetry.Snapshot{
		GeneratedAt: updatedAt,
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "video", DisplayName: "Video"},
		}},
		BoardIssues: []telemetry.Issue{{
			ID:          issue.ID,
			Identifier:  issue.Identifier,
			Number:      issue.Number,
			ProjectID:   "video",
			Title:       issue.Title,
			Description: issue.Description,
			State:       "In Progress",
			URL:         issue.URL,
			Labels:      issue.Labels,
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshotHub
	deps.Store = openWebTestStore(t)
	deps.Registry = registry
	deps.Connector = conn
	server, err := web.NewServer(web.Config{
		DashboardURL: "http://detent.example",
		StaticDir:    t.TempDir(),
	}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}
