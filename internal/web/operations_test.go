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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/project"
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
	for _, tc := range []struct{ name, stored, current, gate, prState, want string }{
		{name: "current question", stored: "current", current: "current", gate: "New gate?", want: "Original question?"},
		{name: "superseded question", stored: "old", current: "current"},
		{name: "superseded question allows current gate", stored: "old", current: "current", gate: "New gate?", want: "New gate?"},
		{name: "unfingerprinted question superseded", current: "current", gate: "New gate?", want: "New gate?"},
		{name: "no new refusal evidence", stored: "old", want: "Original question?"},
		{name: "closed pull request supersedes question", stored: "current", current: "current", prState: "CLOSED"},
		{name: "merged pull request supersedes question", stored: "current", current: "current", prState: "MERGED"},
		{name: "closed pull request preserves independent gate", stored: "current", current: "current", gate: "Restore deployment credentials.", prState: "CLOSED", want: "Restore deployment credentials."},
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
			if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ID: "i", ProjectID: "p", Identifier: q.Identifier, RequiredGate: &telemetry.RequiredGate{HumanAction: tc.gate}, PullRequest: &telemetry.PullRequest{HumanQuestionWorkFingerprint: tc.current, State: tc.prState}}}}); err != nil {
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

func TestOperationsHumanDecisionAggregation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 16, 42, 0, 0, time.UTC)
	for _, tc := range []struct {
		name             string
		recorded         []operations.Decision
		snapshot         telemetry.Snapshot
		wantKind         string
		wantQuestion     string
		configureProject bool
		autoPromote      bool
		optoutLabel      string
		allowedLabels    []string
		gateKind         string
		check            func(*testing.T, operations.Decision)
	}{
		{
			name: "human-owned prerequisite",
			snapshot: telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{
					{ID: "dependent", ProjectID: "parable", Identifier: "getparable/parable#3413", Title: "Ship dependent change", State: "Todo", URL: "https://github.com/getparable/parable/issues/3413", BlockedBy: []telemetry.BlockedRef{{ID: "blocker", Identifier: "getparable/parable#3417", HumanOwned: true}}},
					{ID: "blocker", ProjectID: "parable", Identifier: "getparable/parable#3417", Title: "Approve the production rollout", URL: "https://github.com/getparable/parable/issues/3417"},
				},
				SchedulerDecisions: []telemetry.SchedulerDecision{{ProjectID: "parable", IssueID: "dependent", Lane: "Todo", Result: "skipped", Reason: "blocked_by_dependency", DecisionAt: now}},
			},
			wantKind:     "human_prerequisite",
			wantQuestion: "Complete and close the human-owned prerequisite.",
			check: func(t *testing.T, decision operations.Decision) {
				t.Helper()
				if decision.Prerequisite == nil || decision.Prerequisite.Issue != "getparable/parable#3417" || decision.Prerequisite.Title != "Approve the production rollout" || decision.Prerequisite.Evidence != "Completion evidence is required." {
					t.Fatalf("prerequisite decision = %#v", decision)
				}
			},
		},
		{
			name: "project-local prerequisite metadata stays scoped",
			snapshot: telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{
					{ID: "dependent", ProjectID: "alpha", Identifier: "ENG-123", Title: "Ship dependent change", State: "Todo", BlockedBy: []telemetry.BlockedRef{{ID: "alpha-blocker", Identifier: "ENG-456", HumanOwned: true}}},
					{ID: "alpha-blocker", ProjectID: "alpha", Identifier: "ENG-456", Title: "Approve the alpha rollout"},
					{ID: "beta-blocker", ProjectID: "beta", Identifier: "ENG-456", Title: "Unrelated beta prerequisite", URL: "https://beta.example/issues/ENG-456"},
				},
				SchedulerDecisions: []telemetry.SchedulerDecision{{ProjectID: "alpha", IssueID: "dependent", Lane: "Todo", Result: "skipped", Reason: "blocked_by_dependency", DecisionAt: now}},
			},
			wantKind:     "human_prerequisite",
			wantQuestion: "Complete and close the human-owned prerequisite.",
			check: func(t *testing.T, decision operations.Decision) {
				t.Helper()
				if decision.Prerequisite == nil || decision.Prerequisite.Title != "Approve the alpha rollout" || decision.Prerequisite.URL != "/api/v1/projects/alpha/issues/explanation?reference=ENG-456" {
					t.Fatalf("project-scoped prerequisite decision = %#v", decision)
				}
			},
		},
		{
			name: "blocked park with no automatic exit",
			snapshot: telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{{ID: "parked", ProjectID: "detent", Identifier: "digitaldrywood/detent#2395", Title: "Repair the parked delivery", State: "Blocked", URL: "https://github.com/digitaldrywood/detent/issues/2395"}},
				Blocked:     []telemetry.Blocked{{Issue: telemetry.Issue{ID: "parked", ProjectID: "detent", Identifier: "digitaldrywood/detent#2395", Title: "Repair the parked delivery", State: "Blocked", URL: "https://github.com/digitaldrywood/detent/issues/2395"}, Error: "credentials are unavailable", RecoveryAction: "hold", RecoveryReason: "human_blocker", RecoveryRemedy: "restore the credentials"}},
			},
			wantKind:     "blocked_park",
			wantQuestion: "credentials are unavailable — restore the credentials",
		},
		{
			name: "pull request review gate",
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "review", ProjectID: "parable", Identifier: "getparable/parable#3500", Title: "Review the release", State: "Human Review", URL: "https://github.com/getparable/parable/issues/3500", RequiredGate: &telemetry.RequiredGate{HumanAction: "Review pull request #3501."}, PullRequest: &telemetry.PullRequest{Number: 3501, State: "OPEN", URL: "https://github.com/getparable/parable/pull/3501"}},
				{ID: "review-copy", ProjectID: "release", Identifier: "getparable/parable#3500", Title: "Review the release", State: "Human Review", URL: "https://github.com/getparable/parable/issues/3500", RequiredGate: &telemetry.RequiredGate{HumanAction: "Review pull request #3501."}, PullRequest: &telemetry.PullRequest{Number: 3501, State: "OPEN", URL: "https://github.com/getparable/parable/pull/3501"}},
			}},
			wantKind:     "pull_request_review",
			wantQuestion: "Review pull request #3501.",
			check: func(t *testing.T, decision operations.Decision) {
				t.Helper()
				if decision.URL != "https://github.com/getparable/parable/pull/3501" {
					t.Fatalf("review decision URL = %q", decision.URL)
				}
			},
		},
		{
			name:         "closed pull request supersedes published question",
			recorded:     []operations.Decision{{ProjectID: "detent", Issue: "digitaldrywood/detent#2465", Question: "Choose a recovery?", WorkFingerprint: "same"}},
			snapshot:     telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ID: "closed", ProjectID: "detent", Identifier: "digitaldrywood/detent#2465", PullRequest: &telemetry.PullRequest{State: "CLOSED", HumanQuestionWorkFingerprint: "same"}}}},
			wantKind:     "",
			wantQuestion: "",
		},
		{
			name: "duplicate durable questions",
			recorded: []operations.Decision{
				{ProjectID: "alpha", Issue: "owner/repo#99", Question: "Choose a recovery?"},
				{ProjectID: "beta", Issue: "owner/repo#99", Question: "Choose a recovery?"},
			},
			wantKind:     "question",
			wantQuestion: "Choose a recovery?",
		},
		{
			name:     "question freshness evidence stays project scoped",
			recorded: []operations.Decision{{ProjectID: "alpha", Issue: "owner/repo#99", Question: "Choose a recovery?", WorkFingerprint: "alpha-current"}},
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "alpha", ProjectID: "alpha", Identifier: "owner/repo#99", Title: "Alpha question", PullRequest: &telemetry.PullRequest{State: "OPEN", HumanQuestionWorkFingerprint: "alpha-current"}},
				{ID: "beta", ProjectID: "beta", Identifier: "owner/repo#99", Title: "Beta closed copy", PullRequest: &telemetry.PullRequest{State: "CLOSED", HumanQuestionWorkFingerprint: "beta-current"}},
			}},
			wantKind:     "question",
			wantQuestion: "Choose a recovery?",
			check: func(t *testing.T, decision operations.Decision) {
				t.Helper()
				if decision.Title != "Alpha question" {
					t.Fatalf("question title = %q, want alpha project evidence", decision.Title)
				}
			},
		},
		{
			name:             "configured manual pull request review gate",
			configureProject: true,
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "manual-review", ProjectID: "parable", Identifier: "getparable/parable#3502", Title: "Review the release", State: "Human Review", PullRequest: &telemetry.PullRequest{Number: 3503, State: "OPEN", URL: "https://github.com/getparable/parable/pull/3503"}},
			}},
			wantKind:     "pull_request_review",
			wantQuestion: "Review pull request #3503.",
		},
		{
			name:             "issue opt-out requires review",
			configureProject: true,
			autoPromote:      true,
			optoutLabel:      "requires-human-review",
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "optout-review", ProjectID: "parable", Identifier: "getparable/parable#3504", State: "Human Review", Labels: []string{" Requires-Human-Review "}, PullRequest: &telemetry.PullRequest{Number: 3505, State: "OPEN"}},
			}},
			wantKind:     "pull_request_review",
			wantQuestion: "Review pull request #3505.",
		},
		{
			name:             "allowed-label miss requires review",
			configureProject: true,
			autoPromote:      true,
			allowedLabels:    []string{"release"},
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "allowlist-review", ProjectID: "parable", Identifier: "getparable/parable#3506", State: "Human Review", Labels: []string{"docs"}, PullRequest: &telemetry.PullRequest{Number: 3507, State: "OPEN"}},
			}},
			wantKind:     "pull_request_review",
			wantQuestion: "Review pull request #3507.",
		},
		{
			name:             "human gate requires review",
			configureProject: true,
			autoPromote:      true,
			gateKind:         "human_review",
			snapshot: telemetry.Snapshot{BoardIssues: []telemetry.Issue{
				{ID: "human-gate-review", ProjectID: "parable", Identifier: "getparable/parable#3508", State: "Human Review", PullRequest: &telemetry.PullRequest{Number: 3509, State: "OPEN"}},
			}},
			wantKind:     "pull_request_review",
			wantQuestion: "Review pull request #3509.",
		},
		{
			name:             "automated Human Review wait",
			configureProject: true,
			autoPromote:      true,
			snapshot:         telemetry.Snapshot{BoardIssues: []telemetry.Issue{{ID: "auto", ProjectID: "parable", Identifier: "getparable/parable#3502", State: "Human Review", PullRequest: &telemetry.PullRequest{Number: 3503, State: "OPEN"}}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := testDeps(t)
			if tc.configureProject {
				setOperationsTestProject(t, deps.Registry, "parable", tc.autoPromote, tc.optoutLabel, tc.allowedLabels, tc.gateKind)
			}
			deps.Store = operationsStore{Store: openWebTestStore(t), report: operations.Report{Decisions: tc.recorded}}
			if err := deps.Hub.Publish(tc.snapshot); err != nil {
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
			if tc.wantKind == "" {
				if len(report.Decisions) != 0 {
					t.Fatalf("decisions = %#v, want none", report.Decisions)
				}
				return
			}
			if len(report.Decisions) != 1 || report.Decisions[0].Kind != tc.wantKind || report.Decisions[0].Question != tc.wantQuestion {
				t.Fatalf("decisions = %#v, want one %q decision with %q", report.Decisions, tc.wantKind, tc.wantQuestion)
			}
			if tc.check != nil {
				tc.check(t, report.Decisions[0])
			}
		})
	}
}

func setOperationsTestProject(t *testing.T, registry *project.Registry, id string, autoPromote bool, optoutLabel string, allowedLabels []string, gateKind string) {
	t.Helper()
	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerMemory
	workflowCfg.Agent.AutoPromote.Enabled = autoPromote
	if optoutLabel != "" {
		workflowCfg.Agent.AutoPromote.OptoutLabel = optoutLabel
	}
	workflowCfg.Agent.AutoPromote.AllowedIssueLabels = append([]string(nil), allowedLabels...)
	if gateKind != "" {
		workflowCfg.Gate.Kind = gateKind
	}
	trackedProject, err := project.New(project.Config{
		Project:  global.Project{ID: id},
		Workflow: workflowconfig.Workflow{Config: workflowCfg, Prompt: "Work the issue."},
	}, project.Dependencies{Connector: connectorProbe{name: "memory"}})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

func TestOperationsDecisionDedupeKeepsProjectLocalIdentifiers(t *testing.T) {
	t.Parallel()
	deps := testDeps(t)
	deps.Store = operationsStore{Store: openWebTestStore(t)}
	if err := deps.Hub.Publish(telemetry.Snapshot{BoardIssues: []telemetry.Issue{
		{ID: "alpha", ProjectID: "alpha", Identifier: "ENG-123", State: "Human Review", RequiredGate: &telemetry.RequiredGate{HumanAction: "Review alpha."}, PullRequest: &telemetry.PullRequest{Number: 1, State: "OPEN"}},
		{ID: "beta", ProjectID: "beta", Identifier: "ENG-123", State: "Human Review", RequiredGate: &telemetry.RequiredGate{HumanAction: "Review beta."}, PullRequest: &telemetry.PullRequest{Number: 2, State: "OPEN"}},
	}}); err != nil {
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
	if len(report.Decisions) != 2 {
		t.Fatalf("decisions = %#v, want both project-local issues", report.Decisions)
	}
}
