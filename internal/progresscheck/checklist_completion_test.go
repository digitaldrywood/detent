package progresscheck

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReviewedChecklistCompletion(t *testing.T) {
	const body = "issue contract"
	for _, tc := range []struct {
		name               string
		manual, automatic  bool
		disclosure         string
		staleBody, staleCI bool
		preparing, withCI  bool
		want               tracker.ExecutionCheckStatus
	}{
		{name: "current independent receipt", automatic: true, want: tracker.CheckPassed},
		{name: "current authorized human review", manual: true, want: tracker.CheckManual},
		{name: "summary alone cannot supply proof"},
		{name: "normalized approved disclosure without embedded policy", automatic: true, disclosure: "approved", want: tracker.CheckDisclosed},
		{name: "unapproved disclosure cannot supply proof", automatic: true, disclosure: "changes_requested"},
		{name: "issue amendment invalidates prior approval", manual: true, staleBody: true},
		{name: "exact required status check", automatic: true, withCI: true, want: tracker.CheckPassed},
		{name: "stale status check invalidates prior pass", automatic: true, withCI: true, staleCI: true},
		{name: "existing GitHub owner owns pending check before landing", automatic: true, withCI: true, staleCI: true, preparing: true, want: tracker.CheckPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := tracker.ChangeVersion{ID: "version", ChangeVersionInput: tracker.ChangeVersionInput{
				AttemptID: "attempt", Repository: "repo", HeadSHA: "head", BaseSHA: "base", PolicyID: "policy",
			}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: v.ID},
				Summary: tracker.ChangeSummary{Status: "reviewed", NativeReview: "approved"}}
			list := &tracker.ExecutionChecklist{AttemptID: v.AttemptID, PolicyID: v.PolicyID, Revision: ChecklistRevision(body, v.PolicyID),
				Checks: []tracker.ExecutionCheck{{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: "criterion", Status: tracker.CheckNeedsReview}}}
			if tc.manual {
				detail.Reviews = append(detail.Reviews, tracker.ChangeReview{ID: "approval", Actor: tracker.Actor{Kind: "human"},
					VersionID: v.ID, Decision: "approved"})
			}
			if tc.automatic {
				result := checklistValidator(v.ID, v.Repository, v.BaseSHA, v.HeadSHA)
				decision := "approved"
				if tc.disclosure != "" {
					decision = tc.disclosure
					result.CriteriaEvidence = nil
					result.NotVerified = []string{"criterion"}
				}
				detail.Reviews = append(detail.Reviews, tracker.ChangeReview{ID: "review", Actor: tracker.Actor{Kind: "runner"},
					VersionID: v.ID, Decision: decision, Validator: result})
			}
			var ci *tracker.NativeLandingCIReceipt
			if tc.withCI {
				v.Policy.Gates.GitHubPullRequest = true
				ci = &tracker.NativeLandingCIReceipt{HeadSHA: v.HeadSHA, PullRequest: 17, State: "success", RequiredChecks: []string{"tests"}}
				list.Checks = append(list.Checks, tracker.ExecutionCheck{Ordinal: 2, Kind: tracker.CheckStatusGate, Text: "tests",
					Status: tracker.CheckPassed, Head: v.HeadSHA, Tree: "tree", Receipt: "old-receipt"})
				if tc.staleCI {
					ci.HeadSHA = "different-head"
				}
			}
			currentBody := body
			if tc.staleBody {
				currentBody += " edited"
			}
			changes, ready := ReviewedChecklist(list, currentBody, detail, v, ci, tc.preparing)
			if ready != (tc.want != "") {
				t.Fatalf("ready = %v, changes = %+v", ready, changes)
			}
			if ready && (len(changes) == 0 || changes[0].Status != tc.want || changes[0].Head != v.HeadSHA) {
				t.Fatalf("checklist changes = %+v, want %s on exact head", changes, tc.want)
			}
		})
	}
}

func TestNoChangeChecklistCompletion(t *testing.T) {
	n := tracker.NativeNoChangeCompletion{VerifiedSHA: "base", Repository: "repo", PolicyID: "policy"}
	for _, tc := range []struct {
		name                  string
		stale, missingCommand bool
		disclose, allowed     bool
		want                  tracker.ExecutionCheckStatus
	}{
		{name: "independent evidence on current base without a project command", missingCommand: true, want: tracker.CheckPassed},
		{name: "independent evidence and required source gate", want: tracker.CheckPassed},
		{name: "review on an older base", stale: true},
		{name: "authorized disclosure", disclose: true, allowed: true, want: tracker.CheckDisclosed},
		{name: "disclosure against policy", disclose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := &tracker.ExecutionChecklist{PolicyID: n.PolicyID,
				Checks: []tracker.ExecutionCheck{{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: "criterion", Status: tracker.CheckNeedsReview}}}
			result := checklistValidator(n.VersionID(), n.Repository, n.VerifiedSHA, n.VerifiedSHA)
			cfg := gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}
			if !tc.missingCommand {
				cfg.Run = "make check"
				result.Commands = append(result.Commands, gate.CommandResult{Command: cfg.Run, HeadSHA: n.VerifiedSHA, TreeSHA: "tree", ReceiptID: "gate"})
				list.Checks = append(list.Checks, tracker.ExecutionCheck{Ordinal: 2, Kind: tracker.CheckProjectGate,
					CheckDigest: policy.Digest([]byte(cfg.Run)), Status: tracker.CheckUnverified})
			}
			if tc.stale {
				result.HeadSHA = "different-base"
			}
			if tc.disclose {
				result.CriteriaEvidence = nil
				result.NotVerified = []string{"criterion"}
			}
			if tc.allowed {
				cfg.Validator.UnverifiedCriteria = gate.UnverifiedCriteriaDisclose
			}
			step := tracker.NativeStepReceipt{Step: "validator", PolicyID: n.PolicyID, Source: tracker.NativeCheckpoint{HeadSHA: n.VerifiedSHA},
				Change: tracker.NativeChangeReference{VersionID: n.VersionID(), HeadSHA: n.VerifiedSHA}, Validator: result}
			changes, ready := NoChangeStepChecklist(list, n, []tracker.NativeStepReceipt{step}, cfg)
			if ready != (tc.want != "") || ready && !slices.ContainsFunc(changes, func(change tracker.ExecutionCheckUpdate) bool {
				return change.Ordinal == 1 && change.Status == tc.want && change.Head == n.VerifiedSHA
			}) {
				t.Fatalf("ready = %v, changes = %+v", ready, changes)
			}
			if tc.missingCommand {
				head, generic := RunChecklistEvidence(nil, tracker.NativeRunData{Steps: []tracker.NativeStepReceipt{step}})
				updates := ChecklistChanges(list.Checks, RoundVerdict{RoundInput: head, PolicyID: n.PolicyID}, generic, []tracker.NativeStepReceipt{step})
				if generic != nil || len(updates) != 1 || updates[0].Status != tracker.CheckPassed {
					t.Fatalf("live independent updates = %+v, project gate = %+v", updates, generic)
				}
			}
		})
	}
}

func checklistValidator(version, repo, base, head string) *gate.ValidatorResult {
	return &gate.ValidatorResult{VersionID: version, Repository: repo, BaseSHA: base, HeadSHA: head, Submitted: true, SessionID: 1, Verdict: gate.ValidatorVerdictPass,
		Commands: []gate.CommandResult{{Command: "inspect source", HeadSHA: head, TreeSHA: "tree", ReceiptID: "inspection"}},
		CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "criterion", Kind: gate.EvidenceReceipt, Reference: "source inspection", Behavior: "independent behavior",
			HeadSHA: head, TreeSHA: "tree", ReceiptIDs: []string{"inspection"}}}}
}

func TestChecklistReviewRejectionThenApproval(t *testing.T) {
	const body = "## Acceptance criteria\n\n- criterion"
	v := tracker.ChangeVersion{ID: "version", ChangeVersionInput: tracker.ChangeVersionInput{
		AttemptID: "attempt", Repository: "example", HeadSHA: "head", BaseSHA: "base", PolicyID: "policy",
	}}
	for _, test := range []struct {
		name string
		exit int
		red  bool
	}{
		{name: "genuine review rejection then approval", exit: 1, red: true},
		{name: "instance failure then approval", exit: gate.TemporaryFailureExitCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			list := &tracker.ExecutionChecklist{AttemptID: v.AttemptID, Revision: ChecklistRevision(body, v.PolicyID), PolicyID: v.PolicyID,
				Checks: []tracker.ExecutionCheck{
					{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: "criterion", Status: tracker.CheckNeedsReview},
					{Ordinal: 2, Kind: tracker.CheckIndependentReview, Status: tracker.CheckUnverified},
				}}
			rejected := checklistValidator(v.ID, v.Repository, v.BaseSHA, v.HeadSHA)
			rejected.Verdict = gate.ValidatorVerdictRework
			rejected.Commands[0].ExitCode = test.exit
			gate.NormalizeAttribution(rejected)
			rejected.CriteriaEvidence[0].Kind = gate.EvidenceNotVerified
			verdict := RoundVerdict{PolicyID: v.PolicyID, RoundInput: RoundInput{HeadSHA: v.HeadSHA, TreeSHA: "tree"}}
			step := tracker.NativeStepReceipt{Step: "validator", PolicyID: v.PolicyID, Source: tracker.NativeCheckpoint{HeadSHA: v.HeadSHA},
				Change: tracker.NativeChangeReference{VersionID: v.ID, HeadSHA: v.HeadSHA}, Validator: rejected}
			changes := ChecklistChanges(list.Checks, verdict, nil, []tracker.NativeStepReceipt{step})
			ApplyChecklistChanges(list.Checks, changes)
			if test.red {
				for _, check := range list.Checks {
					if check.Status != tracker.CheckFailed {
						t.Errorf("genuine rejection projected %+v, want failed", check)
					}
				}
			} else if len(changes) != 0 {
				t.Errorf("instance failure changed the checklist: %+v", changes)
			}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: v.ID}, Summary: tracker.ChangeSummary{Status: "reviewed"},
				Reviews: []tracker.ChangeReview{{VersionID: v.ID, Actor: tracker.Actor{Kind: "runner"}, Decision: "changes_requested", Validator: rejected}}}
			if _, ready := ReviewedChecklist(list, body, detail, v, nil, false); ready {
				t.Fatal("completion accepted the current rejection")
			}
			approved := checklistValidator(v.ID, v.Repository, v.BaseSHA, v.HeadSHA)
			detail.Reviews = append(detail.Reviews, tracker.ChangeReview{VersionID: v.ID, Actor: tracker.Actor{Kind: "runner"}, Decision: "approved", Validator: approved})
			changes, ready := ReviewedChecklist(list, body, detail, v, nil, false)
			if !ready || len(changes) != 2 || changes[0].Status != tracker.CheckPassed || changes[1].Status != tracker.CheckPassed {
				t.Fatalf("approved re-review = %v, updates = %+v, want both rows passed", ready, changes)
			}
		})
	}
}

func TestChecklistPolicyRowsFollowCurrentEvidence(t *testing.T) {
	const head, tree = "head", "tree"
	for _, kind := range []tracker.ExecutionCheckKind{tracker.CheckSecurityAudit, tracker.CheckStatusGate} {
		t.Run(string(kind), func(t *testing.T) {
			list := &tracker.ExecutionChecklist{Checks: []tracker.ExecutionCheck{{Ordinal: 1, Kind: kind, Text: "required", Head: head, Tree: tree, Status: tracker.CheckFlaky}}}
			audit := &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{Attribution: "instance"}}
			evidence := checklistEvidence{head: head, tree: tree}
			if kind == tracker.CheckSecurityAudit {
				evidence.audit = &tracker.NativeStepReceipt{SecurityAudit: audit}
				if changes, ready := evaluateChecklist(list, evidence, false); ready || len(changes) != 0 {
					t.Errorf("instance failure: ready = %v, updates = %+v", ready, changes)
				}
				audit.Run.Attribution = "change"
			} else {
				evidence.ci = &tracker.NativeLandingCIReceipt{HeadSHA: head, PullRequest: 1, RequiredChecks: []string{"required"}, FailedChecks: []string{"required"}}
			}
			changes, ready := evaluateChecklist(list, evidence, false)
			if ready || len(changes) != 1 || changes[0].Status != tracker.CheckFailed {
				t.Errorf("current failure: ready = %v, updates = %+v", ready, changes)
			}
			ApplyChecklistChanges(list.Checks, changes)
			if kind == tracker.CheckSecurityAudit {
				audit.Verdict = "pass"
			} else {
				evidence.ci.State, evidence.ci.FailedChecks = "success", nil
			}
			changes, ready = evaluateChecklist(list, evidence, false)
			if !ready || len(changes) != 1 || changes[0].Status != tracker.CheckPassed {
				t.Fatalf("current pass: ready = %v, updates = %+v", ready, changes)
			}
		})
	}
}
