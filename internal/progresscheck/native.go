package progresscheck

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func NativeRequest(recovery tracker.NativeRecovery, policy, instance string) Request {
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
		entry := Attempt{ID: a.AttemptID, Outcome: outcome, Instance: string(a.MachineID)}
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
			if entry.LandingRefusal == "nothing_to_land" {
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
			r.Receipts = append(r.Receipts, Receipt{ID: v.ID, Step: "source", BaseSHA: v.BaseSHA, HeadSHA: v.HeadSHA})
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
