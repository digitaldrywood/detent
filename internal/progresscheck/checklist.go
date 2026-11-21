package progresscheck

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
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
		if referencesOnlyCheck(criterion, gateConfig.Run) {
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

func NativeReviewCriteria(acceptance []string, body, projectCommand string) []string {
	criteria := slices.Clone(acceptance)
	for _, criterion := range config.VerificationCriteria(body) {
		if !referencesOnlyCheck(criterion, projectCommand) && !slices.Contains(criteria, criterion) {
			criteria = append(criteria, criterion)
		}
	}
	return criteria
}

var gateRepairPreparation = regexp.MustCompile(
	"(?i)^(?:Stage the first candidate from `[^`\n]+` for the host gate once\\. " +
		"It (?:rejects|handles|passes) [a-z0-9 -]+ but (?:misses|fails) [a-z0-9 -]+\\. )?" +
		"After the host records [a-z0-9 -]+ as failed, (?:repair|fix) (?:it|the [a-z0-9_-]+)(?: with `[^`\n]+`)?$",
)

func referencesOnlyCheck(criterion, command string) bool {
	if command == "" {
		return false
	}
	criterion = strings.TrimSpace(criterion)
	if preparation, final, found := strings.Cut(criterion, ", then run "); found && gateRepairPreparation.MatchString(preparation) {
		criterion = final
	}
	quoted := "`" + command + "`"
	for _, form := range []string{command, quoted, "Run " + command, "Run " + quoted} {
		if criterion == form || criterion == form+"." {
			return true
		}
	}
	return false
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
	hasProjectGateReceipt := gateDigest != "" && slices.ContainsFunc(checks, func(check tracker.ExecutionCheck) bool {
		return check.Kind == tracker.CheckProjectGate && check.CheckDigest == gateDigest
	})
	for _, check := range checks {
		var status tracker.ExecutionCheckStatus
		var receipt string
		kind := check.Kind
		if kind == tracker.CheckVerification && hasProjectGateReceipt && referencesOnlyCheck(check.Text, command.Command) {
			kind = tracker.CheckProjectGate
		}
		switch kind {
		case tracker.CheckProjectGate:
			if hasProjectGateReceipt && (check.Kind == tracker.CheckVerification || check.CheckDigest == gateDigest) {
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
		case tracker.CheckAcceptance, tracker.CheckVerification:
			if validator != nil {
				var gateCommand string
				if command != nil {
					gateCommand = command.Command
				}
				status, receipt = acceptedCriterion(check.Text, validator.Validator, evidence.head, evidence.tree, gateCommand)
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
		if kind != tracker.CheckProjectGate && check.Head == evidence.head && check.Tree == evidence.tree && (check.Status == tracker.CheckFlaky || check.Status == tracker.CheckPassed && status == tracker.CheckFailed || check.Status == tracker.CheckFailed && status == tracker.CheckPassed) {
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
		if i >= 0 && (checks[i].Status != update.Status || checks[i].Head != update.Head || checks[i].Tree != update.Tree) {
			changes = append(changes, update)
		}
	}
	return changes
}

func acceptedCriterion(text string, result *gate.ValidatorResult, head, tree, projectCommand string) (tracker.ExecutionCheckStatus, string) {
	for _, criterion := range result.CriteriaEvidence {
		if criterion.Criterion != text || criterion.RestatesImplementation || criterion.HeadSHA != head || tree == "" || criterion.TreeSHA != tree || strings.TrimSpace(criterion.Behavior) == "" {
			continue
		}
		if criterion.Kind != gate.EvidenceReceipt && criterion.Kind != gate.EvidenceNotVerified {
			continue
		}
		for _, command := range result.Commands {
			if command.Command == projectCommand || command.HeadSHA != head || command.TreeSHA != tree || command.ReceiptID == "" || !slices.Contains(criterion.ReceiptIDs, command.ReceiptID) {
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
