package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/validation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
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
	if e.change != nil && e.change.NoChange != nil {
		if expected.ChangeID != "" || expected.VersionID != "" || expected.HeadSHA != e.change.HeadSHA {
			return fmt.Errorf("%w: no-change review source changed", runner.ErrNativeRecoveryRequired)
		}
		return nil
	}
	if e.change == nil || e.change.Landing != nil || e.change.ChangeID != expected.ChangeID || e.change.VersionID != expected.VersionID || e.change.HeadSHA != expected.HeadSHA || e.preparedDisposition == nil || e.preparedDisposition.Status != "complete" || e.preparedDisposition.Blockers || e.preparedDisposition.HumanAction || e.preparedDisposition.ReasonCode != "" {
		return fmt.Errorf("%w: published review source or disposition changed", runner.ErrNativeRecoveryRequired)
	}
	detail, err := e.claim.source.NativeClient().RuntimeChange(ctx, e.claim.lease.WorkItemID, expected.ChangeID)
	if err != nil {
		return &runner.NativeImplementationReviewError{Err: err}
	}
	if detail.Change.ID != expected.ChangeID || detail.Change.CurrentVersion != expected.VersionID || detail.Change.CurrentLanding() != nil || detail.Change.ProjectID != e.claim.source.NativeClient().ProjectID() || detail.Change.WorkItemID != e.claim.lease.WorkItemID {
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
	if e.change != nil && e.change.Error != "" {
		return runner.NativeValidation{}, connector.NewTrackerAvailabilityError(connector.TrackerAvailabilityScope{Connector: "hub", Operation: "native_source_publication"}, connector.TrackerAvailabilityClassTransport, errors.Join(runner.ErrNativeRecoveryRequired, ErrUnavailable))
	}
	if e.change != nil && e.change.NoChange != nil {
		return e.noChangeValidation(ctx, stage)
	}
	if e.change == nil || e.change.Landing != nil || e.change.VersionID == "" || e.lastDiff == nil {
		return runner.NativeValidation{}, nil
	}
	client := e.claim.source.NativeClient()
	return validation.PrepareVersion(ctx, client, versionAuthority{e}, validation.VersionState{Change: e.change, Diff: e.lastDiff, Data: e.data, Lease: e.claim.lease, Organization: client.OrganizationID(), Project: client.ProjectID(), SourceRequired: e.sourceRequired}, stage)
}

func (e *nativeExecution) RecordReview(ctx context.Context, result gate.ValidatorResult) error {
	if err := e.Validate(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change != nil && e.change.NoChange != nil {
		if !e.noChangeResultMatches(result) {
			return errors.New("no-change review differs from the verified source")
		}
		e.change.ImplementationReview = &result
		return nil
	}
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
	client, item := e.claim.source.NativeClient(), e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, e.change.ChangeID, result.VersionID, tracker.ReviewChange{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":implementation-review:" + strconv.FormatInt(result.SessionID, 10), LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: result.VersionID, Decision: decision, Body: body.String(), ImplementationReview: &result,
	})
	if err != nil {
		return err
	}
	e.change.ImplementationReview = &result
	e.change.Reviewed = false
	detail, err := client.RuntimeChange(ctx, item, e.change.ChangeID)
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
	if e.change != nil && e.change.NoChange != nil {
		if !e.noChangeResultMatches(result) {
			return errors.New("no-change review differs from the verified source")
		}
		e.change.Validator = &result
		return nil
	}
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
	client, item := e.claim.source.NativeClient(), e.claim.lease.WorkItemID
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
	detail, err := client.RuntimeChange(ctx, item, e.change.ChangeID)
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
		recorded := *review.Validator
		if recorded.Attribution == "" {
			recorded.Attribution = normalized.Attribution
		}
		actual, err := json.Marshal(recorded)
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
	if e.change != nil && e.change.NoChange != nil && !result.Landing {
		version := e.noChangeVersion()
		if result.VersionID != version.ID || result.Run.HeadSHA != version.HeadSHA || result.Run.BaseSHA != version.BaseSHA || result.Run.Repository != version.Repository || result.SessionID <= 0 {
			return errors.New("no-change audit differs from the verified source")
		}
		e.change.SecurityAudit = &result
		e.applySecurityAuditHumanAction(&result)
		return nil
	}
	var changeID, versionID string
	if result.Landing {
		if e.data.Identity == nil || e.data.Identity.Role != runner.RoleMerge {
			return errors.New("landing security audit requires a merge execution")
		}
		detail, err := e.claim.source.NativeClient().CurrentChange(ctx, e.claim.lease.WorkItemID)
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
	client, item := e.claim.source.NativeClient(), e.claim.lease.WorkItemID
	_, err := client.ReviewChange(ctx, item, changeID, versionID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":security-audit:" + result.Run.InvocationID, LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken}, ExpectedVersionID: versionID, Decision: decision, Body: body.String(), SecurityAudit: &result})
	if err != nil {
		var refusal *APIError
		if !result.Landing && errors.As(err, &refusal) && refusal.PermanentInvalidRequest() && refusal.Rejection != nil && refusal.Rejection.Category == "security_audit" && (refusal.Rejection.Field == "run.reviewer_version" || refusal.Rejection.Field == "run.reviewer_digest") {
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
	detail, err := client.RuntimeChange(ctx, item, e.change.ChangeID)
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
	e.preparedPublisher.Store(false)
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
	if e.change.NoChange != nil {
		n := e.change.NoChange
		var verified workspace.LandResult
		var err error
		switch {
		case n.BaseRef != "":
			verified, err = workspace.VerifyNoChangeBase(ctx, n.Repository, n.BaseRef, n.VerifiedSHA)
		case e.integrationSource != nil:
			verified, err = e.integrationSource(ctx, e.noChangeVersion(), n.VerifiedSHA)
		case e.completionOwner.Load():
			e.change.ImplementationReview, e.change.Validator, e.change.SecurityAudit = nil, nil, nil
			e.change.VersionError = "no-change verification target is unavailable; verify acceptance on the current base again"
			return *e.change, nil
		default:
			return runner.NativeChange{}, fmt.Errorf("%w: no-change completion requires current-base verification on the source-owning runner", runner.ErrValidatorInfrastructure)
		}
		if err != nil {
			var refusal *workspace.LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != workspace.LandRefusalBaseMoved {
				return runner.NativeChange{}, fmt.Errorf("%w: no-change base verification: %w", runner.ErrValidatorInfrastructure, err)
			}
			e.change.ImplementationReview, e.change.Validator, e.change.SecurityAudit = nil, nil, nil
			e.change.VersionError = refusal.Reason
			return *e.change, nil
		}
		if verified.BaseBefore != e.change.NoChange.VerifiedSHA {
			return runner.NativeChange{}, fmt.Errorf("%w: no-change source verification differs from the reviewed base", runner.ErrValidatorInfrastructure)
		}
		n.BaseRef = verified.BaseRef
		current, err := e.claim.source.NativeClient().Recovery(ctx, e.claim.lease.WorkItemID)
		if err != nil {
			return runner.NativeChange{}, fmt.Errorf("%w: no-change review state: %w", runner.ErrValidatorInfrastructure, err)
		}
		expected := tracker.NativeStepReviewState(current)
		for _, receipt := range e.data.Steps {
			if (receipt.Step == "review" || receipt.Step == "validator" || receipt.Step == "security_audit") && receipt.ReviewState != expected {
				e.change.ImplementationReview, e.change.Validator, e.change.SecurityAudit = nil, nil, nil
				e.change.VersionError = "issue criteria or human feedback changed during no-change review; verify the current evidence again"
				break
			}
		}
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
