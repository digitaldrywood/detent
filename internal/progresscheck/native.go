package progresscheck

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func AttemptDiagnostics(attempt tracker.NativeAttempt) string {
	var detail strings.Builder
	detail.WriteString("Attempt " + attempt.AttemptID + "\n")
	write := func(label string, value any) {
		encoded, err := json.Marshal(value)
		if err == nil {
			detail.WriteString(label + ": " + string(encoded) + "\n")
		}
	}
	command := func(label string, result *gate.CommandResult) {
		if result == nil {
			return
		}
		copy := *result
		copy.Command = bound(copy.Command, 512)
		copy.Output = diagnosticExcerpt(copy.Output, 4096)
		copy.OutputTruncated = copy.OutputTruncated || copy.Output != result.Output
		copy.Pipeline = nil
		write(label, copy)
	}
	if attempt.Runtime != nil {
		command("validation", attempt.Runtime.Validation)
		if attempt.Runtime.Landing != nil {
			command("landing gate", attempt.Runtime.Landing.Gate)
		}
	}
	for _, step := range attempt.Steps {
		command("step "+step.Step, step.Validation)
		if step.Validator != nil {
			write("validator summary", step.Validator.Summary)
			write("validator findings", step.Validator.Findings)
			for i := range min(len(step.Validator.Commands), 8) {
				command("validator command", &step.Validator.Commands[i])
			}
		}
		if detail.Len() >= 12<<10 {
			break
		}
	}
	if attempt.TerminalFailure != nil {
		write("terminal failure", attempt.TerminalFailure)
	}
	write("unavailable", attempt.Unavailable)
	return diagnosticExcerpt(detail.String(), 12<<10)
}

func diagnosticExcerpt(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit/2], "") + "\n[truncated; read retained receipt for more]\n" + strings.ToValidUTF8(value[len(value)-limit/2:], "")
}

func NativeRequest(recovery tracker.NativeRecovery, policy, instance string) Request {
	return nativeRequest(recovery, policy, instance, nil)
}

func NativeTerminalRequest(recovery tracker.NativeRecovery, attempt tracker.NativeAttempt, policy string, terminal TerminalEvidence) Request {
	history := make([]tracker.NativeAttempt, 0, MaxAttempts)
	for _, prior := range recovery.Attempts {
		if prior.FencingToken < attempt.FencingToken && prior.Status != "running" {
			history = append(history, prior)
		}
	}
	recovery.Attempts = append(history, attempt)
	terminal.AttemptID = attempt.AttemptID
	return nativeRequest(recovery, policy, string(attempt.MachineID), &terminal)
}

func nativeRequest(recovery tracker.NativeRecovery, policy, instance string, terminal *TerminalEvidence) Request {
	r := Request{ProjectID: string(recovery.Issue.ProjectID), IssueID: string(recovery.Issue.WorkItemID), PolicyID: policy,
		EvidenceRevision: strconv.FormatInt(int64(recovery.Issue.Revision), 10), Instance: instance,
		AvailableSteps: []string{"implement"}, Context: bound(recovery.Issue.Title+"\n"+recovery.Issue.Body, 2048)}
	attempts := recovery.Attempts
	if len(attempts) > 0 && attempts[len(attempts)-1].Status == "running" {
		return r
	}
	if len(attempts) > MaxAttempts {
		attempts = attempts[len(attempts)-MaxAttempts:]
	}
	for _, a := range attempts {
		outcome := a.Outcome
		if outcome == "" {
			outcome = a.Status
		}
		if outcome == "succeeded" {
			outcome = "success"
		}
		if outcome == "interrupted" {
			outcome = "abandoned"
		}
		if !Failed(outcome) && outcome != "success" && outcome != "cancelled" {
			continue
		}
		entry := Attempt{ID: a.AttemptID, Outcome: outcome, Instance: string(a.MachineID), WorkProduct: len(a.ArtifactIDs) > 0}
		if a.Disposition != nil {
			entry.Disposition, entry.ReasonCode = a.Disposition.Status, a.Disposition.ReasonCode
		}
		if a.Runtime != nil && a.Runtime.Landing != nil {
			landing := a.Runtime.Landing
			entry.RefusalKind, entry.RefusalBaseSHA, entry.RefusalHeadSHA = landing.RefusalKind, landing.BaseSHA, landing.HeadSHA
		}
		if a.Runtime != nil && a.Runtime.WorkerLaunch != nil {
			entry.InstanceVersion = a.Runtime.WorkerLaunch.ExecutionVersion
		}
		if a.Identity != nil {
			entry.Step = a.Identity.Role
		}
		if a.TerminalFailure != nil {
			if a.TerminalFailure.Stage != "" {
				entry.Step = a.TerminalFailure.Stage
			}
			entry.Error, entry.FailureClass, entry.Attribution = bound(a.TerminalFailure.Error, 768), a.TerminalFailure.ErrorClass, a.TerminalFailure.Attribution
		}
		if a.Checkpoint != nil {
			entry.SourceSHA, entry.BaseSHA, entry.CheckpointSHA = a.Checkpoint.HeadSHA, a.Checkpoint.BaseSHA, a.Checkpoint.CommitSHA
			r.SourceSHA, r.BaseSHA = entry.SourceSHA, entry.BaseSHA
		}
		if a.Finalization != nil {
			changed := a.Finalization.Changed
			entry.FinalizationChanged = &changed
			entry.WorkProduct = entry.WorkProduct || changed || a.Finalization.VersionID != ""
			if a.Finalization.HeadSHA != "" {
				entry.SourceSHA = a.Finalization.HeadSHA
			}
			if a.Finalization.BaseSHA != "" {
				entry.BaseSHA = a.Finalization.BaseSHA
			}
			r.SourceSHA, r.BaseSHA = entry.SourceSHA, entry.BaseSHA
		}
		if a.Runtime != nil && a.Runtime.Landing != nil {
			entry.LandingRefusal = a.Runtime.Landing.RefusalKind
			if entry.LandingRefusal == "nothing_to_land" && (terminal == nil || a.TerminalFailure == nil) {
				entry.Error = bound(a.Runtime.Landing.Refusal, 768)
			}
		}
		r.Attempts = append(r.Attempts, entry)
		for i, step := range a.Steps {
			receipt := Receipt{ID: fmt.Sprintf("%s:step:%d", a.AttemptID, i), Step: step.Step, BaseSHA: step.Source.BaseSHA, HeadSHA: step.Source.HeadSHA, Detail: bound(step.CompletionBody, 256)}
			if step.SecurityAudit != nil {
				receipt.Outcome = step.SecurityAudit.Verdict
				receipt.BaseSHA = step.SecurityAudit.Run.BaseSHA
				receipt.HeadSHA = step.SecurityAudit.Run.HeadSHA
			}
			if step.Validation != nil {
				receipt.Outcome = strconv.Itoa(step.Validation.ExitCode)
			}
			r.Receipts = append(r.Receipts, receipt)
		}
		if terminal != nil && a.AttemptID == terminal.AttemptID && a.Runtime != nil && a.Runtime.Validation != nil {
			validation := a.Runtime.Validation
			r.Receipts = append(r.Receipts, Receipt{ID: a.AttemptID + ":validation", Step: validation.Stage, Outcome: strconv.Itoa(validation.ExitCode), HeadSHA: validation.HeadSHA, Detail: bound(validation.Command, 256)})
		}
		if a.Runtime != nil && a.Runtime.Landing != nil {
			landing := a.Runtime.Landing
			r.Receipts = append(r.Receipts, Receipt{ID: a.AttemptID + ":landing", Step: "landing", Outcome: landing.RefusalKind, BaseSHA: landing.BaseSHA, HeadSHA: landing.HeadSHA, Detail: bound(landing.Refusal, 256)})
		}
	}
	if detail := recovery.ChangeDetail; detail != nil {
		if detail.Change.CurrentVersion != "" {
			r.AvailableSteps = append(r.AvailableSteps, "merge")
		}
		if len(detail.Versions) > 0 {
			v := detail.Versions[len(detail.Versions)-1]
			r.SourceSHA, r.BaseSHA = v.HeadSHA, v.BaseSHA
			if terminal != nil && terminalSourceChanged(recovery.Attempts[len(recovery.Attempts)-1], v) {
				r.BaseSHA, r.SourceSHA = terminalSource(recovery.Attempts[len(recovery.Attempts)-1])
			}
			r.Receipts = append(r.Receipts, Receipt{ID: v.ID, Step: "source", BaseSHA: v.BaseSHA, HeadSHA: v.HeadSHA})
			if terminal != nil && v.Validation != nil {
				validation := v.Validation
				r.Receipts = append(r.Receipts, Receipt{ID: v.ID + ":validation", Step: validation.Stage, Outcome: strconv.Itoa(validation.ExitCode), BaseSHA: v.BaseSHA, HeadSHA: validation.HeadSHA, Detail: bound(validation.Command, 256)})
			}
			if v.ID == detail.Change.CurrentVersion && v.Source != nil && v.Source.DiffSHA256 == tracker.ChangeSourceDigest(nil) {
				r.EmptyDelta = true
				if detail.Summary.Status == "reviewed" {
					r.AvailableSteps = append(r.AvailableSteps, CompleteStep)
				}
			}
		}
		for _, check := range detail.Checks {
			r.Receipts = append(r.Receipts, Receipt{ID: check.VersionID + ":" + check.CheckRunID, Step: "check", Outcome: check.Conclusion, HeadSHA: check.HeadSHA})
		}
		for _, review := range detail.Reviews {
			receipt := Receipt{ID: review.ID, Step: "review", Outcome: review.Decision, Detail: bound(review.Body, 512)}
			if review.SecurityAudit != nil {
				receipt.Step = "security_audit"
				receipt.BaseSHA = review.SecurityAudit.Run.BaseSHA
				receipt.HeadSHA = review.SecurityAudit.Run.HeadSHA
				receipt.Outcome = review.SecurityAudit.Verdict
			}
			if review.Validator != nil {
				receipt.Step = "validator"
				receipt.Detail = bound(review.Validator.Summary, 512)
			}
			if review.ImplementationReview != nil {
				receipt.Step = "implementation_review"
				receipt.Detail = bound(review.ImplementationReview.Summary, 512)
			}
			r.Receipts = append(r.Receipts, receipt)
		}
		for _, comment := range detail.Discussion {
			r.Comments = append(r.Comments, Comment{ID: comment.ID, Human: comment.Actor.Kind == "human", Body: bound(comment.Body, 384)})
		}
	}
	for _, comment := range recovery.Discussion {
		if !comment.Deleted {
			r.Comments = append(r.Comments, nativeComment(comment))
		}
	}
	if terminal != nil && len(r.Attempts) != 0 && r.Attempts[len(r.Attempts)-1].ID == terminal.AttemptID {
		attempt := r.Attempts[len(r.Attempts)-1]
		terminal.Step, terminal.Outcome = attempt.Step, attempt.Outcome
		if terminal.Step == "" {
			terminal.Step = "unknown"
		}
		if terminal.Disposition == LandedDisposition || terminal.Disposition == NoChangeDisposition {
			r.Receipts = append(r.Receipts, Receipt{ID: attempt.ID + ":disposition", Step: "completion", Outcome: terminal.Disposition, BaseSHA: attempt.BaseSHA, HeadSHA: attempt.SourceSHA})
		}
		r.EvidenceRevision = terminalAttemptRevision(attempt, r.Receipts)
		r.AvailableSteps = []string{terminal.NextStep}
		if terminal.Disposition != "" {
			r.AvailableSteps[0] = terminal.Disposition
		}
		r.Terminal = terminal
	}
	if len(r.Receipts) > MaxReceipts {
		r.Receipts = slices.Clone(r.Receipts[len(r.Receipts)-MaxReceipts:])
	}
	if len(r.Comments) > MaxComments {
		r.Comments = slices.Clone(r.Comments[len(r.Comments)-MaxComments:])
	}
	for range 12 {
		encoded, err := json.Marshal(r)
		if err != nil {
			break
		}
		if len(encoded) <= MaxInputBytes {
			break
		}
		r.Context = bound(r.Context, len(r.Context)/2)
		for i := range r.Attempts {
			r.Attempts[i].Error = bound(r.Attempts[i].Error, len(r.Attempts[i].Error)/2)
		}
		for i := range r.Receipts {
			r.Receipts[i].Detail = bound(r.Receipts[i].Detail, len(r.Receipts[i].Detail)/2)
		}
		for i := range r.Comments {
			r.Comments[i].Body = bound(r.Comments[i].Body, len(r.Comments[i].Body)/2)
		}
	}
	return r
}

func nativeComment(c tracker.NativeComment) Comment {
	return Comment{ID: c.ID, Revision: strconv.FormatInt(int64(c.Revision), 10), Human: c.Actor.Kind == "human" || c.EditedBy != nil && c.EditedBy.Kind == "human", Body: bound(c.Body, 384)}
}

func bound(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "") + " [truncated]"
}

func AttemptEvidence(attempt tracker.NativeAttempt) []EvidenceReference {
	references := []EvidenceReference{{ID: attempt.AttemptID}}
	command := func(result *gate.CommandResult) {
		if result != nil && result.ReceiptID != "" {
			references = append(references, EvidenceReference{ID: result.ReceiptID, AttemptID: attempt.AttemptID})
		}
	}
	if attempt.Runtime != nil {
		command(attempt.Runtime.Validation)
		if attempt.Runtime.Landing != nil {
			command(attempt.Runtime.Landing.Gate)
		}
	}
	for _, step := range attempt.Steps {
		command(step.Validation)
		if step.Validator != nil {
			for _, result := range step.Validator.Commands {
				command(&result)
			}
		}
		if step.PayloadAttachment != nil {
			references = append(references, EvidenceReference{ID: step.PayloadAttachment.ID})
		}
	}
	for _, evidence := range attempt.Evidence {
		references = append(references, EvidenceReference{ID: evidence.AttachmentID})
	}
	return references
}

func terminalAttemptRevision(attempt Attempt, receipts []Receipt) string {
	supplied := make([]Receipt, 0, len(receipts))
	for _, receipt := range receipts {
		if strings.HasPrefix(receipt.ID, attempt.ID+":") {
			supplied = append(supplied, receipt)
		}
	}
	raw, err := json.Marshal(struct {
		Attempt  Attempt
		Receipts []Receipt
	}{attempt, supplied})
	if err != nil {
		return ""
	}
	return artifact.Digest(raw)
}

func NativeTerminalRoute(recovery tracker.NativeRecovery, attempt tracker.NativeAttempt, policyID string, landed bool) (step, disposition string) {
	if landed {
		return "", LandedDisposition
	}
	if acceptedNoChange(recovery.Issue, attempt) {
		return "", NoChangeDisposition
	}
	if recovery.Issue.Archived || recovery.Issue.Terminal {
		return "", StoppedDisposition
	}
	detail := recovery.ChangeDetail
	if detail == nil || detail.Change.CurrentVersion == "" || policyID == "" {
		return "implement", ""
	}
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion {
			if terminalSourceChanged(attempt, version) {
				return "project_gate", ""
			}
			return nativeVersionNextStep(recovery, version, policyID), ""
		}
	}
	return "implement", ""
}

func terminalSourceChanged(attempt tracker.NativeAttempt, version tracker.ChangeVersion) bool {
	base, head := terminalSource(attempt)
	return !version.CreatedAt.After(attempt.UpdatedAt) && (base != "" && base != version.BaseSHA || head != "" && head != version.HeadSHA)
}

func terminalSource(attempt tracker.NativeAttempt) (base, head string) {
	if attempt.Checkpoint != nil {
		base, head = attempt.Checkpoint.BaseSHA, attempt.Checkpoint.HeadSHA
	}
	if f := attempt.Finalization; f != nil {
		if f.BaseSHA != "" {
			base = f.BaseSHA
		}
		if f.HeadSHA != "" {
			head = f.HeadSHA
		}
	}
	return base, head
}

func acceptedNoChange(issue tracker.NativeIssue, attempt tracker.NativeAttempt) bool {
	f := attempt.Finalization
	if !issue.Terminal || attempt.Status != "succeeded" || f == nil || !f.Settled || f.Changed ||
		f.ChangeID != "" || f.VersionID != "" || f.Files != 0 || f.Error != "" || f.VersionError != "" ||
		attempt.Runtime == nil || attempt.Runtime.Completion == nil || !attempt.Runtime.Completion.AcceptanceRecorded {
		return false
	}
	n := f.NoChange
	return n != nil && n.VerifiedSHA != "" && n.ReportedSHA != "" && n.Repository != "" && strings.TrimSpace(n.Evidence) != "" &&
		n.PolicyID != "" && n.PolicyID == attempt.PolicyID && n.VerifiedSHA == f.HeadSHA && n.VerifiedSHA == f.BaseSHA
}

func nativeVersionNextStep(recovery tracker.NativeRecovery, version tracker.ChangeVersion, policyID string) string {
	detail := recovery.ChangeDetail
	if version.PolicyID != policyID || version.BaseSHA == "" || version.HeadSHA == "" || detail.Summary.Status == "stale_policy" ||
		detail.Summary.NativeReview == "changes_requested" || detail.Summary.Checks == "failure" || humanFeedbackAfterVersion(recovery, version) {
		return "implement"
	}
	implementationReviewed, rejected := terminalReviewState(detail, version)
	if rejected {
		return "implement"
	}
	if len(recovery.Attempts) > 0 {
		latest := recovery.Attempts[len(recovery.Attempts)-1]
		if latest.Runtime != nil && latest.Runtime.Validation != nil && latest.Runtime.Validation.HeadSHA == version.HeadSHA && latest.Runtime.Validation.ExitCode != 0 {
			return "implement"
		}
	}
	if version.Validation != nil && version.Validation.HeadSHA == version.HeadSHA && version.Validation.ExitCode != 0 {
		return "implement"
	}
	if !currentSourceGate(recovery, version) {
		return "project_gate"
	}
	if detail.Summary.SecurityAudit != "passed" && version.Policy.Gates.SecurityAudit {
		return "security_audit"
	}
	if !implementationReviewed {
		return "implementation_review"
	}
	if detail.Summary.Status != "reviewed" {
		return "acceptance"
	}
	if recovery.Issue.ListChange != nil && recovery.Issue.ListChange.LandingPending {
		return "publication"
	}
	if version.Source != nil && version.Source.DiffSHA256 == tracker.ChangeSourceDigest(nil) {
		return CompleteStep
	}
	return "merge"
}

func humanFeedbackAfterVersion(recovery tracker.NativeRecovery, version tracker.ChangeVersion) bool {
	for _, comment := range recovery.Discussion {
		if !comment.Deleted && (comment.Actor.Kind == "human" || comment.EditedBy != nil && comment.EditedBy.Kind == "human") &&
			(comment.CreatedAt.After(version.CreatedAt) || comment.UpdatedAt.After(version.CreatedAt)) {
			return true
		}
	}
	for _, comment := range recovery.ChangeDetail.Discussion {
		if comment.Actor.Kind == "human" && comment.CreatedAt.After(version.CreatedAt) {
			return true
		}
	}
	return false
}

func currentSourceGate(recovery tracker.NativeRecovery, version tracker.ChangeVersion) bool {
	reviewState := tracker.NativeStepReviewState(recovery)
	reviewEpoch := tracker.NativeStepReviewEpoch(recovery)
	for i := len(recovery.Attempts) - 1; i >= 0; i-- {
		for _, receipt := range recovery.Attempts[i].Steps {
			if receipt.Step == "validation" && receipt.Source.BaseSHA == version.BaseSHA && receipt.Source.HeadSHA == version.HeadSHA &&
				receipt.PolicyID == version.PolicyID && receipt.Validation != nil {
				if receipt.ReviewState != reviewState || receipt.ReviewEpoch != reviewEpoch {
					return false
				}
				return sourceGatePassed(receipt.Validation, version.HeadSHA)
			}
		}
	}
	return sourceGatePassed(version.Validation, version.HeadSHA)
}

func sourceGatePassed(receipt *gate.CommandResult, head string) bool {
	return receipt != nil && receipt.Stage == gate.StageSourceFinalization && receipt.HeadSHA == head &&
		receipt.TreeSHA != "" && receipt.Command != "" && receipt.ExitCode == 0
}

func terminalReviewState(detail *tracker.ChangeDetail, version tracker.ChangeVersion) (implementationReviewed, rejected bool) {
	var implementation, acceptance *tracker.ChangeReview
	for i := range detail.Reviews {
		review := &detail.Reviews[i]
		if review.VersionID != version.ID {
			continue
		}
		switch {
		case review.ImplementationReview != nil:
			implementation = review
			if version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired {
				acceptance = nil
			}
		case review.Validator != nil:
			acceptance = review
		case review.SecurityAudit != nil && review.Decision == "changes_requested" && detail.Summary.SecurityAudit != "passed":
			rejected = rejected || terminalReviewRejected(review)
		}
	}
	implementationReviewed = version.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired ||
		implementation != nil && implementation.Decision == "approved" && implementation.ImplementationReview.Verdict == gate.ValidatorVerdictPass
	return implementationReviewed, rejected || terminalReviewRejected(implementation) || terminalReviewRejected(acceptance)
}

func terminalReviewRejected(review *tracker.ChangeReview) bool {
	return review != nil && review.Decision == "changes_requested" &&
		(len(review.RepairDependencies) == 0 || len(review.IndependentFailures()) == 0)
}
