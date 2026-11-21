package progresscheck

import (
	"slices"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReviewedChecklist(list *tracker.ExecutionChecklist, body string, detail tracker.ChangeDetail, version tracker.ChangeVersion, ci *tracker.NativeLandingCIReceipt, preparingLanding bool) ([]tracker.ExecutionCheckUpdate, bool) {
	if list == nil {
		return nil, true
	}
	if detail.Summary.Status != "reviewed" || version.ID == "" || version.ID != detail.Change.CurrentVersion || list.AttemptID != version.AttemptID ||
		list.PolicyID != version.PolicyID || list.Revision != ChecklistRevision(body, version.PolicyID) {
		return nil, false
	}
	evidence := checklistEvidence{head: version.HeadSHA, command: version.Validation, ci: ci}
	var validator *gate.ValidatorResult
	for i := range detail.Reviews {
		review := detail.Reviews[i]
		if review.VersionID != version.ID || review.Decision != "approved" && review.Decision != "changes_requested" {
			continue
		}
		switch {
		case review.Validator != nil && review.Actor.Kind == "runner":
			result := review.Validator
			if result.Submitted && result.SessionID > 0 && result.VersionID == version.ID && result.HeadSHA == version.HeadSHA && result.BaseSHA == version.BaseSHA && result.Repository == version.Repository {
				validator = result
			}
		case review.SecurityAudit != nil && !review.SecurityAudit.Landing:
			audit := review.SecurityAudit
			if audit.VersionID == version.ID && audit.Run.BaseSHA == version.BaseSHA && audit.Run.HeadSHA == version.HeadSHA && audit.Run.Repository == version.Repository {
				evidence.audit = &tracker.NativeStepReceipt{SecurityAudit: audit}
				evidence.auditApproved = detail.Summary.SecurityAudit == "passed"
			}
		case review.ImplementationReview == nil && review.SecurityAudit == nil && review.Actor.Kind == "human" &&
			review.Decision == "approved" && detail.Summary.NativeReview == "approved":
			evidence.approval = review.ID
		}
	}
	if validator != nil {
		evidence.validator = &tracker.NativeStepReceipt{Validator: validator}
		if workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, version.Policy); err == nil {
			evidence.disclose = allowsChecklistDisclosure(workflow.Config.Gate.Validator, validator)
		}
	}
	evidence.tree = checklistTree(evidence.head, evidence.command, validator)
	return evaluateChecklist(list, evidence, preparingLanding && version.Policy.Gates.GitHubPullRequest)
}

func NoChangeChecklist(list *tracker.ExecutionChecklist, completion tracker.NativeNoChangeCompletion, validator, review *gate.ValidatorResult, audit *tracker.SecurityAuditReview, cfg gate.Config) ([]tracker.ExecutionCheckUpdate, bool) {
	if list == nil {
		return nil, true
	}
	if list.PolicyID != completion.PolicyID || completion.VerifiedSHA == "" {
		return nil, false
	}
	evidence := checklistEvidence{head: completion.VerifiedSHA, command: completion.Validation}
	if validator == nil && !cfg.Validator.Enabled {
		validator = review
	}
	if validator != nil && validator.Submitted && validator.SessionID > 0 && validator.VersionID == completion.VersionID() &&
		validator.HeadSHA == evidence.head && validator.BaseSHA == evidence.head && validator.Repository == completion.Repository {
		evidence.validator = &tracker.NativeStepReceipt{Validator: validator}
		evidence.disclose = allowsChecklistDisclosure(cfg.Validator, validator)
	}
	if evidence.command == nil {
		for _, result := range []*gate.ValidatorResult{review, validator} {
			if result != nil {
				for i := range result.Commands {
					command := &result.Commands[i]
					if command.Command == cfg.Run && command.HeadSHA == evidence.head && command.TreeSHA != "" && command.ReceiptID != "" {
						evidence.command = command
					}
				}
			}
		}
	}
	if audit != nil && audit.VersionID == completion.VersionID() && audit.Run.HeadSHA == evidence.head && audit.Run.BaseSHA == evidence.head && audit.Run.Repository == completion.Repository {
		evidence.audit = &tracker.NativeStepReceipt{SecurityAudit: audit}
	}
	evidence.tree = checklistTree(evidence.head, evidence.command, validator)
	return evaluateChecklist(list, evidence, false)
}

func evaluateChecklist(list *tracker.ExecutionChecklist, evidence checklistEvidence, deferredStatusChecks bool) ([]tracker.ExecutionCheckUpdate, bool) {
	updates := checklistEvidenceUpdates(list.Checks, evidence)
	changes := checklistDeltas(list.Checks, updates)
	for _, check := range list.Checks {
		if check.Kind == tracker.CheckStatusGate && deferredStatusChecks {
			continue
		}
		if !slices.ContainsFunc(updates, func(update tracker.ExecutionCheckUpdate) bool {
			return update.Ordinal == check.Ordinal && (update.Status == tracker.CheckPassed || update.Status == tracker.CheckManual || update.Status == tracker.CheckDisclosed)
		}) {
			return changes, false
		}
	}
	return changes, true
}

func NoChangeStepChecklist(list *tracker.ExecutionChecklist, completion tracker.NativeNoChangeCompletion, steps []tracker.NativeStepReceipt, cfg gate.Config) ([]tracker.ExecutionCheckUpdate, bool) {
	var validator, review *gate.ValidatorResult
	var audit *tracker.SecurityAuditReview
	for _, step := range steps {
		if step.Change.VersionID != completion.VersionID() || step.Change.HeadSHA != completion.VerifiedSHA || step.Source.HeadSHA != completion.VerifiedSHA ||
			step.PolicyID != completion.PolicyID {
			continue
		}
		switch step.Step {
		case "validator":
			validator = step.Validator
		case "review":
			review = step.Validator
		case "security_audit":
			audit = step.SecurityAudit
		}
	}
	return NoChangeChecklist(list, completion, validator, review, audit, cfg)
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
	}
	return tree
}

func allowsChecklistDisclosure(cfg gate.ValidatorConfig, result *gate.ValidatorResult) bool {
	return cfg.UnverifiedCriteria == gate.UnverifiedCriteriaDisclose && result.Verdict == gate.ValidatorVerdictPass && len(result.UnsatisfiableCriteria()) == 0
}

func checklistStatusGate(check tracker.ExecutionCheck, head string, ci *tracker.NativeLandingCIReceipt) (tracker.ExecutionCheckStatus, string) {
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
