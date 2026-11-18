package reviewevidence

import (
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func AcceptedSecurityAudit(detail tracker.ChangeDetail, version tracker.ChangeVersion) *runner.NativeSecurityAuditEvidence {
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
