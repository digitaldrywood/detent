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

func RunChecklistEvidence(previous *tracker.NativeRunData, current tracker.NativeRunData) RoundInput {
	var command *gate.CommandResult
	if current.Runtime != nil && current.Runtime.Validation != nil && current.Runtime.Validation.ReceiptID != "" {
		command = current.Runtime.Validation
	}
	for i := len(current.Steps) - 1; i >= 0; i-- {
		step := current.Steps[i]
		if step.Step != "validator" && step.Step != "security_audit" &&
			(step.Step != "review" || !noChangeChecklistReview(&step, current.PolicyID, step.Source.HeadSHA)) {
			continue
		}
		if step.Source.HeadSHA == "" || previous != nil && slices.ContainsFunc(previous.Steps, func(old tracker.NativeStepReceipt) bool {
			return reflect.DeepEqual(old, step)
		}) {
			continue
		}
		return RoundInput{HeadSHA: step.Source.HeadSHA, TreeSHA: checklistTree(step.Source.HeadSHA, command, step.Validator)}
	}
	if command != nil && (previous == nil || previous.Runtime == nil || previous.Runtime.Validation == nil || previous.Runtime.Validation.ReceiptID != command.ReceiptID) {
		return RoundInput{HeadSHA: command.HeadSHA, TreeSHA: command.TreeSHA}
	}
	if ci := checklistCI(current.Runtime); ci != nil && (previous == nil || !reflect.DeepEqual(ci, checklistCI(previous.Runtime))) {
		return RoundInput{HeadSHA: ci.HeadSHA, TreeSHA: checklistTree(ci.HeadSHA, command, nil)}
	}
	return RoundInput{}
}

func checklistCI(runtime *tracker.NativeRuntimeObservation) *tracker.NativeLandingCIReceipt {
	if runtime == nil || runtime.Landing == nil {
		return nil
	}
	return runtime.Landing.CI
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

func ChecklistChanges(checks []tracker.ExecutionCheck, verdict RoundVerdict, runtime *tracker.NativeRuntimeObservation, steps []tracker.NativeStepReceipt) []tracker.ExecutionCheckUpdate {
	if verdict.HeadSHA == "" {
		return nil
	}
	var review, validator, audit *tracker.NativeStepReceipt
	for i := range steps {
		step := &steps[i]
		if step.PolicyID != verdict.PolicyID || step.Change.HeadSHA != verdict.HeadSHA || step.Source.HeadSHA != verdict.HeadSHA {
			continue
		}
		switch {
		case step.Step == "review" && noChangeChecklistReview(step, verdict.PolicyID, verdict.HeadSHA):
			review = step
		case step.Step == "validator" && step.Validator != nil && step.Validator.Submitted && step.Validator.VersionID == step.Change.VersionID && step.Validator.HeadSHA == verdict.HeadSHA:
			validator = step
		case step.Step == "security_audit" && step.SecurityAudit != nil && step.SecurityAudit.VersionID == step.Change.VersionID && step.SecurityAudit.Run.HeadSHA == verdict.HeadSHA:
			audit = step
		}
	}
	var updates []tracker.ExecutionCheckUpdate
	var command *gate.CommandResult
	if runtime != nil {
		command = runtime.Validation
	}
	head, tree := verdict.HeadSHA, verdict.TreeSHA
	ci := checklistCI(runtime)
	for _, check := range checks {
		var status tracker.ExecutionCheckStatus
		var receipt string
		switch check.Kind {
		case tracker.CheckProjectGate:
			if checklistGateMatches(check, command, head, tree) {
				receipt = command.ReceiptID
				status = tracker.CheckPassed
				if command.ExitCode != 0 {
					status = tracker.CheckFailed
				}
			} else {
				for _, step := range []*tracker.NativeStepReceipt{validator, review} {
					if !noChangeChecklistReview(step, verdict.PolicyID, head) {
						continue
					}
					if gate := passingReviewGate(check, step.Validator, head, tree); gate != nil {
						status, receipt = tracker.CheckPassed, gate.ReceiptID
						break
					}
				}
			}
		case tracker.CheckIndependentReview:
			if validator != nil && (validator.Validator.Verdict == gate.ValidatorVerdictPass || !validator.Validator.InstanceFailure()) {
				receipt = checklistStepDigest(validator)
				status = tracker.CheckFailed
				if validator.Validator.Verdict == gate.ValidatorVerdictPass {
					status = tracker.CheckPassed
				}
			}
		case tracker.CheckSecurityAudit:
			if audit != nil && (audit.SecurityAudit.Verdict == "pass" || audit.SecurityAudit.Run.Attribution != gate.InstanceAttribution) {
				receipt = checklistStepDigest(audit)
				status = tracker.CheckFailed
				if audit.SecurityAudit.Verdict == "pass" {
					status = tracker.CheckPassed
				}
			}
		case tracker.CheckAcceptance:
			if validator != nil {
				status, receipt = acceptedCriterion(check.Text, validator, head, tree)
				if status == "" && validator.Validator.Verdict == gate.ValidatorVerdictPass && slices.Contains(validator.Validator.NotVerified, check.Text) {
					status, receipt = tracker.CheckDisclosed, checklistStepDigest(validator)
				}
			}
		case tracker.CheckStatusGate:
			status, receipt = ChecklistStatusGate(check, head, ci)
		}
		if status == "" || receipt == "" {
			continue
		}
		updates = append(updates, tracker.ExecutionCheckUpdate{Ordinal: check.Ordinal, Status: status, Head: head, Tree: tree, Receipt: receipt})
	}
	return checklistDeltas(checks, updates)
}

func noChangeChecklistReview(step *tracker.NativeStepReceipt, policyID, head string) bool {
	return step != nil && step.PolicyID == policyID && step.Source.HeadSHA == head && step.Change.HeadSHA == head &&
		step.Change.ChangeID == "" && step.Change.VersionID != "" && step.Validator != nil && step.Validator.Submitted &&
		step.Validator.SessionID > 0 && step.Validator.VersionID == step.Change.VersionID && step.Validator.HeadSHA == head &&
		step.Validator.BaseSHA == head
}

func checklistGateMatches(check tracker.ExecutionCheck, command *gate.CommandResult, head, tree string) bool {
	return command != nil && command.ReceiptID != "" && !command.InstanceFailure() && command.HeadSHA == head &&
		tree != "" && command.TreeSHA == tree && check.CheckDigest == policy.Digest([]byte(command.Command))
}

func NoChangeChecklistGate(check tracker.ExecutionCheck, completion tracker.NativeNoChangeCompletion, step tracker.NativeStepReceipt) *gate.CommandResult {
	head := completion.VerifiedSHA
	if !noChangeChecklistReview(&step, completion.PolicyID, head) || step.Change.VersionID != completion.VersionID() || step.Validator.Repository != completion.Repository {
		return nil
	}
	return passingReviewGate(check, step.Validator, head, checklistTree(head, completion.Validation, step.Validator))
}

func passingReviewGate(check tracker.ExecutionCheck, result *gate.ValidatorResult, head, tree string) *gate.CommandResult {
	for i := range result.Commands {
		command := &result.Commands[i]
		if command.ExitCode == 0 && checklistGateMatches(check, command, head, tree) {
			return command
		}
	}
	return nil
}

func checklistDeltas(checks []tracker.ExecutionCheck, updates []tracker.ExecutionCheckUpdate) []tracker.ExecutionCheckUpdate {
	changes := updates[:0:0]
	for _, update := range updates {
		i := slices.IndexFunc(checks, func(check tracker.ExecutionCheck) bool { return check.Ordinal == update.Ordinal })
		if i >= 0 && (checks[i].Status != update.Status || checks[i].Head != update.Head || checks[i].Tree != update.Tree) {
			changes = append(changes, update)
		}
	}
	return changes
}

func acceptedCriterion(text string, step *tracker.NativeStepReceipt, head, tree string) (tracker.ExecutionCheckStatus, string) {
	result := step.Validator
	for _, criterion := range result.CriteriaEvidence {
		if criterion.Criterion != text || criterion.RestatesImplementation || strings.TrimSpace(criterion.Behavior) == "" {
			continue
		}
		exactSource := criterion.HeadSHA == head && tree != "" && criterion.TreeSHA == tree
		if criterion.Kind == gate.EvidenceNotVerified && exactSource && result.Attribution != gate.InstanceAttribution {
			for _, command := range result.Commands {
				if command.ExitCode != 0 && !command.InstanceFailure() && command.HeadSHA == head && command.TreeSHA == tree &&
					command.ReceiptID != "" && slices.Contains(criterion.ReceiptIDs, command.ReceiptID) {
					return tracker.CheckFailed, command.ReceiptID
				}
			}
		}
		if result.Verdict == gate.ValidatorVerdictPass && criterion.Reference != "" && (criterion.Kind == gate.EvidenceTest ||
			exactSource && (criterion.Kind == gate.EvidenceReceipt || criterion.Kind == gate.EvidenceAttachment)) {
			if criterion.Kind != gate.EvidenceTest && len(criterion.ReceiptIDs) == 1 && criterion.ReceiptIDs[0] != "" {
				return tracker.CheckPassed, criterion.ReceiptIDs[0]
			}
			return tracker.CheckPassed, checklistStepDigest(step)
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

func checklistTree(head string, command *gate.CommandResult, validator *gate.ValidatorResult) string {
	if command != nil && command.HeadSHA == head && command.TreeSHA != "" {
		return command.TreeSHA
	}
	var tree string
	if validator != nil && validator.HeadSHA == head {
		for _, result := range validator.Commands {
			if result.HeadSHA != head || result.TreeSHA == "" || result.ReceiptID == "" {
				continue
			}
			if tree != "" && result.TreeSHA != tree {
				return ""
			}
			tree = result.TreeSHA
		}
		for _, read := range validator.AttachmentReads {
			if read.HeadSHA != head || read.TreeSHA == "" || read.ReceiptID == "" {
				continue
			}
			if tree != "" && read.TreeSHA != tree {
				return ""
			}
			tree = read.TreeSHA
		}
	}
	return tree
}

func ChecklistStatusGate(check tracker.ExecutionCheck, head string, ci *tracker.NativeLandingCIReceipt) (tracker.ExecutionCheckStatus, string) {
	if ci == nil || ci.HeadSHA != head || ci.PullRequest <= 0 || !slices.Contains(ci.RequiredChecks, check.Text) {
		return "", ""
	}
	if slices.Contains(ci.FailedChecks, check.Text) {
		return tracker.CheckFailed, policy.Digest([]byte(head + "\x00" + check.Text + "\x00failure"))
	}
	if ci.State != "success" || slices.Contains(ci.PendingChecks, check.Text) || slices.Contains(ci.MissingChecks, check.Text) {
		return "", ""
	}
	return tracker.CheckPassed, policy.Digest([]byte(head + "\x00" + check.Text + "\x00success"))
}
