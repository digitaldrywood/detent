package workitem_test

import (
	"context"
	"errors"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func TestCreateBuildsWorkItemWithDefaults(t *testing.T) {
	t.Parallel()

	conn := &creatorConnector{}
	now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	got, err := workitem.Create(context.Background(), workitem.Target{
		ProjectID:           "video",
		Workflow:            localSQLiteWorkflow(),
		Connector:           conn,
		DashboardURL:        "http://127.0.0.1:4000",
		Now:                 func() time.Time { return now },
		IdentifierGenerator: func() (string, error) { return "wi-test", nil },
	}, workitem.Request{
		Title:       " Author beat visuals ",
		Description: " Render storyboard frames ",
		Labels:      []string{" video-assets ", ""},
		Fields:      map[string]string{" render_status ": " queued "},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got.ID != "wi-test" || got.Identifier != "wi-test" {
		t.Fatalf("Create() response = %#v, want generated identifier", got)
	}
	if got.URL != "http://127.0.0.1:4000/projects/video/kanban" {
		t.Fatalf("URL = %q", got.URL)
	}
	if len(conn.upserts) != 1 {
		t.Fatalf("upserts len = %d, want 1", len(conn.upserts))
	}
	issue := conn.upserts[0]
	if issue.Title != "Author beat visuals" || issue.Description != "Render storyboard frames" {
		t.Fatalf("issue text = %#v", issue)
	}
	if issue.State != "Todo" {
		t.Fatalf("State = %q, want Todo", issue.State)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "video-assets" {
		t.Fatalf("Labels = %#v", issue.Labels)
	}
	if issue.Fields["render_status"] != "queued" {
		t.Fatalf("Fields = %#v", issue.Fields)
	}
	if issue.CreatedAt == nil || !issue.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", issue.CreatedAt, now)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  workitem.Request
		want workitem.ErrorCode
	}{
		{
			name: "missing title",
			req:  workitem.Request{Description: "body"},
			want: workitem.CodeMissingTitle,
		},
		{
			name: "missing description",
			req:  workitem.Request{Title: "title"},
			want: workitem.CodeMissingDescription,
		},
		{
			name: "invalid state",
			req:  workitem.Request{Title: "title", Description: "body", State: "Missing"},
			want: workitem.CodeInvalidState,
		},
		{
			name: "blank field key",
			req:  workitem.Request{Title: "title", Description: "body", Fields: map[string]string{" ": "value"}},
			want: workitem.CodeInvalidFields,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := workitem.Create(context.Background(), workitem.Target{
				Workflow:  localSQLiteWorkflow(),
				Connector: &creatorConnector{},
			}, tt.req)
			var itemErr *workitem.Error
			if !errors.As(err, &itemErr) {
				t.Fatalf("Create() error = %v, want workitem.Error", err)
			}
			if itemErr.Code != tt.want {
				t.Fatalf("error code = %q, want %q", itemErr.Code, tt.want)
			}
		})
	}
}

func TestCreateRejectsDuplicateIdentifier(t *testing.T) {
	t.Parallel()

	conn := &creatorConnector{
		existing: []connector.Issue{{ID: "existing", Identifier: "external-123"}},
	}
	_, err := workitem.Create(context.Background(), workitem.Target{
		Workflow:  localSQLiteWorkflow(),
		Connector: conn,
	}, workitem.Request{
		Title:       "title",
		Description: "body",
		Identifier:  "external-123",
	})
	var itemErr *workitem.Error
	if !errors.As(err, &itemErr) {
		t.Fatalf("Create() error = %v, want workitem.Error", err)
	}
	if itemErr.Code != workitem.CodeDuplicateIdentifier {
		t.Fatalf("error code = %q, want %q", itemErr.Code, workitem.CodeDuplicateIdentifier)
	}
}

func TestCreateConnectorCapabilityGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		conn connector.Connector
		want workitem.ErrorCode
	}{
		{
			name: "upserter succeeds",
			conn: &creatorConnector{},
		},
		{
			name: "connector without upserter is unsupported",
			conn: readonlyConnector{},
			want: workitem.CodeUnsupportedTracker,
		},
		{
			name: "nil connector is unavailable",
			want: workitem.CodeConnectorUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := workitem.Create(context.Background(), workitem.Target{
				Workflow:            localSQLiteWorkflow(),
				Connector:           tt.conn,
				IdentifierGenerator: func() (string, error) { return "wi-gate", nil },
			}, workitem.Request{
				Title:       "title",
				Description: "body",
			})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Create() error = %v", err)
				}
				if got.Identifier != "wi-gate" {
					t.Fatalf("Identifier = %q, want wi-gate", got.Identifier)
				}
				return
			}

			var itemErr *workitem.Error
			if !errors.As(err, &itemErr) {
				t.Fatalf("Create() error = %v, want workitem.Error", err)
			}
			if itemErr.Code != tt.want {
				t.Fatalf("error code = %q, want %q", itemErr.Code, tt.want)
			}
		})
	}
}

func localSQLiteWorkflow() workflowconfig.Config {
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerLocalSQLite
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	cfg.Tracker.ObservedStates = []string{"Backlog", "Blocked"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	return cfg
}

type creatorConnector struct {
	upserts  []connector.Issue
	existing []connector.Issue
}

func (c *creatorConnector) Name() string {
	return connector.BackendLocalSQLite.String()
}

func (c *creatorConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *creatorConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *creatorConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *creatorConnector) FetchIssueStatesByIdentifiers(context.Context, []string) ([]connector.Issue, error) {
	return append([]connector.Issue(nil), c.existing...), nil
}

func (c *creatorConnector) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *creatorConnector) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *creatorConnector) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (c *creatorConnector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

func (c *creatorConnector) UpsertIssues(_ context.Context, issues []connector.Issue) error {
	c.upserts = append(c.upserts, issues...)
	return nil
}

type readonlyConnector struct{}

func (readonlyConnector) Name() string {
	return connector.BackendMemory.String()
}

func (readonlyConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (readonlyConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (readonlyConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (readonlyConnector) CreateComment(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (readonlyConnector) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (readonlyConnector) SetAssignee(context.Context, string, string) error {
	return connector.ErrNotImplemented
}

func (readonlyConnector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}
