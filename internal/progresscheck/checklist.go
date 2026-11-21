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
	if gateConfig.Run != "" {
		add(tracker.CheckProjectGate, "Project command gate", policy.Digest([]byte(gateConfig.Run)), tracker.CheckUnverified)
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

func RunChecklistEvidence(previous *tracker.NativeRunData, current tracker.NativeRunData) (RoundInput, *gate.CommandResult) {
	var command *gate.CommandResult
	if current.Runtime != nil && current.Runtime.Validation != nil && current.Runtime.Validation.ReceiptID != "" {
		command = current.Runtime.Validation
	}
	for i := len(current.Steps) - 1; i >= 0; i-- {
		step := current.Steps[i]
		if step.Step != "validator" && step.Step != "security_audit" {
			continue
		}
		if step.Source.HeadSHA == "" || previous != nil && slices.ContainsFunc(previous.Steps, func(old tracker.NativeStepReceipt) bool {
			return reflect.DeepEqual(old, step)
		}) {
			continue
		}
		return RoundInput{HeadSHA: step.Source.HeadSHA, TreeSHA: checklistTree(step.Source.HeadSHA, command, step.Validator)}, command
	}
	if command != nil && (previous == nil || previous.Runtime == nil || previous.Runtime.Validation == nil || previous.Runtime.Validation.ReceiptID != command.ReceiptID) {
		return RoundInput{HeadSHA: command.HeadSHA, TreeSHA: command.TreeSHA}, command
	}
	return RoundInput{}, nil
}

func ApplyChecklistChanges(checks []tracker.ExecutionCheck, changes []tracker.ExecutionCheckUpdate) {
	for _, change := range changes {
		index := slices.IndexFunc(checks, func(check tracker.ExecutionCheck) bool { return check.Ordinal == change.Ordinal })
		if index >= 0 {
			check := &checks[index]
			check.Status, check.Head, check.Tree, check.Receipt = change.Status, change.Head, change.Tree, change.Receipt
		}
	}
}

func ChecklistChanges(checks []tracker.ExecutionCheck, verdict RoundVerdict, command *gate.CommandResult, steps []tracker.NativeStepReceipt) []tracker.ExecutionCheckUpdate {
	if verdict.HeadSHA == "" {
		return nil
	}
	var validator, audit *tracker.NativeStepReceipt
	for i := range steps {
		step := &steps[i]
		if step.PolicyID != verdict.PolicyID || step.Change.HeadSHA != verdict.HeadSHA || step.Source.HeadSHA != verdict.HeadSHA {
			continue
		}
		switch {
		case step.Step == "validator" && step.Validator != nil && step.Validator.Submitted && step.Validator.VersionID == step.Change.VersionID && step.Validator.HeadSHA == verdict.HeadSHA:
			validator = step
		case step.Step == "security_audit" && step.SecurityAudit != nil && step.SecurityAudit.VersionID == step.Change.VersionID && step.SecurityAudit.Run.HeadSHA == verdict.HeadSHA:
			audit = step
		}
	}
	updates := checklistEvidenceUpdates(checks, checklistEvidence{head: verdict.HeadSHA, tree: verdict.TreeSHA, command: command, validator: validator, audit: audit})
	return checklistDeltas(checks, updates)
}

type checklistEvidence struct {
	head, tree, approval string
	command              *gate.CommandResult
	validator, audit     *tracker.NativeStepReceipt
	auditApproved        bool
	disclose             bool
	ci                   *tracker.NativeLandingCIReceipt
}

func checklistEvidenceUpdates(checks []tracker.ExecutionCheck, evidence checklistEvidence) []tracker.ExecutionCheckUpdate {
	var updates []tracker.ExecutionCheckUpdate
	command, validator, audit := evidence.command, evidence.validator, evidence.audit
	var gateDigest string
	var gateStatus tracker.ExecutionCheckStatus
	if command != nil && !command.InstanceFailure() && command.HeadSHA == evidence.head && command.TreeSHA != "" && command.TreeSHA == evidence.tree && command.ReceiptID != "" {
		gateDigest = policy.Digest([]byte(command.Command))
		gateStatus = tracker.CheckPassed
		if command.ExitCode != 0 {
			gateStatus = tracker.CheckFailed
		}
	}
	for _, check := range checks {
		var status tracker.ExecutionCheckStatus
		var receipt string
		switch check.Kind {
		case tracker.CheckProjectGate:
			if gateDigest != "" && check.CheckDigest == gateDigest {
				status, receipt = gateStatus, command.ReceiptID
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
				} else if evidence.auditApproved {
					status = tracker.CheckManual
				}
			}
		case tracker.CheckAcceptance:
			if validator != nil {
				status, receipt = acceptedCriterion(check.Text, validator, evidence.head, evidence.tree)
				if status == "" && evidence.disclose && slices.Contains(validator.Validator.NotVerified, check.Text) {
					status, receipt = tracker.CheckDisclosed, checklistStepDigest(validator)
				}
			}
			if status == "" && evidence.approval != "" {
				status, receipt = tracker.CheckManual, evidence.approval
			}
		case tracker.CheckStatusGate:
			status, receipt = checklistStatusGate(check, evidence.head, evidence.ci)
		}
		if status == "" || receipt == "" {
			continue
		}
		if (check.Kind == tracker.CheckStatusGate || check.Kind == tracker.CheckSecurityAudit) && check.Head == evidence.head && check.Tree == evidence.tree && (check.Status == tracker.CheckFlaky || check.Status == tracker.CheckPassed && status == tracker.CheckFailed || check.Status == tracker.CheckFailed && status == tracker.CheckPassed) {
			status = tracker.CheckFlaky
		}
		updates = append(updates, tracker.ExecutionCheckUpdate{Ordinal: check.Ordinal, Status: status, Head: evidence.head, Tree: evidence.tree, Receipt: receipt})
	}
	return updates
}

func checklistDeltas(checks []tracker.ExecutionCheck, updates []tracker.ExecutionCheckUpdate) []tracker.ExecutionCheckUpdate {
	changes := updates[:0:0]
	for _, update := range updates {
		i := slices.IndexFunc(checks, func(check tracker.ExecutionCheck) bool { return check.Ordinal == update.Ordinal })
		if i < 0 {
			continue
		}
		check := checks[i]
		newGateFailure := check.Kind == tracker.CheckProjectGate && update.Status == tracker.CheckFailed && check.Receipt != update.Receipt
		if check.Status != update.Status || check.Head != update.Head || check.Tree != update.Tree || newGateFailure {
			changes = append(changes, update)
		}
	}
	return changes
}

func acceptedCriterion(text string, step *tracker.NativeStepReceipt, head, tree string) (tracker.ExecutionCheckStatus, string) {
	result := step.Validator
	for _, criterion := range result.CriteriaEvidence {
		if criterion.Criterion != text {
			continue
		}
		if criterion.RestatesImplementation {
			return "", ""
		}
		if criterion.Kind == gate.EvidenceNotVerified {
			if tree != "" && criterion.HeadSHA == head && criterion.TreeSHA == tree && strings.TrimSpace(criterion.Behavior) != "" {
				for _, command := range result.Commands {
					if command.ExitCode != 0 && !command.InstanceFailure() && command.HeadSHA == head && command.TreeSHA == tree &&
						command.ReceiptID != "" && slices.Contains(criterion.ReceiptIDs, command.ReceiptID) {
						return tracker.CheckFailed, command.ReceiptID
					}
				}
			}
			return "", ""
		}
		if result.Verdict != gate.ValidatorVerdictPass || strings.TrimSpace(criterion.Reference) == "" || strings.TrimSpace(criterion.Behavior) == "" {
			continue
		}
		switch criterion.Kind {
		case gate.EvidenceTest:
			return tracker.CheckPassed, checklistStepDigest(step)
		case gate.EvidenceReceipt, gate.EvidenceAttachment:
			if criterion.HeadSHA == head && tree != "" && criterion.TreeSHA == tree {
				if len(criterion.ReceiptIDs) == 1 {
					return tracker.CheckPassed, criterion.ReceiptIDs[0]
				}
				return tracker.CheckPassed, checklistStepDigest(step)
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
