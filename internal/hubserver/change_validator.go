package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateNativeValidatorReview(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, request tracker.ReviewChange) (tracker.MachineID, error) {
	worker := scope.credential.Scope == apiScopeWorker
	implementationReview := request.ImplementationReview != nil
	if implementationReview && request.Validator != nil {
		return "", nativeInvalid("implementation review and acceptance validation must be separate results")
	}
	if request.Validator == nil && !implementationReview {
		if worker {
			return "", &nativeError{status: 403, Code: "forbidden", Message: "workers may only submit validator reviews"}
		}
		return "", nil
	}
	result := request.Validator
	required := version.Policy.Gates.Validator
	if implementationReview {
		result = request.ImplementationReview
		required = version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
	}
	if !worker || !required || change.CurrentVersion != version.ID || !result.Submitted || result.SessionID <= 0 || result.VersionID != version.ID || result.Repository != version.Repository || result.BaseSHA != version.BaseSHA || result.HeadSHA != version.HeadSHA || !changerequest.ValidHash(result.DiffDigest, 64) || math.IsNaN(result.Score) || math.IsInf(result.Score, 0) || result.Score < 0 || result.Score > 1 {
		return "", nativeInvalid("validator review must identify the current immutable version and a fresh validator session")
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
	if !implementationReview {
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
		return "", nativeInvalid("validator verdict does not match the review decision")
	}
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT data_json FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND id=?", scope.organization, scope.project, change.WorkItemID, version.AttemptID).Scan(&raw)
	if err != nil {
		return "", err
	}
	var source tracker.NativeRunData
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		return "", err
	}
	if source.RunID != version.RunID || source.PolicyID != version.PolicyID || source.Identity == nil || (source.Identity.Role != "code" && source.Identity.Role != "rework") || source.Runtime == nil || source.Runtime.Activity == nil || source.Runtime.Activity.SessionID <= 0 {
		return "", nativeInvalid("validator review requires the implementing session's recorded identity")
	}
	var current tracker.NativeRunData
	if err := tx.QueryRowContext(ctx, "SELECT data_json FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND lease_id=?", scope.organization, scope.project, change.WorkItemID, request.LeaseID).Scan(&raw); err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return "", err
	}
	if current.PolicyID != version.PolicyID || current.FencingToken != request.FencingToken || current.Identity == nil || (current.Identity.Role != "code" && current.Identity.Role != "rework") || current.Runtime == nil || current.Runtime.Activity == nil || current.Runtime.Activity.SessionID <= 0 || current.Runtime.Activity.SessionID == result.SessionID || current.MachineID == source.MachineID && source.Runtime.Activity.SessionID == result.SessionID {
		return "", nativeInvalid("validator review must come from a session separate from the implementing session under its active execution authority")
	}
	reviews, err := changeRows[tracker.ChangeReview](ctx, tx, "SELECT record_json FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'review' ORDER BY sequence", change.ID, version.ID)
	if err != nil {
		return "", err
	}
	for _, review := range reviews {
		other := review.ImplementationReview
		if implementationReview {
			other = review.Validator
		}
		if other != nil && review.SessionMachineID == current.MachineID && other.SessionID == result.SessionID {
			return "", nativeInvalid("implementation review and acceptance validation require separate sessions")
		}
	}
	return current.MachineID, nil
}
