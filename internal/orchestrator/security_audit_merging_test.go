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

func TestMergingSecurityAuditCandidate(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			o := securityAuditTestOrchestrator(newSecurityAuditMemoryStore())
			o.cfg.AutoPromote.Enabled = true
			o.cfg.AutoPromote.Gate.SecurityAudit.Enabled = enabled
			issue := securityAuditTestIssue()
			issue.State = "Merging"
			state := newState(o.cfg)
			got := o.autoPromoteEvaluationIssues(&state, []connector.Issue{issue}, normalizeAutoPromoteConfig(o.cfg.AutoPromote))
			if (len(got) == 1) != enabled {
				t.Fatalf("candidates = %d, audit enabled = %v", len(got), enabled)
			}
		})
	}
}

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

func TestMergingSecurityAuditVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		verdict      string
		running      bool
		wantState    string
		wantComments int
		wantReason   string
	}{
		{name: "fail", verdict: securityaudit.VerdictFail, wantState: "Rework", wantComments: 1, wantReason: "security_audit_findings"},
		{name: "pass", verdict: securityaudit.VerdictPass, wantState: "Done"},
		{name: "running", running: true, wantReason: "security_audit_wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			memo := newSecurityAuditMemoryStore()
			o.securityAuditStore = memo
			run := securityAuditPassingRun(issue)
			run.Verdict = tc.verdict
			if tc.verdict == securityaudit.VerdictFail {
				run.Findings = []securityaudit.Finding{
					{ID: "authz", Severity: "p1", Path: "internal/database/webhook.go", Line: 342, Body: "authorization bypass"},
					{ID: "payment", Severity: "p2", Path: "templates/pages/payment_settings.templ", Line: 340, Body: "payment input is unsafe"},
				}
			}
			if tc.running {
				o.securityAuditRuns[o.securityAuditIdentity(issue).cacheKey] = struct{}{}
			} else {
				if _, err := memo.RecordSecurityAuditRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			state := newState(o.cfg)
			now := time.Now()
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
			running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge, StartedAt: now.Add(-time.Minute)}
			if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
				t.Fatal("merge completion was not handled")
			}
			if tc.running {
				if retry := state.Retry[issue.ID]; retry.Error != tc.wantReason {
					t.Fatalf("retry = %+v", retry)
				}
			} else if _, retry := state.Retry[issue.ID]; retry {
				t.Fatal("completed audit scheduled another merge attempt")
			}
			if tc.wantState == "" {
				if len(tracker.updates) != 0 {
					t.Fatalf("updates = %+v", tracker.updates)
				}
			} else if len(tracker.updates) != 1 || tracker.updates[0].state != tc.wantState {
				t.Fatalf("updates = %+v, want %s", tracker.updates, tc.wantState)
			}
			if len(tracker.prComments) != tc.wantComments {
				t.Fatalf("PR comments = %+v", tracker.prComments)
			}
			if tc.wantComments > 0 {
				if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0].body, "authorization bypass") {
					t.Fatalf("issue findings handoff = %+v", tracker.comments)
				}
				if got := state.PriorAttempts[issue.ID].Reason; got != tc.wantReason {
					t.Fatalf("Rework reason = %q, want %q", got, tc.wantReason)
				}
				for _, fragment := range []string{"p1", "p2", "internal/database/webhook.go:342", "templates/pages/payment_settings.templ:340", "authorization bypass", "payment input is unsafe"} {
					if !strings.Contains(tracker.prComments[0].body, fragment) {
						t.Errorf("comment missing %q", fragment)
					}
				}
				// A repeated stale tracker snapshot and a restarted orchestrator must reuse the PR comment.
				o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
				if len(tracker.prComments) != 1 || len(tracker.updates) != 1 {
					t.Fatal("duplicate findings comment or lane transition")
				}
			}
			if got := len(tracker.merges); (got == 1) != (tc.verdict == securityaudit.VerdictPass) {
				t.Fatalf("merges = %d", got)
			}
		})
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

func TestMergingSecurityAuditCompletesAfterWorkerWait(t *testing.T) {
	t.Parallel()
	o, tracker, issue := mergingSecurityAuditFixture()
	auditor := &mergingSecurityAuditor{release: make(chan struct{})}
	o.securityAuditor = auditor
	state := newState(o.cfg)
	now := time.Now()
	event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
	running := Running{Issue: issue, Attempt: 1, Mode: runpkg.RunModeMerge, StartedAt: now.Add(-time.Minute)}
	t.Cleanup(func() { close(auditor.release); o.securityAuditWG.Wait() })
	if !o.completeProgrammaticMergeWorkerResult(t.Context(), &state, event, running, issue) {
		t.Fatal("merge completion was not handled")
	}
	if retry := state.Retry[issue.ID]; retry.Error != "security_audit_wait" {
		t.Fatalf("retry = %+v", retry)
	}
	o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
	if len(tracker.updates) != 0 || len(tracker.prComments) != 0 {
		t.Fatal("running audit published or transitioned")
	}
	auditor.release <- struct{}{}
	o.securityAuditWG.Wait()
	o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Rework" || len(tracker.prComments) != 1 {
		t.Fatalf("updates = %+v, comments = %+v", tracker.updates, tracker.prComments)
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
func (c *mergingSecurityAuditConnector) SecurityAuditSnapshot(_ context.Context, issue connector.Issue, _ int) (securityaudit.Snapshot, error) {
	return securityAuditSnapshotFromIssue("detent", issue), nil
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
		{name: "metered authentication", mutate: func(r *securityaudit.Run) { r.AuthenticationMode = "api_key" }},
		{name: "untrusted evidence", mutate: func(r *securityaudit.Run) { r.ServiceIdentity = "untrusted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, tracker, issue := mergingSecurityAuditFixture()
			memo := newSecurityAuditMemoryStore()
			o.securityAuditStore = memo
			audit := securityAuditPassingRun(issue)
			audit.Attempt = gate.DefaultSecurityAuditMaxAttempts
			tc.mutate(&audit)
			if _, err := memo.RecordSecurityAuditRun(t.Context(), audit); err != nil {
				t.Fatal(err)
			}
			tracker.publishErr = errors.New("PR comment unavailable")
			state := newState(o.cfg)
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: time.Now(), Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean, TurnStarted: true}}
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
		})
	}
}
