package scenariotest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeOutageProgressBackend struct{}

func (nativeOutageProgressBackend) Decide(_ context.Context, evidence progresscheck.Request) (progresscheck.Decision, error) {
	last := evidence.Attempts[len(evidence.Attempts)-1]
	return progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: last.ID + " records a shared backend protocol outage", Reason: "Shared backend protocol outage", Evidence: []string{last.ID}, DefectKey: "shared-backend-protocol", DefectTitle: "Shared backend protocol outage", DefectBody: "Repair the backend protocol." + issueContractTestSections}, nil
}

func RunNativeRetryProgressSharedOutagePersistsOneDefect(t *testing.T, factories Constructors) {
	newNativeChangeHubTransport := func(t *testing.T, review string, states []tracker.NativeState, memory bool) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, memory, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "Backlog", Transitions: []string{"Todo"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}, true)
	var defectID tracker.NativeWorkItemID
	for index := range 6 {
		issue := h.createInProgress(t, fmt.Sprintf("Backend outage issue %d %s", index, testenv.DistinctText(fmt.Sprint(index), 6)))
		h.claim(t, issue.ID)
		failed := h.scheduler.RunExecution(issue.ID)
		if err := failed.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := failed.Finish(t.Context(), "failed"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
			t.Fatal(err)
		}
		issue = h.claim(t, issue.ID)
		request, owner, err := h.scheduler.RetryProgress(t.Context(), issue)
		if err != nil {
			t.Fatal(err)
		}
		record, checked, err := progresscheck.Check(t.Context(), request, nativeOutageProgressBackend{}, owner)
		if err != nil || !checked || record.DefectIssueID == "" || record.CommentID == "" {
			t.Fatalf("outage progress publication failed: record=%+v checked=%v error=%v", record, checked, err)
		}
		if defectID == "" {
			defectID = tracker.NativeWorkItemID(record.DefectIssueID)
		}
		if record.DefectIssueID != string(defectID) {
			t.Fatalf("outage created another defect: got %s want %s", record.DefectIssueID, defectID)
		}
		defect, err := h.admin.Issue(t.Context(), defectID)
		if err != nil || defect.WorkItemID != defectID || defect.OrganizationID != h.organization || defect.ProjectID != h.project || defect.State != "Backlog" || defect.Title != "Shared backend protocol outage" {
			t.Fatalf("read persisted outage defect %s: issue=%+v error=%v", defectID, defect, err)
		}
		comments, err := h.admin.Comments(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
		if err != nil {
			t.Fatal(err)
		}
		link := fmt.Sprintf("System defect: [#%d](%s)", defect.Number, defect.WebURL)
		if !slices.ContainsFunc(comments.Items, func(comment tracker.NativeComment) bool {
			return comment.ID == record.CommentID && !comment.Deleted && strings.Contains(comment.Body, link)
		}) {
			t.Fatalf("persisted progress comment %s does not link to defect %s", record.CommentID, defectID)
		}
		var persisted corestore.RelationshipPage
		hubclientReadPostgres(t, h.config, h.organization, func(reader tenantstore.Reader) error {
			var err error
			persisted, err = reader.Relationships(t.Context(), h.project, tracker.NativeWorkItemID(issue.ID), corestore.RelationshipCursor{}, 2)
			return err
		})
		if len(persisted.Items) != 0 || persisted.Next != (corestore.RelationshipCursor{}) {
			t.Fatalf("diagnostic outage created work-item relationships: %+v", persisted)
		}
		if state := h.state(t, issue.ID); state != "In Progress" {
			t.Fatalf("diagnostic outage moved work to %q, want In Progress", state)
		}
		if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Review"); err != nil {
			t.Fatal(err)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := h.admin.Issues(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defects := 0
	for _, issue := range page.Items {
		if issue.Title == "Shared backend protocol outage" {
			defects++
		}
	}
	if defects != 1 {
		t.Fatalf("persisted %d shared outage defects, want 1", defects)
	}
}

func RunNativeRetryProgressLoadsRepeatedLandingRefusals(t *testing.T, factories Constructors) {
	newNativeChangeHub := func(t *testing.T) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
			{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
			{Name: "In Progress", Dispatchable: true, Transitions: []string{"Rework", "Done"}},
			{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
			{Name: "In Review", Transitions: []string{"Rework", "Done"}},
			{Name: "Done", Terminal: true},
		}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	t.Parallel()
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, test := range []struct {
		name string
		base string
		head string
		want bool
	}{
		{name: "same source", base: base, head: head, want: true},
		{name: "changed base", base: strings.Repeat("c", 40), head: head},
		{name: "changed head", base: base, head: strings.Repeat("c", 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t)
			issue := h.createInProgress(t, "Retry landing refusal")
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: current.Revision, State: "Rework", Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			claim := func() connector.Issue {
				t.Helper()
				candidates := h.candidatesIn(t, "Rework")
				if len(candidates) != 1 || candidates[0].ID != issue.ID {
					t.Fatalf("Rework candidates = %+v, want %s", candidates, issue.ID)
				}
				claimed, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return claimed.Issue
			}
			var attempts []string
			for _, source := range []struct{ base, head string }{{base, head}, {test.base, test.head}} {
				issue = claim()
				execution := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
				if err := execution.StartLanding(t.Context(), 1, 0); err != nil {
					t.Fatal(err)
				}
				if err := execution.ObserveLanding(t.Context(), runner.NativeLanding{RefusalKind: workspace.LandRefusalBaseMoved, BaseSHA: source.base, HeadSHA: source.head}); err != nil {
					t.Fatal(err)
				}
				if err := execution.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: source.head, ExternalEffect: "none", EffectState: "none"}); err != nil {
					t.Fatal(err)
				}
				if err := execution.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				attempts = append(attempts, execution.CompletionData().AttemptID)
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "waiting"); err != nil {
					t.Fatal(err)
				}
			}
			issue = claim()
			currentClaim := h.scheduler.RunExecution(issue.ID).(*ownedExecution).claim
			for _, attempt := range currentClaim.recovery.Attempts {
				if attempt.AttemptID == attempts[0] {
					t.Fatal("sparse claim recovery unexpectedly retained the preceding refusal")
				}
			}
			client := fixtureClient(currentClaim.source.NativeClient().Transport()).requests
			transport := client.Load()
			reads := 0
			client.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/progress") {
					reads++
				}
				return transport.RoundTrip(request)
			}))
			request, _, err := h.scheduler.RetryProgress(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if reads != 1 || len(request.Attempts) != 2 {
				t.Fatalf("progress history missing: reads=%d attempts=%+v", reads, request.Attempts)
			}
			if request.Attempts[0].ID != attempts[0] || request.Attempts[1].ID != attempts[1] {
				t.Fatalf("progress attempts out of order: %+v", request.Attempts)
			}
			if got := request.NeedsCheck(); got != test.want {
				t.Fatalf("NeedsCheck=%v, want %v", got, test.want)
			}
		})
	}
}

type nativeCurrentClaimRunner struct {
	started chan nativeCompletionRun
	checked bool
}

func (r *nativeCurrentClaimRunner) CheckProgress(context.Context, runner.RunRequest, progresscheck.Request) (progresscheck.Decision, error) {
	r.checked = true
	return progresscheck.Decision{}, errors.New("pre-dispatch progress should not run")
}

func (r *nativeCurrentClaimRunner) Run(ctx context.Context, req runner.RunRequest) (runner.RunResult, error) {
	err := req.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"})
	r.started <- nativeCompletionRun{request: req, err: err}
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}

func RunNativeRetryUsesCurrentClaim(t *testing.T, factories Constructors) {
	t.Parallel()
	h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done", "Todo"}},
		{Name: "Done", Terminal: true},
	}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	issue := h.createInProgress(t, "Retry under the admitted lease")
	h.claim(t, issue.ID)
	prior := h.scheduler.RunExecution(issue.ID)
	if err := prior.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := prior.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	issue = h.claim(t, issue.ID)
	original := h.scheduler.RunExecution(issue.ID)
	backend := &nativeCurrentClaimRunner{started: make(chan nativeCompletionRun, 1)}
	source := &nativeCompletionScheduling{Scheduler: h.scheduler, issue: issue, released: make(chan error, 1)}
	orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: source, Runner: backend, Logger: slog.New(slog.NewTextHandler(t.Output(), nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	select {
	case started := <-backend.started:
		if started.err != nil || started.request.Execution != original || backend.checked {
			t.Fatalf("retry did not start on the admitted execution: check=%t error=%v", backend.checked, started.err)
		}
	case <-testenv.WaitContext(t, ctx).Done():
		t.Fatal("retry did not start")
	}
}
