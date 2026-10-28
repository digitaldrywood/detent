package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) ValidatorVersion(ctx context.Context) (runner.NativeValidation, error) {
	return e.validationVersion(ctx, "validator")
}

func (e *nativeExecution) ReviewVersion(ctx context.Context) (runner.NativeValidation, error) {
	return e.validationVersion(ctx, "review")
}

func (e *nativeExecution) PrepareReview(ctx context.Context, expected tracker.NativeChangeReference) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.Landing != nil || e.change.ChangeID != expected.ChangeID || e.change.VersionID != expected.VersionID || e.change.HeadSHA != expected.HeadSHA || e.preparedDisposition == nil || e.preparedDisposition.Status != "complete" || e.preparedDisposition.Blockers || e.preparedDisposition.HumanAction || e.preparedDisposition.ReasonCode != "" {
		return fmt.Errorf("%w: published review source or disposition changed", runner.ErrNativeRecoveryRequired)
	}
	detail, err := e.claim.source.client.runtimeChange(ctx, e.claim.lease.WorkItemID, expected.ChangeID)
	if err != nil {
		return &runner.NativeImplementationReviewError{Err: err}
	}
	if detail.Change.ID != expected.ChangeID || detail.Change.CurrentVersion != expected.VersionID || detail.Change.CurrentLanding() != nil || detail.Change.ProjectID != e.claim.source.client.project || detail.Change.WorkItemID != e.claim.lease.WorkItemID {
		return fmt.Errorf("%w: published review version is no longer current", runner.ErrNativeRecoveryRequired)
	}
	for _, version := range detail.Versions {
		if version.ID != expected.VersionID {
			continue
		}
		owner := e.data
		if e.recoveredSource != nil && *e.recoveredSource == expected {
			for _, attempt := range e.claim.recovery.Attempts {
				if attempt.AttemptID == version.AttemptID {
					owner = attempt.NativeRunData
					break
				}
			}
		}
		if version.HeadSHA != expected.HeadSHA || version.PolicyID != e.data.PolicyID || version.Policy.ID != e.data.PolicyID || owner.PolicyID != e.data.PolicyID || version.AttemptID != owner.AttemptID || version.RunID != owner.RunID || e.lastDiff == nil || version.BaseSHA != e.lastDiff.BaseSHA || version.HeadSHA != e.lastDiff.HeadSHA {
			return fmt.Errorf("%w: published review source differs from its owner", runner.ErrNativeRecoveryRequired)
		}
		e.stepReviews = &detail
		return nil
	}
	return fmt.Errorf("%w: published review version is unavailable", runner.ErrNativeRecoveryRequired)
}

func (e *nativeExecution) validationVersion(ctx context.Context, stage string) (runner.NativeValidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.Landing != nil || e.change.VersionID == "" || e.lastDiff == nil {
		return runner.NativeValidation{}, nil
	}
	detail, err := e.claim.source.client.runtimeChange(ctx, e.claim.lease.WorkItemID, e.change.ChangeID)
	if err != nil {
		return runner.NativeValidation{}, err
	}
	for _, version := range detail.Versions {
		if version.ID != e.change.VersionID || version.ID != detail.Change.CurrentVersion {
			continue
		}
		if version.HeadSHA != e.lastDiff.HeadSHA || version.BaseSHA != e.lastDiff.BaseSHA {
			return runner.NativeValidation{}, errors.New("native validator version differs from the published source")
		}
		var discussion runner.NativeReviewDiscussion
		if stage == "review" {
			if version.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired {
				return runner.NativeValidation{}, nil
			}
			recovery, err := e.claim.source.client.Recovery(ctx, e.claim.lease.WorkItemID)
			if err != nil {
				return runner.NativeValidation{}, err
			}
			discussion = runner.NativeReviewDiscussionForVersion(recovery.Discussion, detail.Discussion, version.ID)
			e.stepDiscussion = recovery.Discussion
			var latest *tracker.ChangeReview
			for i := range detail.Reviews {
				review := &detail.Reviews[i]
				if review.VersionID == version.ID && review.ImplementationReview != nil && review.ImplementationReview.HeadSHA == version.HeadSHA && (latest == nil || !review.CreatedAt.Before(latest.CreatedAt)) {
					latest = review
				}
			}
			if latest != nil && latest.Decision == "approved" && !discussion.HumanFeedbackSince(latest.CreatedAt) {
				return runner.NativeValidation{}, nil
			}
		} else if stage == "security_audit" {
			if !version.Policy.Gates.SecurityAudit {
				return runner.NativeValidation{}, nil
			}
			for _, review := range detail.Reviews {
				if review.VersionID == version.ID && review.SecurityAudit != nil && (review.Decision == "changes_requested" || review.SecurityAudit.Verdict == "wait") {
					e.change.SecurityAudit = review.SecurityAudit
					e.change.Reviewed = false
					e.applySecurityAuditHumanAction(review.SecurityAudit)
					return runner.NativeValidation{}, nil
				}
			}
			for _, review := range detail.Reviews {
				if review.VersionID == version.ID && review.SecurityAudit != nil && !review.SecurityAudit.Landing && review.Decision == "approved" && review.SecurityAudit.Run.BaseSHA == version.BaseSHA && review.SecurityAudit.Run.HeadSHA == version.HeadSHA && review.SecurityAudit.Run.ReviewerVersion == securityaudit.ReviewerVersion && review.SecurityAudit.Run.ReviewerDigest == securityaudit.ReviewerDigest() {
					return runner.NativeValidation{}, nil
				}
			}
		} else if !version.Policy.Gates.Validator || detail.Summary.Status == "reviewed" {
			return runner.NativeValidation{}, nil
		}
		raw, err := json.Marshal(e.lastDiff.Files)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		diff := &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest(raw)}
		var patch strings.Builder
		complete := true
		for _, file := range e.lastDiff.Files {
			diff.Files = append(diff.Files, file.Path)
			patch.WriteString(file.Patch)
			complete = complete && !file.Truncated && !file.Denied
		}
		if complete {
			diff.Patch = patch.String()
		}
		if stage == "security_audit" && e.sourceRequired {
			if err := e.retainChangeSource(ctx, *e.lastDiff); err != nil {
				return runner.NativeValidation{}, err
			}
			if e.retainedSource == nil || e.retainedSource.Patch == "" {
				return runner.NativeValidation{}, errors.New("security audit retained source patch is unavailable")
			}
			diff.Patch = e.retainedSource.Patch
		}
		runtime, err := e.claim.source.client.RuntimeEvidence(ctx, e.claim.lease.WorkItemID, e.data.AttemptID)
		if err != nil {
			return runner.NativeValidation{}, err
		}
		if runtime.Issue.OrganizationID != e.claim.source.client.organization || runtime.Issue.ProjectID != e.claim.source.client.project || runtime.Issue.WorkItemID != e.claim.lease.WorkItemID || runtime.Attempt == nil || runtime.Attempt.AttemptID != e.data.AttemptID || runtime.Attempt.LeaseID != e.claim.lease.ID || runtime.Attempt.FencingToken != e.claim.lease.FencingToken || runtime.Attempt.PolicyID != e.data.PolicyID {
			return runner.NativeValidation{}, errors.New("native validator evidence differs from the current attempt")
		}
		var commands []gate.CommandResult
		for _, review := range detail.Reviews {
			if review.VersionID != version.ID || review.Actor.Kind != "runner" {
				continue
			}
			for _, result := range []*gate.ValidatorResult{review.ImplementationReview, review.Validator} {
				if result != nil && result.VersionID == version.ID && result.HeadSHA == version.HeadSHA && result.BaseSHA == version.BaseSHA && result.Repository == version.Repository {
					commands = append(commands, result.Commands...)
				}
			}
		}
		return runner.NativeValidation{SecurityAudit: nativeValidatorSecurityAudit(detail, version), Discussion: discussion, Version: &version, Diff: diff, Evidence: runtime.Attempt.Evidence, Commands: commands}, nil
	}
	return runner.NativeValidation{}, errors.New("native validator version is no longer current")
}

func nativeValidatorSecurityAudit(detail tracker.ChangeDetail, version tracker.ChangeVersion) *runner.NativeSecurityAuditEvidence {
	var evidence *runner.NativeSecurityAuditEvidence
	for _, review := range detail.Reviews {
		audit := review.SecurityAudit
		if review.VersionID != version.ID || audit == nil {
			continue
		}
		if review.Decision == "changes_requested" {
			return nil
		}
		run := audit.Run
		if review.ID == "" || review.Actor.Kind != "runner" || review.Decision != "approved" || audit.Verdict != "pass" || audit.Landing {
			continue
		}
		// The accepted review binds the immutable version below. A restored
		// attempt diff may have normalized or omitted patches, so its display
		// digest is not the identity of the source that this audit reviewed.
		if !changerequest.ValidHash(audit.DiffDigest, 64) || run.IssueID != string(detail.Change.WorkItemID) || audit.SessionID <= 0 || run.ID != audit.SessionID {
			continue
		}
		// The Hub already applied the version's severity policy when accepting
		// this review. Reuse the audit owner's integrity and binding checks
		// without replacing that policy with a different severity threshold.
		key := securityaudit.Key{ProjectID: string(detail.Change.ProjectID), Repository: version.Repository, PRNumber: run.PRNumber, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}
		if !securityaudit.Evaluate(run, nil, key, "", []string{"none"}).Allowed {
			continue
		}
		evidence = &runner.NativeSecurityAuditEvidence{
			ReviewID: review.ID, ProjectID: run.ProjectID, WorkItemID: run.IssueID,
			ChangeID: detail.Change.ID, VersionID: review.VersionID,
			Repository: run.Repository, BaseSHA: run.BaseSHA, HeadSHA: run.HeadSHA,
			DiffDigest: audit.DiffDigest, Verdict: audit.Verdict, SessionID: audit.SessionID,
			ReviewerVersion: run.ReviewerVersion, ReviewerDigest: run.ReviewerDigest,
		}
	}
	return evidence
}

func (e *nativeExecution) RecordReview(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.lastDiff == nil || e.change.VersionID != result.VersionID || e.lastDiff.HeadSHA != result.HeadSHA || e.lastDiff.BaseSHA != result.BaseSHA {
		return errors.New("native implementation review differs from the published source")
	}
	decision := "commented"
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Independent automated implementation review of immutable head %s in fresh session %d. This is a source review, separate from acceptance validation.\n\n%s", result.HeadSHA, result.SessionID, result.Summary)
	for _, finding := range result.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	for _, command := range result.Commands {
		fmt.Fprintf(&body, "\n\nHost command evidence: %s; head=%s tree=%s exit=%d", command.Command, command.HeadSHA, command.TreeSHA, command.ExitCode)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":implementation-review:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), ImplementationReview: &result,
	})
	if err != nil {
		return err
	}
	e.change.ImplementationReview = &result
	e.change.Reviewed = false
	detail, err := client.runtimeChange(ctx, item, e.change.ChangeID)
	if err != nil {
		if result.Verdict == gate.ValidatorVerdictRework {
			return nil
		}
		return err
	}
	e.change.Reviewed = detail.Change.CurrentVersion == result.VersionID && detail.Summary.Status == "reviewed"
	e.stepReviews = &detail
	return nil
}

func (e *nativeExecution) RecordValidator(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil || e.change.VersionID != result.VersionID {
		return &APIError{Status: 422, Code: "invalid_request", Rejection: &tracker.PublicationRejection{Category: "acceptance_validation", Field: "validator.version_id"}}
	}
	decision := "commented"
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	}
	var body strings.Builder
	body.WriteString(result.Summary)
	if len(result.Commands) > 0 {
		fmt.Fprintf(&body, "\n\nHost-observed validation command evidence: %d receipts in this review's structured validator.commands field, including command, head/tree, exit status and captured output.", len(result.Commands))
	}

	for _, finding := range result.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":validator:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), Validator: &result,
	})
	publicationErr := err
	if publicationErr != nil {
		var refusal *APIError
		if !errors.Is(publicationErr, ErrUnavailable) || ctx.Err() != nil || errors.As(publicationErr, &refusal) && refusal.Status >= 400 && refusal.Status < 500 {
			return publicationErr
		}
	}
	detail, err := client.runtimeChange(ctx, item, e.change.ChangeID)
	if err != nil {
		return errors.Join(publicationErr, err)
	}
	if publicationErr != nil && !recordedValidatorMatches(detail, result, decision, body.String(), e.claim.lease.MachineID) {
		return publicationErr
	}
	e.change.Validator = &result
	e.change.Reviewed = detail.Change.CurrentVersion == result.VersionID && detail.Summary.Status == "reviewed"
	e.stepReviews = &detail
	return nil
}

func recordedValidatorMatches(detail tracker.ChangeDetail, result gate.ValidatorResult, decision, body string, machine tracker.MachineID) bool {
	if detail.Change.CurrentVersion != result.VersionID {
		return false
	}
	expected, err := json.Marshal(result)
	if err != nil {
		return false
	}
	for _, review := range detail.Reviews {
		if review.ID == "" || review.VersionID != result.VersionID || review.Actor.Kind != "runner" || review.SessionMachineID != machine || review.Decision != decision || review.Body != body || review.Validator == nil {
			continue
		}
		var normalized gate.ValidatorResult
		if err := json.Unmarshal(expected, &normalized); err != nil {
			return false
		}
		criteria := make([]string, 0, len(review.Validator.CriteriaEvidence))
		for _, evidence := range review.Validator.CriteriaEvidence {
			criteria = append(criteria, evidence.Criterion)
		}
		tree := ""
		for _, version := range detail.Versions {
			if version.ID == result.VersionID && version.Validation != nil {
				tree = version.Validation.TreeSHA
			}
		}
		gate.NormalizeCriterionEvidence(&normalized, criteria, tree)
		gate.NormalizeAttribution(&normalized)
		canonical, err := json.Marshal(normalized)
		if err != nil {
			return false
		}
		actual, err := json.Marshal(review.Validator)
		if err == nil && bytes.Equal(canonical, actual) {
			return true
		}
	}
	return false
}

func (e *nativeExecution) SecurityAuditVersion(ctx context.Context) (runner.NativeValidation, error) {
	return e.validationVersion(ctx, "security_audit")
}

func (e *nativeExecution) RecordSecurityAudit(ctx context.Context, result tracker.SecurityAuditReview) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var changeID, versionID string
	if result.Landing {
		if e.data.Identity == nil || e.data.Identity.Role != runner.RoleMerge {
			return errors.New("landing security audit requires a merge execution")
		}
		detail, err := e.claim.source.client.currentChange(ctx, e.claim.lease.WorkItemID)
		if err != nil {
			return err
		}
		changeID, versionID = detail.Change.ID, detail.Change.CurrentVersion
		if versionID != result.VersionID {
			return errors.New("landing security audit version is no longer current")
		}
	} else {
		if e.change == nil || e.lastDiff == nil || e.lastDiff.HeadSHA != result.Run.HeadSHA || e.lastDiff.BaseSHA != result.Run.BaseSHA {
			return errors.New("security audit differs from the published source")
		}
		changeID, versionID = e.change.ChangeID, e.change.VersionID
	}
	decision := "approved"
	switch result.Verdict {
	case "rework":
		decision = "changes_requested"
	case "wait":
		decision = "commented"
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Security audit of base %s and head %s; reviewer %s (%s).\n\n%s", result.Run.BaseSHA, result.Run.HeadSHA, result.Run.ReviewerVersion, result.Run.ReviewerDigest, result.Run.Summary)
	for _, finding := range result.Run.Findings {
		fmt.Fprintf(&body, "\n\n%s %s: %s", finding.Severity, finding.Path, finding.Body)
	}
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, changeID, versionID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":security-audit:" + result.Run.InvocationID, LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken}, ExpectedVersionID: versionID, Decision: decision, Body: body.String(), SecurityAudit: &result})
	if err != nil {
		var refusal *APIError
		if !result.Landing && errors.As(err, &refusal) && refusal.permanentInvalidRequest() && refusal.Rejection != nil && refusal.Rejection.Category == "security_audit" && (refusal.Rejection.Field == "run.reviewer_version" || refusal.Rejection.Field == "run.reviewer_digest") {
			return &runner.NativeImplementationReviewError{Err: err}
		}
		return err
	}
	if result.Landing {
		if e.change != nil {
			e.change.SecurityAudit = &result
		}
		e.applySecurityAuditHumanAction(&result)
		return nil
	}
	detail, err := client.runtimeChange(ctx, item, e.change.ChangeID)
	if err != nil {
		return err
	}
	e.change.SecurityAudit = &result
	e.applySecurityAuditHumanAction(&result)
	e.change.Reviewed = detail.Change.CurrentVersion == e.change.VersionID && detail.Summary.Status == "reviewed"
	e.stepReviews = &detail
	return nil
}

func (e *nativeExecution) PrepareSourceIteration(ctx context.Context, expected tracker.NativeChangeReference) error {
	if err := e.PrepareReview(ctx, expected); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settled = false
	e.change = nil
	e.validation = nil
	e.data.Steps = nil
	e.publication = nil
	e.preparedFailure = nil
	e.preparedOutcome, e.preparedMessage = "", ""
	e.preparedDisposition = nil
	e.data.Finalization = nil
	return nil
}

func (e *nativeExecution) SourceReview(ctx context.Context) (runner.NativeChange, error) {
	if err := e.Validate(ctx); err != nil {
		return runner.NativeChange{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil {
		return runner.NativeChange{}, nil
	}
	if e.change.Error != "" && e.data.Handoff.UncertainForgeEffect() {
		return *e.change, fmt.Errorf("%w: %s", ErrUnavailable, e.change.Error)
	}
	return *e.change, nil
}

func (e *nativeExecution) applySecurityAuditHumanAction(audit *tracker.SecurityAuditReview) {
	if audit == nil || audit.Verdict != "wait" {
		return
	}
	e.preparedMessage = audit.Run.CompletionMessage(e.preparedMessage)
	e.preparedDisposition = runner.NativeDispositionFromMessage(e.preparedMessage)
}
