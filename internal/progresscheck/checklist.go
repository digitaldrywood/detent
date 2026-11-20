package progresscheck

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaxChecklistEntries = 64
const ChecklistSupersededCode = "checklist_superseded"

var ErrChecklistSuperseded = errors.New("frozen execution checklist superseded")

const maxChecklistTextBytes = 32 << 10

func FreezeChecklist(body string, cfg config.Config) (tracker.ExecutionChecklist, error) {
	contract, err := cfg.PolicyIssueContract()
	if err != nil {
		return tracker.ExecutionChecklist{}, err
	}
	gateConfig := gate.Effective(cfg.Gate)
	checklist := tracker.ExecutionChecklist{
		Revision: ChecklistRevision(body, cfg.Policy.ID),
		PolicyID: cfg.Policy.ID,
		Checks:   []tracker.ExecutionCheck{},
	}
	bytes := 0
	add := func(kind tracker.ExecutionCheckKind, text, digest string, status tracker.ExecutionCheckStatus) {
		bytes += len(text)
		checklist.Checks = append(checklist.Checks, tracker.ExecutionCheck{Ordinal: len(checklist.Checks) + 1, Kind: kind, Text: text, CheckDigest: digest, Status: status})
	}
	for _, criterion := range contract.AcceptanceCriteria(body) {
		add(tracker.CheckAcceptance, criterion, "", tracker.CheckNeedsReview)
	}
	var gateReferences []string
	for _, criterion := range config.VerificationCriteria(body) {
		if gateConfig.Run != "" && referencesOnlyCheck(criterion, gateConfig.Run) {
			gateReferences = append(gateReferences, criterion)
			continue
		}
		add(tracker.CheckVerification, criterion, "", tracker.CheckNeedsReview)
	}
	if gateConfig.Run != "" {
		if len(gateReferences) == 0 {
			gateReferences = append(gateReferences, "Project command gate")
		}
		for _, reference := range gateReferences {
			add(tracker.CheckProjectGate, reference, policy.Digest([]byte(gateConfig.Run)), tracker.CheckUnverified)
		}
	}
	for _, check := range gateConfig.RequiredStatusChecks {
		add(tracker.CheckStatusGate, check, policy.Digest([]byte(check)), tracker.CheckUnverified)
	}
	if gateConfig.Validator.Enabled {
		add(tracker.CheckIndependentReview, "Independent acceptance review", "", tracker.CheckUnverified)
	}
	if gateConfig.SecurityAudit.Enabled {
		add(tracker.CheckSecurityAudit, "Independent security audit", "", tracker.CheckUnverified)
	}
	if len(checklist.Checks) > MaxChecklistEntries || bytes > maxChecklistTextBytes {
		return tracker.ExecutionChecklist{}, errors.New("issue checklist exceeds the execution evidence limit")
	}
	return checklist, nil
}

func ChecklistRevision(body, policyID string) string {
	return policy.Digest([]byte(policyID + "\x00" + body))
}

func referencesOnlyCheck(criterion, command string) bool {
	if strings.TrimSpace(criterion) == command {
		return true
	}
	fields := strings.Split(criterion, "`")
	return len(fields) == 3 && fields[1] == command
}

func RunChecklistEvidence(previous *tracker.NativeRunData, current tracker.NativeRunData) *gate.CommandResult {
	if current.Runtime == nil || current.Runtime.Validation == nil || current.Runtime.Validation.ReceiptID == "" {
		return nil
	}
	command := current.Runtime.Validation
	if previous == nil || previous.Runtime == nil || previous.Runtime.Validation == nil || previous.Runtime.Validation.ReceiptID != command.ReceiptID {
		return command
	}
	for _, step := range current.Steps {
		if step.Step != "validator" && step.Step != "security_audit" {
			continue
		}
		if !slices.ContainsFunc(previous.Steps, func(old tracker.NativeStepReceipt) bool {
			return old.Step == step.Step && reflect.DeepEqual(old, step)
		}) {
			return command
		}
	}
	return nil
}

func ApplyChecklistChanges(checks []tracker.ExecutionCheck, changes []tracker.ExecutionCheckUpdate) {
	for _, change := range changes {
		index := slices.IndexFunc(checks, func(check tracker.ExecutionCheck) bool { return check.Ordinal == change.Ordinal })
		if index >= 0 {
			check := &checks[index]
			check.Status, check.Tree, check.Receipt = change.Status, change.Tree, change.Receipt
		}
	}
}

func ChecklistChanges(checks []tracker.ExecutionCheck, verdict RoundVerdict, command *gate.CommandResult, steps []tracker.NativeStepReceipt) []tracker.ExecutionCheckUpdate {
	if command == nil || command.ReceiptID == "" || command.TreeSHA == "" || command.TreeSHA != verdict.TreeSHA || command.HeadSHA != verdict.HeadSHA {
		return nil
	}
	var validator, audit *tracker.NativeStepReceipt
	for i := range steps {
		step := &steps[i]
		if step.PolicyID != verdict.PolicyID || step.Change.HeadSHA != verdict.HeadSHA || step.Source.HeadSHA != verdict.HeadSHA {
			continue
		}
		switch {
		case step.Step == "validator" && step.Validator != nil && step.Validator.Submitted:
			validator = step
		case step.Step == "security_audit" && step.SecurityAudit != nil:
			audit = step
		}
	}
	var changes []tracker.ExecutionCheckUpdate
	for _, check := range checks {
		var status tracker.ExecutionCheckStatus
		var receipt string
		switch check.Kind {
		case tracker.CheckProjectGate:
			if check.CheckDigest == policy.Digest([]byte(command.Command)) {
				receipt = command.ReceiptID
				status = tracker.CheckPassed
				if command.ExitCode != 0 {
					status = tracker.CheckFailed
				}
			}
		case tracker.CheckIndependentReview:
			if validator != nil {
				receipt = checklistStepDigest(validator)
				status = tracker.CheckFailed
				if validator.Validator.Verdict == gate.ValidatorVerdictPass {
					status = tracker.CheckPassed
				}
			}
		case tracker.CheckSecurityAudit:
			if audit != nil {
				receipt = checklistStepDigest(audit)
				status = tracker.CheckFailed
				if audit.SecurityAudit.Verdict == "pass" {
					status = tracker.CheckPassed
				}
			}
		case tracker.CheckAcceptance:
			if validator != nil {
				status, receipt = acceptedCriterion(check.Text, validator.Validator, command)
			}
		}
		if status == "" || receipt == "" {
			continue
		}
		if check.Tree == command.TreeSHA && (check.Status == tracker.CheckFlaky || check.Status == tracker.CheckPassed && status == tracker.CheckFailed || check.Status == tracker.CheckFailed && status == tracker.CheckPassed) {
			status = tracker.CheckFlaky
		}
		if check.Status != status || check.Tree != command.TreeSHA {
			changes = append(changes, tracker.ExecutionCheckUpdate{Ordinal: check.Ordinal, Status: status, Tree: command.TreeSHA, Receipt: receipt})
		}
	}
	return changes
}

func acceptedCriterion(text string, result *gate.ValidatorResult, projectGate *gate.CommandResult) (tracker.ExecutionCheckStatus, string) {
	for _, criterion := range result.CriteriaEvidence {
		if criterion.Criterion != text || criterion.RestatesImplementation || criterion.HeadSHA != projectGate.HeadSHA || criterion.TreeSHA != projectGate.TreeSHA || strings.TrimSpace(criterion.Behavior) == "" {
			continue
		}
		if criterion.Kind != gate.EvidenceReceipt && criterion.Kind != gate.EvidenceNotVerified {
			continue
		}
		for _, command := range result.Commands {
			if command.Command == projectGate.Command || command.HeadSHA != projectGate.HeadSHA || command.TreeSHA != projectGate.TreeSHA || command.ReceiptID == "" || !slices.Contains(criterion.ReceiptIDs, command.ReceiptID) {
				continue
			}
			if command.ExitCode == 0 && criterion.Kind == gate.EvidenceReceipt && result.Verdict == gate.ValidatorVerdictPass {
				return tracker.CheckPassed, command.ReceiptID
			}
			if command.ExitCode != 0 && criterion.Kind == gate.EvidenceNotVerified {
				return tracker.CheckFailed, command.ReceiptID
			}
		}
	}
	return "", ""
}

func checklistStepDigest(step *tracker.NativeStepReceipt) string {
	raw, err := json.Marshal(step)
	if err != nil {
		return ""
	}
	return policy.Digest(raw)
}
