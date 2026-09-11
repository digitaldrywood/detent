package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

type operationsStore struct {
	store.Store
	report operations.Report
	err    error
}

func (s operationsStore) OperationsReport(context.Context, time.Time, time.Time) (operations.Report, error) {
	return s.report, s.err
}

func TestOperationsHandlers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		fragment   bool
		failure    bool
		status     int
		contains   []string
	}{
		{name: "api recorded sections", path: "/api/v1/operations", status: 200, contains: []string{`"merges":3`, `return_retired_parks`, `Choose target?`, `"instance"`}},
		{name: "dashboard recorded sections", path: "/operations", status: 200, contains: []string{`id="operations-stats"`, `id="operations-actions"`, `id="operations-decisions"`, `Choose target?`, `id="help-tooltip"`, `morph:innerHTML`}},
		{name: "fragment", path: "/operations", fragment: true, status: 200, contains: []string{`id="operations-stats"`, `Refresh`}},
		{name: "invalid cursor", path: "/api/v1/operations?since=invalid", status: 400},
		{name: "future cursor", path: "/api/v1/operations?since=2099-01-01T00:00:00Z", status: 400},
		{name: "history failure", path: "/api/v1/operations", failure: true, status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := testDeps(t)
			fixture := operations.Report{DataTime: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC), Stats: []operations.Window{{Label: "24h", Merges: 3}}, Actions: []operations.Action{{ID: 1, ProjectID: "p", Issue: "i", Kind: "return_retired_parks", Reason: "operator_routine:return_retired_parks"}}, Decisions: []operations.Decision{{ProjectID: "p", Issue: "owner/repo#1", Question: "Choose target?", URL: "https://github.com/owner/repo/issues/1"}}}
			backend := operationsStore{Store: openWebTestStore(t), report: fixture}
			if tc.failure {
				backend.err = errors.New("database unavailable")
			}
			deps.Store = backend
			if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ID: "i", ProjectID: "p", Identifier: "owner/repo#1", URL: "https://github.com/owner/repo/issues/1", RequiredGate: &telemetry.RequiredGate{HumanAction: "Choose target?"}, PullRequest: &telemetry.PullRequest{MergeQueueEntry: &telemetry.PullRequestMergeQueueEntry{Depth: 4, MaxGroupSize: 5, MinGroupWaitSeconds: 300}}}}}); err != nil {
				t.Fatal(err)
			}
			server, err := newServerWithLaneWriter(web.Config{ServerAddress: "127.0.0.1:0", LookupEnv: func(string) string { return "" }}, deps)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			if tc.fragment {
				req.Header.Set("HX-Request", "true")
			}
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			for _, want := range tc.contains {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("missing %q in %s", want, rec.Body.String())
				}
			}
			if tc.fragment && strings.Contains(rec.Body.String(), "<html") {
				t.Fatal("fragment contains page shell")
			}
			if tc.name == "api recorded sections" {
				var got operations.Report
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.QueueDepth != 4 {
					t.Fatalf("merge queue depth: %d", got.QueueDepth)
				}
				if got.MergeGroupSize == nil || *got.MergeGroupSize != 5 || got.MergeGroupWait == nil || *got.MergeGroupWait != 300 {
					t.Fatalf("merge group telemetry: size=%v wait=%v", got.MergeGroupSize, got.MergeGroupWait)
				}
				if len(got.Decisions) != 1 || got.Actions[0].EvidenceURL != "https://github.com/owner/repo/issues/1" {
					t.Fatalf("report: %#v", got)
				}
			}
		})
	}
}

func TestOperationsCurrentDecisionEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, stored, current, gate, want string }{
		{"current question", "current", "current", "New gate?", "Original question?"},
		{"superseded question", "old", "current", "", ""},
		{"superseded question allows current gate", "old", "current", "New gate?", "New gate?"},
		{"unfingerprinted question superseded", "", "current", "New gate?", "New gate?"},
		{"no new refusal evidence", "old", "", "", "Original question?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := testDeps(t)
			backend := openWebTestStore(t)
			q := store.HumanQuestion{ProjectID: "p", IssueID: "i", Identifier: "owner/repo#1", Key: "choice", Body: "Original question?", WorkFingerprint: tc.stored, QuestionCommentID: "123"}
			questions := backend.(store.HumanQuestionStore)
			if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			deps.Store = backend
			if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ID: "i", ProjectID: "p", Identifier: q.Identifier, RequiredGate: &telemetry.RequiredGate{HumanAction: tc.gate}, PullRequest: &telemetry.PullRequest{HumanQuestionWorkFingerprint: tc.current}}}}); err != nil {
				t.Fatal(err)
			}
			server, err := newServerWithLaneWriter(web.Config{ServerAddress: "127.0.0.1:0", LookupEnv: func(string) string { return "" }}, deps)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/operations", nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var report operations.Report
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(report.Decisions) != 0 {
					t.Fatalf("superseded decisions: %#v", report.Decisions)
				}
				return
			}
			if len(report.Decisions) != 1 || report.Decisions[0].Question != tc.want {
				t.Fatalf("decisions: %#v, want %q", report.Decisions, tc.want)
			}
		})
	}
}
