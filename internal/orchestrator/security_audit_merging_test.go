package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestRunningSecurityAuditGate(t *testing.T) {
	t.Parallel()
	o := securityAuditTestOrchestrator(newSecurityAuditMemoryStore())
	issue := securityAuditTestIssue()
	o.securityAuditRuns[o.securityAuditIdentity(issue).cacheKey] = struct{}{}
	evaluation := o.securityAuditEvaluation(t.Context(), issue)
	got := gate.Evaluate(o.cfg.AutoPromote.Gate, nil, gate.Summary{PullRequestPresent: true, CIStatus: "success", SecurityAudit: evaluation}, time.Now(), gate.EvaluationOptions{})
	if got.Action != gate.ActionWait || string(got.Reason) != "security_audit_wait" {
		t.Fatalf("decision = %+v", got)
	}
}

type mergingSecurityAuditConnector struct {
	*autoPromoteTickMergeConnector
	readErr    error
	publishErr error
}

func (c *mergingSecurityAuditConnector) FetchPullRequestComments(context.Context, string, int) ([]connector.IssueComment, error) {
	var comments []connector.IssueComment
	for _, comment := range c.prComments {
		comments = append(comments, connector.IssueComment{Body: comment.body})
	}
	return comments, c.readErr
}
func (c *mergingSecurityAuditConnector) CreatePullRequestComment(ctx context.Context, repository string, number int, body string) error {
	if c.publishErr != nil {
		return c.publishErr
	}
	return c.autoPromoteTickConnector.CreatePullRequestComment(ctx, repository, number, body)
}
func mergingSecurityAuditFixture() (*Orchestrator, *mergingSecurityAuditConnector, connector.Issue) {
	o := securityAuditTestOrchestrator(newSecurityAuditMemoryStore())
	o.cfg.AutoPromote.Enabled = true
	o.cfg.AutoPromote.Gate.SecurityAudit.BlockOn = []string{"p1", "p2"}
	o.cfg.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	o.cfg.TerminalStates = []string{"Done", "Cancelled"}
	o.cfg = normalizeConfig(o.cfg)
	issue := securityAuditTestIssue()
	issue.State = "Merging"
	issue.PullRequest.State = "OPEN"
	issue.PullRequest.CIStatus = "success"
	issue.PullRequest.MergeableState = "clean"
	issue.PullRequest.URL = "https://github.test/digitaldrywood/detent/pull/2006"
	tracker := &mergingSecurityAuditConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}}
	o.connector = tracker
	return o, tracker, issue
}

func TestSecurityAuditPublicationRetry(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"read", "publish", "transition"} {
		t.Run(failure, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			run := securityAuditPassingRun(issue)
			run.Verdict = securityaudit.VerdictFail
			run.Findings = []securityaudit.Finding{{ID: "authz", Severity: "p1", Body: "authorization bypass"}}
			if _, err := o.securityAuditStore.RecordSecurityAuditRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			err := errors.New("temporary connector failure")
			switch failure {
			case "read":
				tracker.readErr = err
			case "publish":
				tracker.publishErr = err
			case "transition":
				tracker.updateErr = err
			}
			state := newState(o.cfg)
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: time.Now(), Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
			running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge}
			o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue)
			if tracker.stateIssues[0].State != "Merging" {
				t.Fatalf("transition before publication: %+v", tracker.updates)
			}
			tracker.updates = nil
			tracker.readErr, tracker.publishErr, tracker.updateErr = nil, nil, nil
			// No in-memory publication cache survives this reset.
			state = newState(o.cfg)
			o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue)
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" || len(tracker.prComments) != 1 {
				t.Fatalf("updates = %+v, comments = %+v", tracker.updates, tracker.prComments)
			}
		})
	}
}

type mergingSecurityAuditor struct{ release chan struct{} }

func (a *mergingSecurityAuditor) Audit(ctx context.Context, request SecurityAuditRequest) (SecurityAuditExecution, error) {
	select {
	case <-a.release:
	case <-ctx.Done():
		return SecurityAuditExecution{}, ctx.Err()
	}
	execution, err := (&securityAuditTestAuditor{}).Audit(ctx, request)
	execution.Result.Verdict = securityaudit.VerdictFail
	execution.Result.Findings = []securityaudit.Finding{{ID: "authz", Severity: "p1", Path: "internal/database/webhook.go", Line: 342, Body: "authorization bypass"}}
	return execution, err
}
func (c *mergingSecurityAuditConnector) PullRequestValidationDiff(_ context.Context, issue connector.Issue) (connector.ValidationDiff, error) {
	snapshot := securityAuditSnapshotFromIssue("detent", issue)
	return connector.ValidationDiff{Repository: snapshot.Repository, PRNumber: snapshot.PRNumber, BaseSHA: snapshot.BaseSHA, HeadSHA: snapshot.HeadSHA, Patch: "diff"}, nil
}

func TestMergeWorkerActionableGateControls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		ci         string
		threads    []connector.PullRequestReviewThread
		wantReason string
	}{
		{name: "failed checks", ci: "failure", wantReason: mergeWorkerFastPathNotReadyReason},
		{name: "unresolved threads", ci: "success", threads: []connector.PullRequestReviewThread{{Path: "merge.go", Line: 10}}, wantReason: string(AutoPromoteReasonUnresolvedReviewThreads)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			o.cfg.AutoPromote.Gate.SecurityAudit.Enabled = false
			o.cfg.AutoPromote.Gate.Kind = gate.KindCommand
			issue.PullRequest.CIStatus = tc.ci
			issue.PullRequest.UnresolvedReviewThreads = tc.threads
			tracker.stateIssues = []connector.Issue{issue}
			state := newState(o.cfg)
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: time.Now(), Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
			running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge}
			if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
				t.Fatal("completion was not handled")
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" || len(tracker.merges) != 0 {
				t.Fatalf("updates = %+v, merges = %+v", tracker.updates, tracker.merges)
			}
			if _, retry := state.Retry[issue.ID]; retry {
				t.Fatal("Rework scheduled a merge retry")
			}
			if state.PriorAttempts[issue.ID].Reason != tc.wantReason {
				t.Fatalf("reason = %q", state.PriorAttempts[issue.ID].Reason)
			}
		})
	}
}

func TestMergeWorkerAuditFailureDoesNotPublishFindings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*securityaudit.Run)
	}{
		{name: "backend failure", mutate: func(r *securityaudit.Run) { r.ExitStatus = securityaudit.ExitStatusFailed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			memo := newSecurityAuditMemoryStore()
			o.securityAuditStore = memo
			audit := securityAuditPassingRun(issue)
			audit.Attempt = gate.DefaultSecurityAuditMaxAttempts
			now := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
			o.now = func() time.Time { return now }
			audit.CompletedAt = now
			tc.mutate(&audit)
			if _, err := memo.RecordSecurityAuditRun(t.Context(), audit); err != nil {
				t.Fatal(err)
			}
			tracker.publishErr = errors.New("PR comment unavailable")
			state := newState(o.cfg)
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
			running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge}
			if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
				t.Fatal("completion was not handled")
			}
			if len(tracker.prComments) != 0 || len(tracker.comments) != 0 || len(tracker.updates) != 0 || len(tracker.merges) != 0 {
				t.Fatalf("audit infrastructure failure published findings or changed issue: %+v", tracker)
			}
			if got := state.Retry[issue.ID].Error; got != string(gate.ReasonSecurityAuditFailed) {
				t.Fatalf("retry reason = %q, want %q", got, gate.ReasonSecurityAuditFailed)
			}
			now = now.Add(o.retryDelay(audit.Attempt, false))
			o.securityAuditor = &securityAuditTestAuditor{}
			event.CompletedAt = now
			if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
				t.Fatal("recovered completion was not handled")
			}
			o.securityAuditWG.Wait()
			if got := o.securityAuditEvaluation(t.Context(), issue); !got.Allowed {
				t.Fatalf("recovered backend did not produce a passing audit: %+v", got)
			}
			latest, err := memo.LatestSecurityAuditRun(t.Context(), o.securityAuditIdentity(issue).key)
			if err != nil || latest.Attempt != audit.Attempt+1 || latest.BaseSHA != audit.BaseSHA || latest.HeadSHA != audit.HeadSHA {
				t.Fatalf("recovery audit = %+v, err = %v", latest, err)
			}
			if len(tracker.merges) == 0 {
				o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue)
			}
			if len(tracker.merges) != 1 || len(tracker.updates) != 1 || tracker.updates[0].state != "Done" || len(tracker.comments) != 0 || len(tracker.prComments) != 0 {
				t.Fatalf("recovered audit did not merge without findings or Rework: %+v", tracker)
			}
		})
	}
}

func TestSecurityAuditAdvisoryPublication(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, status, verdict string
		publishErr            bool
	}{
		{"advisory", "unresolved", securityaudit.VerdictFail, false},
		{"resolved", "resolved", securityaudit.VerdictPass, false},
		{"publication failure", "unresolved", securityaudit.VerdictFail, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			memo := newSecurityAuditMemoryStore()
			o.securityAuditStore = memo
			run := securityAuditPassingRun(issue)
			run.Verdict = tt.verdict
			run.Findings = []securityaudit.Finding{{ID: "advisory", Severity: "p3", Status: tt.status, Body: "advisory detail", Path: "file.go"}}
			if _, err := memo.RecordSecurityAuditRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			if tt.publishErr {
				tracker.publishErr = errors.New("publication unavailable")
			}
			state := newState(o.cfg)
			now := time.Now()
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
			running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge, StartedAt: now.Add(-time.Minute)}
			if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
				t.Fatal("completion not handled")
			}
			if tt.publishErr {
				if len(tracker.merges) != 0 {
					t.Fatal("merged without publication")
				}
				return
			}
			if len(tracker.merges) != 1 || len(tracker.prComments) != 1 || !strings.Contains(tracker.prComments[0].body, "advisory detail") || !strings.Contains(tracker.prComments[0].body, tt.status) {
				t.Fatalf("merges=%v comments=%v", tracker.merges, tracker.prComments)
			}
		})
	}
}

// This catches the recorded promotion sequence repeating current-head hydration
// and review-thread reads for Merging, although only its audit verdict is used.
