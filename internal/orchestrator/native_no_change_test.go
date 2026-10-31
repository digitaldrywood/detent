package orchestrator

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNoChangeEvidenceRetainsRetryAccounting(t *testing.T) {
	valid := "```detent-status\nschema: 1\nstatus: no_change_needed\nno_change:\n  verified_sha: " + strings.Repeat("a", 40) + "\n  evidence: focused regression passed\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name               string
		message            string
		err                error
		wantProductFailure bool
		evidence           *runpkg.NativeNoChange
		wantDone           bool
		command            string
		wantFeedback       string
		reverify           bool
	}{
		{name: "pending independent receipts", message: valid},
		{name: "missing command receipt", message: valid, command: "check-source", evidence: noChangeTestEvidence("pass", "pass", "pass")},
		{name: "command and independent receipts complete", message: valid, command: "check-source", evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Validation = &gate.CommandResult{Command: "check-source", HeadSHA: n.VerifiedSHA}
			return n
		}(), wantDone: true},

		{name: "accepted evidence completes without Change", message: valid, evidence: noChangeTestEvidence("pass", "pass", "pass"), wantDone: true},
		{name: "review rejection", message: valid, evidence: noChangeTestEvidence("rework", "pass", "pass"), wantFeedback: "review rejected"},
		{name: "validator rejection", message: valid, evidence: noChangeTestEvidence("pass", "rework", "pass"), wantFeedback: "validator rejected"},
		{name: "missing audit receipt", message: valid, evidence: noChangeTestEvidence("pass", "pass", "")},
		{name: "missing review receipt", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Review = nil
			return n
		}()},
		{name: "missing validator receipt", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Validator = nil
			return n
		}()},
		{name: "audit rejection", message: valid, evidence: noChangeTestEvidence("pass", "pass", "rework")},
		{name: "review receipt for older source", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Review.HeadSHA = strings.Repeat("b", 40)
			return n
		}()},
		{name: "validator receipt for older source", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Validator.BaseSHA = strings.Repeat("b", 40)
			return n
		}()},
		{name: "audit receipt for older source", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.Audit.Run.HeadSHA = strings.Repeat("b", 40)
			return n
		}()},

		{name: "stale base receipts", message: valid, evidence: func() *runpkg.NativeNoChange {
			n := noChangeTestEvidence("pass", "pass", "pass")
			n.CurrentBase = false
			return n
		}()},
		{name: "stale source requires verification", message: valid, err: errors.New("completion base advanced; re-verify the criteria on the current base"), reverify: true},
		{name: "invalid report retains existing classification", message: strings.Replace(valid, "focused regression passed", "", 1), wantProductFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}})
			cfg.AutoPromote.HumanReview = new(false)
			cfg.AutoPromote.Gate.Run = test.command
			issue := completionTransitionIssue("In Progress", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			conn := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: []connector.WorkflowState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true},
			}}
			attempts := &recordingWorkAttemptStore{}
			o := &Orchestrator{cfg: cfg, connector: conn, workAttempts: attempts}
			state := newState(cfg)
			now := time.Now()
			running := Running{Issue: issue, Attempt: 8, Mode: runpkg.RunModeImplement, TurnCount: 1, WorkAttemptID: 1, DiffStats: DiffStats{Status: "clean"}}
			running.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
			running.Policy.Gates.Validator = true
			running.Policy.Gates.SecurityAudit = true
			event := runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Err: test.err, Result: runpkg.RunResult{FinalMessage: test.message, FinalState: FinalStateCompleted, NativeNoChange: test.evidence}}
			if !o.completeNativeChangeRun(t.Context(), &state, event, running, FinalStateCompleted) {
				t.Fatal("native completion was not handled")
			}
			if test.wantDone {
				if len(tick.updates) != 1 || tick.updates[0].state != "Done" || len(state.Completed) != 1 || len(state.Retry) != 0 {
					t.Fatalf("accepted no-change did not complete: updates=%+v completed=%+v retry=%+v", tick.updates, state.Completed, state.Retry)
				}
				return
			}
			retry, exists := state.Retry[issue.ID]
			if !exists || retry.Attempt != 9 || len(state.Completed) != 0 || retry.Issue.State != "Todo" {
				t.Fatalf("evidence bypassed gates or reset retries: retry=%+v completed=%+v", retry, state.Completed)
			}
			if test.wantFeedback != "" && !strings.Contains(retry.Error, test.wantFeedback) {
				t.Fatalf("lost rejection feedback: %s", retry.Error)
			}
			if !test.wantProductFailure {
				for _, reason := range conn.reasonCodes {
					if reason == terminalAttemptWithoutWorkProductReason {
						t.Fatal("typed evidence was classified as absent work product")
					}
				}
			}
			if test.err != nil {
				if !strings.Contains(retry.Error, "re-verify") {
					t.Fatalf("lost source verification feedback: %s", retry.Error)
				}
			} else if strings.Contains(retry.Error, noWorkProductMessage) != test.wantProductFailure {
				t.Fatalf("incorrect work-product classification: %s", retry.Error)
			}
			if test.reverify {
				current := strings.Repeat("b", 40)
				evidence := noChangeTestEvidence("pass", "pass", "pass")
				evidence.VerifiedSHA = current
				evidence.Review.BaseSHA, evidence.Review.HeadSHA = current, current
				evidence.Validator.BaseSHA, evidence.Validator.HeadSHA = current, current
				evidence.Audit.Run.BaseSHA, evidence.Audit.Run.HeadSHA = current, current
				running.Issue, running.Attempt = retry.Issue, retry.Attempt
				running.WorkAttemptID = 2
				event.Err = nil
				event.Result.FinalMessage = strings.ReplaceAll(valid, strings.Repeat("a", 40), current)
				event.Result.NativeNoChange = evidence
				if !o.completeNativeChangeRun(t.Context(), &state, event, running, FinalStateCompleted) {
					t.Fatal("current-base completion was not handled")
				}
				if len(tick.updates) != 2 || tick.updates[1].state != "Done" || len(state.Completed) != 1 || len(state.Retry) != 0 {
					t.Fatalf("current-base re-verification did not complete: updates=%+v completed=%+v retry=%+v", tick.updates, state.Completed, state.Retry)
				}
			}
		})
	}
}

func noChangeTestEvidence(review, validator, audit string) *runpkg.NativeNoChange {
	sha := strings.Repeat("a", 40)
	result := func(verdict, summary string) *gate.ValidatorResult {
		return &gate.ValidatorResult{BaseSHA: sha, HeadSHA: sha, Submitted: true, Verdict: verdict, Summary: summary}
	}
	n := &runpkg.NativeNoChange{VerifiedSHA: sha, CurrentBase: true, Review: result(review, "review rejected"), Validator: result(validator, "validator rejected")}
	if audit != "" {
		n.Audit = &tracker.SecurityAuditReview{Verdict: audit, Run: securityaudit.Run{BaseSHA: sha, HeadSHA: sha}}
	}
	return n
}
