package progresscheck

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFreezeChecklist(t *testing.T) {
	cfg := config.Default()
	cfg.Policy.ID = "test-policy"
	cfg.Gate.Run = "make check"
	cfg.Gate.SecurityAudit.Enabled = false
	for _, test := range []struct {
		name, verification string
		kinds              []tracker.ExecutionCheckKind
	}{
		{name: "deduplicate policy reference", verification: "Run `make check`.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckProjectGate}},
		{name: "bare command only", verification: "make check", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckProjectGate}},
		{name: "retains compound verification with a single command", verification: "Run `make check` and confirm the exported report contains both tenants.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckVerification, tracker.CheckProjectGate}},
		{name: "retains additional named verification", verification: "Run `make check` and confirm `TestRecovery` fails on the old version.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckVerification, tracker.CheckProjectGate}},
		{name: "unmapped command is review only", verification: "Run `make other`.", kinds: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckVerification, tracker.CheckProjectGate}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "## Acceptance criteria\n\n- Same text\n- Same text\n\n## How we know it worked\n\n" + test.verification
			list, err := FreezeChecklist(body, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Checks) != len(test.kinds) {
				t.Fatalf("checks=%+v, want kinds %v", list.Checks, test.kinds)
			}
			criteria := NativeReviewCriteria([]string{"Same text", "Same text"}, body, cfg.Gate.Run)
			if slices.Contains(criteria, test.verification) != slices.Contains(test.kinds, tracker.CheckVerification) {
				t.Fatalf("native review criteria do not match the checklist: %v", criteria)
			}
			for i, check := range list.Checks {
				if check.Ordinal != i+1 || check.Kind != test.kinds[i] {
					t.Fatalf("check[%d]=%+v; want kind %s", i, check, test.kinds[i])
				}
				if check.Kind == tracker.CheckProjectGate {
					if check.CheckDigest == "" || check.Status != tracker.CheckUnverified {
						t.Fatalf("project gate lacks its trusted binding: %+v", check)
					}
				} else if check.CheckDigest != "" || check.Status != tracker.CheckNeedsReview {
					t.Fatalf("unmapped criterion acquired execution authority: %+v", check)
				}
				if check.Kind == tracker.CheckVerification {
					if check.Text != test.verification {
						t.Fatalf("verification text = %q, want %q", check.Text, test.verification)
					}
					assertIndependentVerification(t, list, check)
				}
			}
			changed, err := FreezeChecklist(body+" changed", cfg)
			if err != nil || changed.Revision == list.Revision {
				t.Fatalf("issue edit preserved its checklist revision: %+v, %v", changed, err)
			}
		})
	}
}

func assertIndependentVerification(t *testing.T, list tracker.ExecutionChecklist, check tracker.ExecutionCheck) {
	t.Helper()
	list.Checks = slices.Clone(list.Checks)
	command := gate.CommandResult{Command: "make check", ReceiptID: "gate", HeadSHA: "head", TreeSHA: "tree"}
	verdict := RoundVerdict{RoundInput: RoundInput{HeadSHA: command.HeadSHA, TreeSHA: command.TreeSHA}, PolicyID: list.PolicyID}
	changes := ChecklistChanges(list.Checks, verdict, &command, nil)
	ApplyChecklistChanges(list.Checks, changes)
	if len(changes) != 1 || changes[0].Status != tracker.CheckPassed || changes[0].Receipt != command.ReceiptID || list.Checks[check.Ordinal-1].Status != tracker.CheckNeedsReview {
		t.Fatalf("generic gate changed independent verification: %+v", list.Checks)
	}
	proof := gate.CommandResult{Command: "verify exported tenant report", ReceiptID: "report-proof", HeadSHA: "head", TreeSHA: "tree"}
	criterion := gate.CriterionEvidence{Criterion: check.Text, Kind: gate.EvidenceReceipt, HeadSHA: "head", TreeSHA: "tree", ReceiptIDs: []string{command.ReceiptID}, Behavior: "verified required result"}
	validator := &gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, HeadSHA: "head", CriteriaEvidence: []gate.CriterionEvidence{criterion}, Commands: []gate.CommandResult{command, proof}}
	steps := []tracker.NativeStepReceipt{{Step: "validator", PolicyID: list.PolicyID, Source: tracker.NativeCheckpoint{HeadSHA: "head"}, Change: tracker.NativeChangeReference{HeadSHA: "head"}, Validator: validator}}
	if changes = ChecklistChanges(list.Checks, verdict, &command, steps); len(changes) != 0 {
		t.Fatalf("verifier citing only the generic gate changed the criterion: %+v", changes)
	}
	validator.CriteriaEvidence[0].ReceiptIDs = []string{proof.ReceiptID}
	changes = ChecklistChanges(list.Checks, verdict, &command, steps)
	if len(changes) != 1 || changes[0].Ordinal != check.Ordinal || changes[0].Status != tracker.CheckPassed || changes[0].Receipt != proof.ReceiptID {
		t.Fatalf("independent evidence must pass its own verification row: %+v", changes)
	}
}

func TestChecklistBindsAcceptanceToIndependentReceipt(t *testing.T) {
	command := gate.CommandResult{Command: "make check", ReceiptID: "gate", HeadSHA: "head", TreeSHA: "tree"}
	proof := gate.CommandResult{Command: "git diff -- main.go", ReceiptID: "inspection", HeadSHA: "head", TreeSHA: "tree"}
	verdict := RoundVerdict{RoundInput: RoundInput{HeadSHA: "head", TreeSHA: "tree"}, PolicyID: "policy", VerifiedCriteria: []string{"criterion"}}
	checks := []tracker.ExecutionCheck{{Ordinal: 1, Kind: tracker.CheckAcceptance, Text: "criterion", Status: tracker.CheckNeedsReview}}
	for _, test := range []struct {
		name, citation string
		proofTree      string
		want           string
	}{
		{name: "generic gate alone remains unverified", citation: "gate", proofTree: "tree"},
		{name: "mismatched tree remains unverified", citation: "inspection", proofTree: "wrong"},
		{name: "accepted independent receipt verifies criterion", citation: "inspection", proofTree: "tree", want: "inspection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := proof
			check.TreeSHA = test.proofTree
			validator := gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, HeadSHA: "head",
				CriteriaEvidence: []gate.CriterionEvidence{{Criterion: "criterion", Kind: gate.EvidenceReceipt, HeadSHA: "head", TreeSHA: "tree", ReceiptIDs: []string{test.citation}, Behavior: "inspected exact changed line"}},
				Commands:         []gate.CommandResult{check, command}}
			steps := []tracker.NativeStepReceipt{{Step: "validator", PolicyID: "policy", Source: tracker.NativeCheckpoint{HeadSHA: "head"}, Change: tracker.NativeChangeReference{HeadSHA: "head"}, Validator: &validator}}
			changes := ChecklistChanges(checks, verdict, &command, steps)
			if test.want == "" && len(changes) != 0 || test.want != "" && (len(changes) != 1 || changes[0].Receipt != test.want || changes[0].Status != tracker.CheckPassed) {
				t.Fatalf("checklist changes %+v, want receipt %q", changes, test.want)
			}
		})
	}
}

func TestRunRoundHandsBackSupersededChecklist(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		retried bool
	}{
		{name: "superseded contract", err: ErrChecklistSuperseded},
		{name: "ordinary round conflict", err: ErrRoundVerdictRejected, retried: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			assessments, publications, retries := 0, 0, 0
			_, err := RunRound(t.Context(), func(error) error {
				retries++
				return nil
			}, func(context.Context) error { return nil }, func(context.Context, RoundInput) error {
				publications++
				if publications == 1 {
					return test.err
				}
				return nil
			}, func(context.Context) (RoundAssessment, RoundInput, error) {
				assessments++
				return RoundAssessment{}, RoundInput{RoundID: "round"}, nil
			})
			if test.retried {
				if err != nil || assessments != 2 || publications != 2 || retries != 1 {
					t.Fatalf("ordinary conflict: assessments=%d publications=%d retries=%d error=%v", assessments, publications, retries, err)
				}
			} else if !errors.Is(err, test.err) || assessments != 1 || publications != 1 || retries != 0 {
				t.Fatalf("stale checklist: assessments=%d publications=%d retries=%d error=%v", assessments, publications, retries, err)
			}
		})
	}
}

func TestRunChecklistEvidenceOnlyProjectsNewReceipts(t *testing.T) {
	command := &gate.CommandResult{ReceiptID: "gate-1", HeadSHA: "head", TreeSHA: "tree"}
	previous := tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Validation: command}}
	for _, test := range []struct {
		name     string
		previous *tracker.NativeRunData
		current  tracker.NativeRunData
		want     string
	}{
		{name: "first gate result", current: previous, want: "gate-1"},
		{name: "same validation heartbeat", previous: &previous, current: previous},
		{name: "distinct gate receipt", previous: &previous, current: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Validation: &gate.CommandResult{ReceiptID: "gate-2", HeadSHA: "head"}}}, want: "gate-2"},
		{name: "independent review on same source", previous: &previous, current: tracker.NativeRunData{Runtime: previous.Runtime, Steps: []tracker.NativeStepReceipt{{Step: "validator", Source: tracker.NativeCheckpoint{HeadSHA: "head"}, Validator: &gate.ValidatorResult{Submitted: true}}}}, want: "gate-1"},
		{name: "unrelated tracked step", previous: &previous, current: tracker.NativeRunData{Runtime: previous.Runtime, Steps: []tracker.NativeStepReceipt{{Step: "agent_turn"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, result := RunChecklistEvidence(test.previous, test.current)
			var receipt string
			if result != nil {
				receipt = result.ReceiptID
			}
			if receipt != test.want {
				t.Fatalf("selected receipt %q, want %q", receipt, test.want)
			}
		})
	}
}
