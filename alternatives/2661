package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

// Audit rows are completion records. A live stage can legitimately be waiting
// with no row, as in session 12329 in the #2716 incident.
func TestSecurityAuditLifecycleAcrossRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		previousHead bool
		restart      bool
		completed    bool
	}{
		{name: "no row"},
		{name: "completed different head", previousHead: true},
		{name: "restart without persisted result", restart: true},
		{name: "restart after completion", restart: true, completed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, issue := mergingSecurityAuditFixture()
			memo := newSecurityAuditMemoryStore()
			o.securityAuditStore = memo
			if tc.previousHead || tc.completed {
				run := securityAuditPassingRun(issue)
				if tc.previousHead {
					run.HeadSHA = "previous-head"
				}
				if _, err := memo.RecordSecurityAuditRun(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			if tc.restart {
				// The old process's launch registry is not durable and cannot be restored
				// into the replacement orchestrator, with or without a completed result.
				o.securityAuditRuns[o.securityAuditIdentity(issue).cacheKey] = struct{}{}
				replacement, _, _ := mergingSecurityAuditFixture()
				replacement.securityAuditStore = memo
				o = replacement
			}
			evaluation := o.securityAuditEvaluation(t.Context(), issue)
			if tc.completed {
				if !evaluation.Allowed || evaluation.Reason != securityaudit.ReasonReady {
					t.Fatalf("persisted result after restart = %+v", evaluation)
				}
				return
			}
			if evaluation.Running || evaluation.Reason != securityaudit.ReasonMissing {
				t.Fatalf("evaluation before launch = %+v", evaluation)
			}
			auditor := &mergingSecurityAuditor{release: make(chan struct{})}
			o.securityAuditor = auditor
			t.Cleanup(func() { close(auditor.release); o.securityAuditWG.Wait() })
			state := newState(o.cfg)
			now := time.Date(2026, 9, 14, 23, 49, 18, 0, time.UTC)
			o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
			evaluation = o.securityAuditEvaluation(t.Context(), issue)
			decision, pending := gate.EvaluateSecurityAudit(o.cfg.AutoPromote.Gate.SecurityAudit, evaluation)
			if !pending || !evaluation.Running || decision.Reason != gate.ReasonSecurityAuditWait {
				t.Fatalf("auto-promote did not start missing audit: evaluation=%+v decision=%+v", evaluation, decision)
			}
			// Repeated promotion while the reviewer is held must keep waiting.
			o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))
			if got := o.securityAuditEvaluation(t.Context(), issue); !got.Running {
				t.Fatalf("active audit = %+v", got)
			}
		})
	}
}
