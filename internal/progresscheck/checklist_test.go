package progresscheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFreezeChecklist(t *testing.T) {
	cfg := config.Default()
	cfg.Policy.ID = "test-policy"
	cfg.Gate.Validator.Enabled = true
	cfg.Gate.SecurityAudit.Enabled = true
	cfg.Gate.RequiredStatusChecks = []string{"ci"}
	const body = "## Acceptance criteria\n\n- Same text\n- Same text\n\n## How we know it worked\n\nRun `make check`. Inspect the report.\n"
	for _, test := range []struct {
		name, command string
		want          []tracker.ExecutionCheckKind
	}{
		{name: "with project gate", command: "make check", want: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckProjectGate, tracker.CheckStatusGate, tracker.CheckIndependentReview, tracker.CheckSecurityAudit}},
		{name: "without project gate", want: []tracker.ExecutionCheckKind{tracker.CheckAcceptance, tracker.CheckAcceptance, tracker.CheckStatusGate, tracker.CheckIndependentReview, tracker.CheckSecurityAudit}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := cfg
			cfg.Gate.Run = test.command
			if test.command == "" {
				cfg.Gate.Kind = gate.KindHumanReview
			}
			list, err := FreezeChecklist(body, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Checks) != len(test.want) {
				t.Fatalf("checks = %+v, want kinds %v", list.Checks, test.want)
			}
			for i, check := range list.Checks {
				if check.Ordinal != i+1 || check.Kind != test.want[i] {
					t.Fatalf("check %d = %+v, want kind %s", i, check, test.want[i])
				}
				switch check.Kind {
				case tracker.CheckAcceptance:
					if check.Text != "Same text" || check.Status != tracker.CheckNeedsReview || check.CheckDigest != "" {
						t.Fatalf("acceptance row = %+v", check)
					}
				case tracker.CheckProjectGate:
					if check.CheckDigest != policy.Digest([]byte(test.command)) || check.Status != tracker.CheckUnverified {
						t.Fatalf("project gate row = %+v", check)
					}
				}
			}
			changed, err := FreezeChecklist(body+" changed", cfg)
			if err != nil || changed.Revision == list.Revision {
				t.Fatalf("issue edit preserved its checklist revision: %+v, %v", changed, err)
			}
		})
	}
}

func TestProjectGateChecklistSameSourceRetry(t *testing.T) {
	cfg := config.Default()
	cfg.Policy.ID = "policy"
	cfg.Gate.Run = "make check"
	cfg.Gate.SecurityAudit.Enabled = false
	for _, test := range []struct {
		name   string
		prior  tracker.ExecutionCheckStatus
		exit   int
		failed tracker.ExecutionCheckStatus
	}{
		{name: "temporary failure followed by pass", prior: tracker.CheckUnverified, exit: gate.TemporaryFailureExitCode, failed: tracker.CheckUnverified},
		{name: "temporary failure after a pass", prior: tracker.CheckPassed, exit: gate.TemporaryFailureExitCode, failed: tracker.CheckPassed},
		{name: "code failure followed by pass", prior: tracker.CheckUnverified, exit: 1, failed: tracker.CheckFailed},
		{name: "code failure after a pass", prior: tracker.CheckPassed, exit: 1, failed: tracker.CheckFailed},
		{name: "pass recovers previously recorded flake", prior: tracker.CheckFlaky, exit: gate.TemporaryFailureExitCode, failed: tracker.CheckFlaky},
	} {
		t.Run(test.name, func(t *testing.T) {
			list, err := FreezeChecklist("", cfg)
			if err != nil {
				t.Fatal(err)
			}
			check := &list.Checks[0]
			check.Status = test.prior
			if test.prior != tracker.CheckUnverified {
				check.Head, check.Tree, check.Receipt = "head", "tree", "prior"
			}
			verdict := RoundVerdict{RoundInput: RoundInput{HeadSHA: "head", TreeSHA: "tree"}, PolicyID: list.PolicyID}
			failure := gate.CommandResult{Command: cfg.Gate.Run, HeadSHA: "head", TreeSHA: "tree", ReceiptID: "failed", ExitCode: test.exit}
			gate.NormalizeCommandAttribution("base", failure.HeadSHA, &failure)
			changes := ChecklistChanges(list.Checks, verdict, &failure, nil)
			ApplyChecklistChanges(list.Checks, changes)
			if check.Status != test.failed || test.exit == gate.TemporaryFailureExitCode && len(changes) != 0 {
				t.Errorf("after failure: status = %s, updates = %+v, want %s", check.Status, changes, test.failed)
			}
			if _, ready := evaluateChecklist(&list, checklistEvidence{head: "head", tree: "tree", command: &failure}, false); ready {
				t.Error("completion accepted a failed receipt")
			}
			pass := gate.CommandResult{Command: cfg.Gate.Run, HeadSHA: "head", TreeSHA: "tree", ReceiptID: "passed"}
			changes, ready := evaluateChecklist(&list, checklistEvidence{head: "head", tree: "tree", command: &pass}, false)
			ApplyChecklistChanges(list.Checks, changes)
			wantReceipt := pass.ReceiptID
			if test.failed == tracker.CheckPassed {
				wantReceipt = "prior"
			}
			if !ready || check.Status != tracker.CheckPassed || check.Receipt != wantReceipt {
				t.Errorf("same-source pass: ready = %v, check = %+v, want passed receipt", ready, check)
			}
		})
	}
}

func TestProjectGateChecklistAfterRepair(t *testing.T) {
	cfg := config.Default()
	cfg.Policy.ID = "policy"
	cfg.Gate.Run = "make check"
	cfg.Gate.SecurityAudit.Enabled = false
	const guidance = "Once the gate fails, fix the check and rerun the project gate."
	for _, test := range []struct {
		name, verification string
		legacy             bool
	}{
		{name: "paraphrased repair", verification: guidance},
		{name: "multi-step guidance", verification: "Stage a failing candidate, then repair it and run `make check`."},
		{name: "legacy verification row", verification: guidance, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "## How we know it worked\n\n" + test.verification
			list, err := FreezeChecklist(body, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Checks) != 1 || list.Checks[0].Kind != tracker.CheckProjectGate {
				t.Fatalf("verification guidance changed the frozen checklist: %+v", list.Checks)
			}
			legacy := tracker.ExecutionCheck{Ordinal: 1, Kind: tracker.CheckVerification, Text: test.verification, Status: tracker.CheckFailed, Head: "A", Tree: "tree-A", Receipt: "old-review"}
			if test.legacy {
				list.Checks[0].Ordinal = 2
				list.Checks = append([]tracker.ExecutionCheck{legacy}, list.Checks...)
			}
			failure := gate.CommandResult{Command: cfg.Gate.Run, HeadSHA: "A", TreeSHA: "tree-A", ReceiptID: "failed-A", ExitCode: 1}
			verdict := RoundVerdict{RoundInput: RoundInput{HeadSHA: "A", TreeSHA: "tree-A"}, PolicyID: list.PolicyID}
			ApplyChecklistChanges(list.Checks, ChecklistChanges(list.Checks, verdict, &failure, nil))
			if check := list.Checks[len(list.Checks)-1]; check.Status != tracker.CheckFailed || check.Receipt != failure.ReceiptID {
				t.Fatalf("first gate = %+v, want recorded failure on A", check)
			}
			pass := gate.CommandResult{Command: cfg.Gate.Run, HeadSHA: "B", TreeSHA: "tree-B", ReceiptID: "passed-B"}
			for _, stale := range []checklistEvidence{
				{head: "B", tree: "tree-B", command: &failure},
				{head: "B", tree: "other-tree", command: &pass},
			} {
				if _, ready := evaluateChecklist(&list, stale, false); ready {
					t.Fatal("completion accepted a receipt for different source")
				}
			}
			changes, ready := evaluateChecklist(&list, checklistEvidence{head: "B", tree: "tree-B", command: &pass}, false)
			ApplyChecklistChanges(list.Checks, changes)
			if !ready || len(changes) != 1 || changes[0].Status != tracker.CheckPassed || changes[0].Receipt != pass.ReceiptID {
				t.Fatalf("current gate: ready = %v, changes = %+v, want current gate pass", ready, changes)
			}
			if test.legacy && list.Checks[0] != legacy {
				t.Fatalf("legacy verification row changed: %+v", list.Checks[0])
			}
		})
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

func TestRoundPromptPreservesActiveChecklistOrdinals(t *testing.T) {
	list := &tracker.ExecutionChecklist{Revision: "revision", Checks: []tracker.ExecutionCheck{
		{Ordinal: 1, Kind: tracker.CheckVerification, Text: "legacy failure evidence", Receipt: "legacy-receipt"},
		{Ordinal: 2, Kind: tracker.CheckAcceptance, Text: "current criterion"},
	}}
	_, input, err := AssessRound(t.Context(), RoundEvidence{Phase: "implement", Checklist: list},
		RoundAssessment{Action: RoundContinue, NextPhase: "implement"}, false, false, func(_ context.Context, prompt string) (string, error) {
			if strings.Contains(prompt, "legacy failure evidence") || strings.Contains(prompt, "legacy-receipt") || !strings.Contains(prompt, `"ordinal":2`) || !strings.Contains(prompt, "current criterion") {
				t.Fatalf("round checklist = %s", prompt)
			}
			return `{"action":"continue","next_phase":"implement","reason":"continue"}`, nil
		})
	if err != nil || input.ChecklistRevision != list.Revision || len(list.Checks) != 2 || list.Checks[0].Receipt != "legacy-receipt" {
		t.Fatalf("legacy checklist was changed: input = %+v, checklist = %+v, error = %v", input, list, err)
	}
}
