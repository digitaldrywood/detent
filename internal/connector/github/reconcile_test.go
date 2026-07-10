package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorReconcileIssueFetchesOneLabelIssue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		repository string
		labels     string
		wantState  string
	}{
		{name: "configured status label", repository: "digitaldrywood/detent", labels: `[{"name":"detent:in-progress"}]`, wantState: "In Progress"},
		{name: "status label removed", repository: "digitaldrywood/detent", labels: `[{"name":"enhancement"}]`, wantState: "Open"},
		{name: "repository case differs", repository: "DigitalDrywood/Detent", labels: `[{"name":"detent:in-progress"}]`, wantState: "In Progress"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/digitaldrywood/detent/issues/1133" {
					t.Fatalf("path = %q, want targeted issue path", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"number":1133,"node_id":"I_1133","title":"Board freshness","body":"","state":"open","html_url":"https://github.com/digitaldrywood/detent/issues/1133","labels":` + tt.labels + `}`))
			}))
			t.Cleanup(server.Close)

			tracker, err := NewConnector(Config{
				Endpoint:           server.URL,
				APIKey:             "test-token",
				HTTPClient:         server.Client(),
				GitHubStatusSource: GitHubStatusSourceLabel,
				Repository:         tt.repository,
				ActiveStates:       []string{"Todo", "In Progress"},
				ObservedStates:     []string{"Blocked"},
				TerminalStates:     []string{"Done"},
			})
			if err != nil {
				t.Fatalf("NewConnector() error = %v", err)
			}

			result, err := tracker.ReconcileIssue(context.Background(), connector.ReconcileTarget{
				Scope:          "digitaldrywood/detent",
				WorkItemNumber: 1133,
				Event:          "issues",
			})
			if err != nil {
				t.Fatalf("ReconcileIssue() error = %v", err)
			}
			if !result.Found || result.Issue.ID != "I_1133" || result.Issue.State != tt.wantState {
				t.Fatalf("ReconcileIssue() = %#v, want found issue in %q", result, tt.wantState)
			}
		})
	}
}

func TestIssueRefFromDetentBranch(t *testing.T) {
	t.Parallel()

	repo := pullRequestRepo{Owner: "digitaldrywood", Name: "detent"}
	tests := []struct {
		name       string
		branch     string
		wantNumber int
		wantOK     bool
	}{
		{name: "current workspace format", branch: "detent/detent-digitaldrywood_detent_1133-94412618830b", wantNumber: 1133, wantOK: true},
		{name: "canonical prefix", branch: "detent/digitaldrywood_detent_1133-feature", wantNumber: 1133, wantOK: true},
		{name: "unrelated branch", branch: "feature/1133", wantOK: false},
		{name: "other repository", branch: "detent/digitaldrywood_other_1133", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, ok := issueRefFromDetentBranch(repo, tt.branch)
			if ok != tt.wantOK || ref.Number != tt.wantNumber {
				t.Fatalf("issueRefFromDetentBranch() = %#v, %v; want number %d, ok %v", ref, ok, tt.wantNumber, tt.wantOK)
			}
		})
	}
}
