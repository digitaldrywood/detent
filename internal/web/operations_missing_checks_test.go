package web_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	webconfig "github.com/digitaldrywood/detent/internal/web"
)

func TestOperationsMissingRequiredChecks(t *testing.T) {
	for _, tt := range []struct {
		name, state, status, label string
		existing                   bool
		unavailable                bool
		closed                     bool
		want                       int
	}{
		{name: "missing", state: "Merging", status: "missing", want: 1},
		{name: "configured label", state: "Merging", status: "missing", label: "run-full-ci"},
		{name: "hydration unavailable without missing evidence", state: "Merging", unavailable: true},
		{name: "known missing during cooldown", state: "Merging", status: "missing", unavailable: true, want: 1},
		{name: "closed PR", state: "Merging", status: "missing", closed: true},
		{name: "pending", state: "Merging", status: "pending"},
		{name: "different lane", state: "In Progress", status: "missing"},
		{name: "existing question", state: "Merging", status: "missing", existing: true, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(t)
			setOperationsTestProject(t, deps.Registry, "p", true, "", nil, "", "", "", "", []string{"Done"})
			if tt.label != "" {
				cfg := workflowconfig.Default()
				cfg.Tracker.Kind = workflowconfig.TrackerMemory
				cfg.Gate.CITriggerLabel = tt.label
				tracked, err := project.New(project.Config{Project: global.Project{ID: "p"}, Workflow: workflowconfig.Workflow{Config: cfg}}, project.Dependencies{Connector: connectorProbe{name: "memory"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := deps.Registry.Set(tracked); err != nil {
					t.Fatal(err)
				}
			}

			report := operations.Report{DataTime: time.Now()}
			if tt.existing {
				report.Decisions = []operations.Decision{{ProjectID: "p", Issue: "owner/repo#1", Question: "Existing"}}
			}
			deps.Store = operationsStore{Store: openWebTestStore(t), report: report}
			issue := telemetry.Issue{ID: "i", Title: "Label-gated Full CI", URL: "https://github.com/owner/repo/issues/1", Identifier: "owner/repo#1", ProjectID: "p", State: tt.state, PullRequest: &telemetry.PullRequest{Number: 42, State: "open", HeadSHA: "head", RequiredCheckFailures: []telemetry.PullRequestCheck{{Name: "Full CI", Status: tt.status}, {Name: "Full CI", Status: tt.status}}}}
			if tt.unavailable {
				issue.PullRequest.HydrationUnavailableReason = "rate_limited"
			}
			if tt.closed {
				issue.PullRequest.State = "merged"
			}
			if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}, Pipeline: []telemetry.Issue{issue}}); err != nil {
				t.Fatal(err)
			}
			server, err := newServerWithLaneWriter(webconfig.Config{ServerAddress: "127.0.0.1:0", SSEFragmentInterval: -1, LookupEnv: func(string) string { return "" }}, deps)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			card := httptest.NewRecorder()
			server.Handler().ServeHTTP(card, req)
			wantCard := tt.state == "Merging" && tt.status == "missing" && tt.label == "" && !tt.closed
			if strings.Contains(card.Body.String(), "Needs you") != wantCard {
				t.Fatalf("card human action = %t, want %t; status=%d", strings.Contains(card.Body.String(), "Needs you"), wantCard, card.Code)
			}
			withBoardSSEStream(t, server.Handler(), "/events?view=kanban", func(reader *bufio.Reader, _ func()) {
				for range 2 {
					for {
						body := readBoardSSEData(t, reader)
						if !strings.Contains(body, issue.Title) {
							continue
						}
						if strings.Contains(body, "Needs you") != wantCard {
							t.Fatalf("streamed card human action = %t, want %t", strings.Contains(body, "Needs you"), wantCard)
						}
						break
					}
					issue.Title += " updated"
					if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}, Pipeline: []telemetry.Issue{issue}}); err != nil {
						t.Fatal(err)
					}
				}
			})
			for range 2 {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
				req.RemoteAddr = "127.0.0.1:12345"
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var got operations.Report
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Decisions) != tt.want {
					t.Fatalf("decisions=%+v", got.Decisions)
				}
				if tt.want > 0 && !tt.existing && (strings.Count(got.Decisions[0].Question, "Full CI") != 1 || !strings.Contains(got.Decisions[0].Question, "gate.ci_trigger_label")) {
					t.Fatalf("decision=%+v", got.Decisions[0])
				}
			}
		})
	}
}
