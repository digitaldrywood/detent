package hubserver

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeValidatorReview(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, request tracker.ReviewChange) (tracker.MachineID, error) {
	worker := scope.credential.Scope == apiScopeWorker
	implementationReview := request.ImplementationReview != nil
	audit := request.SecurityAudit != nil
	landingAudit := audit && request.SecurityAudit.Landing
	invalid := func(message, field string) error {
		err := nativeInvalid(message)
		if worker && request.Validator != nil && !implementationReview && !audit {
			return nativeRejection(err, "acceptance_validation", field)
		}
		return err
	}
	auditInvalid := func(message, field string) error {
		if !worker {
			return nativeInvalid(message)
		}
		return securityAuditRejection(message, field)
	}
	if implementationReview && request.Validator != nil || audit && (implementationReview || request.Validator != nil) {
		if audit {
			return "", auditInvalid("implementation review and acceptance validation must be separate results", "review")
		}
		return "", nativePublicInvalid("implementation review and acceptance validation must be separate results")
	}
	if request.Validator == nil && !implementationReview && !audit {
		if worker {
			return "", &nativeError{status: 403, Code: "forbidden", Message: "workers may only submit validator reviews", publicMessage: true}
		}
		return "", nil
	}
	result := request.Validator
	required := version.Policy.Gates.Validator
	if implementationReview {
		result = request.ImplementationReview
		required = version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
	}
	if audit {
		run := request.SecurityAudit.Run
		result = &gate.ValidatorResult{VersionID: version.ID, SessionID: request.SecurityAudit.SessionID, Submitted: true, Repository: run.Repository, BaseSHA: run.BaseSHA, HeadSHA: run.HeadSHA, DiffDigest: request.SecurityAudit.DiffDigest, Score: 1, Verdict: request.SecurityAudit.Verdict}
		required = version.Policy.Gates.SecurityAudit
	}
	if !worker || !required || change.CurrentVersion != version.ID || !result.Submitted || result.SessionID <= 0 || result.VersionID != version.ID || result.Repository != version.Repository || (!landingAudit && (result.BaseSHA != version.BaseSHA || result.HeadSHA != version.HeadSHA)) || (landingAudit && (!changerequest.ValidHash(result.BaseSHA, len(version.BaseSHA)) || !changerequest.ValidHash(result.HeadSHA, len(version.HeadSHA)) || result.BaseSHA == result.HeadSHA || version.Policy.Gates.GitHubPullRequest || request.SecurityAudit.VersionID != version.ID)) || !changerequest.ValidHash(result.DiffDigest, 64) || math.IsNaN(result.Score) || math.IsInf(result.Score, 0) || result.Score < 0 || result.Score > 1 {
		if audit {
			switch {
			case !required:
				return "", auditInvalid("security audit is not required by the version policy", "policy.security_audit")
			case result.SessionID <= 0:
				return "", auditInvalid("security audit evidence is incomplete: session_id is missing or invalid", "session_id")
			case change.CurrentVersion != version.ID || landingAudit && request.SecurityAudit.VersionID != version.ID:
				return "", auditInvalid("security audit must identify the current immutable version", "version_id")
			case result.Repository != version.Repository:
				return "", auditInvalid("security audit evidence is incomplete: run.repository differs from the immutable version", "repository")
			case !landingAudit && result.BaseSHA != version.BaseSHA:
				return "", auditInvalid("security audit evidence is incomplete: run.base_sha differs from the immutable version", "base_sha")
			case !landingAudit && result.HeadSHA != version.HeadSHA:
				return "", auditInvalid("security audit evidence is incomplete: run.head_sha differs from the immutable version", "head_sha")
			case landingAudit && version.Policy.Gates.GitHubPullRequest:
				return "", auditInvalid("landing security audit is not accepted for pull request landings", "policy.security_audit")
			case landingAudit && (!changerequest.ValidHash(result.BaseSHA, len(version.BaseSHA)) || result.BaseSHA == result.HeadSHA):
				return "", auditInvalid("landing security audit base is invalid", "base_sha")
			case landingAudit && !changerequest.ValidHash(result.HeadSHA, len(version.HeadSHA)):
				return "", auditInvalid("landing security audit head is invalid", "head_sha")
			case !changerequest.ValidHash(result.DiffDigest, 64):
				return "", auditInvalid("security audit diff digest is invalid", "diff_digest")
			default:
				return "", auditInvalid("security audit must identify the current immutable version from a worker", "review")
			}
		}
		field := "review"
		switch {
		case !required:
			field = "policy.validator"
		case change.CurrentVersion != version.ID || result.VersionID != version.ID:
			field = "validator.version_id"
		case !result.Submitted:
			field = "validator.submitted"
		case result.SessionID <= 0:
			field = "validator.session_id"
		case result.Repository != version.Repository:
			field = "validator.repository"
		case result.BaseSHA != version.BaseSHA:
			field = "validator.base_sha"
		case result.HeadSHA != version.HeadSHA:
			field = "validator.head_sha"
		case !changerequest.ValidHash(result.DiffDigest, 64):
			field = "validator.diff_digest"
		case math.IsNaN(result.Score) || math.IsInf(result.Score, 0) || result.Score < 0 || result.Score > 1:
			field = "validator.score"
		}
		return "", invalid("validator review must identify the current immutable version and a fresh validator session", field)
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
	if err != nil {
		return "", err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, version.Policy)
	if err != nil {
		return "", err
	}
	tree := ""
	if version.Validation != nil {
		tree = version.Validation.TreeSHA
	}
	contract, err := config.ResolveIssueContract(workflow.SharedPrompt)
	if err != nil {
		return "", err
	}
	if audit {
		run := request.SecurityAudit.Run
		switch {
		case run.ID <= 0:
			return "", auditInvalid("security audit evidence is incomplete: missing: run.id is missing or invalid", "run.id")
		case run.IssueID != string(change.WorkItemID):
			return "", auditInvalid("security audit identity differs from the work item: run.issue_id", "run.issue_id")
		case run.ProjectID != string(scope.project):
			return "", auditInvalid("security audit identity differs from the work item: run.project_id", "run.project_id")
		case run.ID != result.SessionID:
			return "", auditInvalid("security audit identity differs from the work item: run.id differs from session_id", "session_id")
		}
		evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: string(scope.project), Repository: version.Repository, PRNumber: run.PRNumber, BaseSHA: result.BaseSHA, HeadSHA: result.HeadSHA}, "", workflow.Config.Gate.SecurityAudit.BlockOn)
		verdict := gate.ValidatorVerdictPass
		if evaluation.Reason == securityaudit.ReasonUnresolvedFindings {
			verdict = gate.ValidatorVerdictRework
			if run.HumanAction() != "" {
				verdict = gate.ValidatorVerdictWait
			}
		} else if !evaluation.Allowed {
			return "", auditInvalid("security audit evidence is incomplete: "+evaluation.Diagnostic(), securityAuditEvaluationField(evaluation))
		}
		if result.Verdict != verdict {
			return "", auditInvalid("security audit verdict differs from the configured gate", "verdict")
		}
	}
	originalVerdict := result.Verdict
	if !implementationReview && !audit {
		gate.NormalizeCriterionEvidence(result, contract.AcceptanceCriteria(issue.Body), tree)
		gate.ApplyCriterionPolicy(workflow.Config.Gate.Validator, result)
	}
	decision := ""
	switch result.Verdict {
	case gate.ValidatorVerdictPass:
		decision = "approved"
	case gate.ValidatorVerdictRework:
		decision = "changes_requested"
	case gate.ValidatorVerdictWait:
		decision = "commented"
	}
	if decision == "" || decision != request.Decision {
		if audit {
			return "", auditInvalid("security audit verdict does not match the review decision", "decision")
		}
		field := "decision"
		if originalVerdict != result.Verdict {
			field = "validator.criteria_evidence"
		}
		return "", invalid("validator verdict does not match the review decision", field)
	}
	var raw string
	var sourceSession sql.NullInt64
	var sourceMachine sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT implementing_session_id,implementing_machine_id FROM change_versions WHERE id=? AND change_id=?`, version.ID, change.ID).Scan(&sourceSession, &sourceMachine); err != nil {
		return "", err
	}
	if !sourceSession.Valid || sourceSession.Int64 < 0 || !sourceMachine.Valid || sourceMachine.String == "" {
		if audit {
			return "", auditInvalid("security audit requires the implementing session's recorded identity", "source.identity")
		}
		return "", invalid("validator review requires the implementing session's recorded identity", "source.identity")
	}
	var current tracker.NativeRunData
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND lease_id=?", scope.organization, scope.project, change.WorkItemID, request.LeaseID).Scan(&raw); err != nil {
		return "", err
	}
	if err := decodeRunDetail(ctx, tx, raw, &current); err != nil {
		return "", err
	}
	var currentSession int64
	if current.Runtime != nil && current.Runtime.Activity != nil {
		currentSession = current.Runtime.Activity.SessionID
	}
	validSession := current.Identity != nil && (current.Identity.Role == "code" || current.Identity.Role == "rework") && currentSession != result.SessionID
	if landingAudit {
		validSession = current.Identity != nil && current.Identity.Role == "merge"
	}
	if current.FencingToken != request.FencingToken || !validSession || sourceSession.Int64 == 0 && current.AttemptID == version.AttemptID || current.MachineID == tracker.MachineID(sourceMachine.String) && sourceSession.Int64 == result.SessionID {
		if audit {
			return "", auditInvalid("security audit must come from a session separate from the implementing session under its active execution authority", "execution_session")
		}
		return "", invalid("validator review must come from a session separate from the implementing session under its active execution authority", "validator.session_id")
	}
	reviews, err := changeRows[tracker.ChangeReview](ctx, tx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'review' ORDER BY sequence", change.ID, version.ID)
	if err != nil {
		return "", err
	}
	implementationReviewed := version.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired
	for _, review := range reviews {
		if version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired && review.ImplementationReview != nil {
			implementationReviewed = review.Decision == "approved" && review.ImplementationReview.Verdict == gate.ValidatorVerdictPass
		}
		if review.SecurityAudit != nil && review.SessionMachineID == current.MachineID && review.SecurityAudit.SessionID == result.SessionID {
			if audit {
				return "", auditInvalid("security audit requires a separate session", "separate_session")
			}
			return "", invalid("security audit requires a separate session", "validator.session_id")
		}

		other := review.ImplementationReview
		if implementationReview {
			other = review.Validator
		}
		if audit && review.Validator != nil && review.SessionMachineID == current.MachineID && review.Validator.SessionID == result.SessionID {
			return "", auditInvalid("security audit requires a separate session", "separate_session")
		}
		if other != nil && review.SessionMachineID == current.MachineID && other.SessionID == result.SessionID {
			return "", invalid("implementation review and acceptance validation require separate sessions", "validator.session_id")
		}
	}
	if !implementationReview && !audit && !implementationReviewed {
		return "", invalid("acceptance validation requires a passing implementation review before acceptance", "implementation_review")
	}
	return current.MachineID, nil
}

func securityAuditRejection(message, field string) error {
	rejection := runnerauth.PublicationRejection{Category: "security_audit", Field: field}
	if !rejection.Valid() {
		rejection.Field = "review"
	}
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, Details: map[string]any{"rejection": rejection}}
}

func securityAuditEvaluationField(evaluation securityaudit.Evaluation) string {
	if evaluation.Reason == securityaudit.ReasonDiffTooLarge {
		return "run.diff_bytes"
	}
	field, _, _ := strings.Cut(evaluation.Detail, " ")
	if (runnerauth.PublicationRejection{Category: "security_audit", Field: field}).Valid() {
		return field
	}
	return "run"
}
