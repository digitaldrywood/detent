package progresscheck

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checklistValidator(version, base, head string) *gate.ValidatorResult {
	return &gate.ValidatorResult{VersionID: version, Repository: "example", BaseSHA: base, HeadSHA: head, Submitted: true, SessionID: 1, Verdict: gate.ValidatorVerdictPass,
		Commands: []gate.CommandResult{{Command: "inspect source", HeadSHA: head, TreeSHA: "tree", ReceiptID: "inspection"}},
		CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "criterion", Kind: gate.EvidenceReceipt, Reference: "source inspection", Behavior: "independent behavior",
			HeadSHA: head, TreeSHA: "tree", ReceiptIDs: []string{"inspection"}}}}
}

func checklistValidatorStep(policyID string, result *gate.ValidatorResult) tracker.NativeStepReceipt {
	return tracker.NativeStepReceipt{Step: "validator", PolicyID: policyID, Source: tracker.NativeCheckpoint{HeadSHA: result.HeadSHA},
		Change: tracker.NativeChangeReference{VersionID: result.VersionID, HeadSHA: result.HeadSHA}, Validator: result}
}

func TestChecklistReviewRejectionThenApproval(t *testing.T) {
	const head, base, version, policyID = "head", "base", "version", "policy"
	for _, test := range []struct {
		name string
		exit int
		red  bool
	}{
		{name: "genuine review rejection then approval", exit: 1, red: true},
		{name: "instance failure then approval", exit: gate.TemporaryFailureExitCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks := []tracker.ExecutionCheck{
				{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: "criterion", Status: tracker.CheckNeedsReview},
				{Ordinal: 2, Kind: tracker.CheckIndependentReview, Status: tracker.CheckUnverified},
			}
			rejected := checklistValidator(version, base, head)
			rejected.Verdict = gate.ValidatorVerdictRework
			rejected.Commands[0].ExitCode = test.exit
			gate.NormalizeAttribution(rejected)
			rejected.CriteriaEvidence[0].Kind = gate.EvidenceNotVerified
			verdict := RoundVerdict{PolicyID: policyID, RoundInput: RoundInput{HeadSHA: head, TreeSHA: "tree"}}
			changes := ChecklistChanges(checks, verdict, nil, []tracker.NativeStepReceipt{checklistValidatorStep(policyID, rejected)})
			ApplyChecklistChanges(checks, changes)
			if test.red {
				for _, check := range checks {
					if check.Status != tracker.CheckFailed {
						t.Errorf("genuine rejection projected %+v, want failed", check)
					}
				}
			} else if len(changes) != 0 {
				t.Errorf("instance failure changed the checklist: %+v", changes)
			}
			approved := checklistValidator(version, base, head)
			changes = ChecklistChanges(checks, verdict, nil, []tracker.NativeStepReceipt{checklistValidatorStep(policyID, approved)})
			if len(changes) != 2 || changes[0].Status != tracker.CheckPassed || changes[1].Status != tracker.CheckPassed {
				t.Fatalf("approved re-review updates = %+v, want both rows passed", changes)
			}
		})
	}
}

func TestChecklistPolicyRowsFollowCurrentEvidence(t *testing.T) {
	const head, tree, policyID = "head", "tree", "policy"
	verdict := RoundVerdict{PolicyID: policyID, RoundInput: RoundInput{HeadSHA: head, TreeSHA: tree}}
	for _, kind := range []tracker.ExecutionCheckKind{tracker.CheckSecurityAudit, tracker.CheckStatusGate} {
		t.Run(string(kind), func(t *testing.T) {
			checks := []tracker.ExecutionCheck{{Ordinal: 1, Kind: kind, Text: "required", Head: head, Tree: tree, Status: tracker.CheckFlaky}}
			audit := &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{HeadSHA: head, Attribution: "instance"}}
			var steps []tracker.NativeStepReceipt
			var runtime *tracker.NativeRuntimeObservation
			if kind == tracker.CheckSecurityAudit {
				steps = []tracker.NativeStepReceipt{{Step: "security_audit", PolicyID: policyID, Source: tracker.NativeCheckpoint{HeadSHA: head}, Change: tracker.NativeChangeReference{HeadSHA: head}, SecurityAudit: audit}}
				if changes := ChecklistChanges(checks, verdict, nil, steps); len(changes) != 0 {
					t.Errorf("instance audit updates = %+v", changes)
				}
				audit.Run.Attribution = "change"
			} else {
				runtime = &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{
					CI: &tracker.NativeLandingCIReceipt{HeadSHA: head, PullRequest: 1, RequiredChecks: []string{"required"}, FailedChecks: []string{"required"}},
				}}
				source := RunChecklistEvidence(nil, tracker.NativeRunData{Runtime: runtime})
				if source.HeadSHA != head {
					t.Fatalf("CI observation selected source %+v", source)
				}
			}
			changes := ChecklistChanges(checks, verdict, runtime, steps)
			if len(changes) != 1 || changes[0].Status != tracker.CheckFailed {
				t.Errorf("current failure updates = %+v", changes)
			}
			ApplyChecklistChanges(checks, changes)
			if kind == tracker.CheckSecurityAudit {
				audit.Verdict = "pass"
			} else {
				runtime.Landing.CI.State, runtime.Landing.CI.FailedChecks = "success", nil
			}
			changes = ChecklistChanges(checks, verdict, runtime, steps)
			if len(changes) != 1 || changes[0].Status != tracker.CheckPassed {
				t.Fatalf("current pass updates = %+v", changes)
			}
		})
	}
}
