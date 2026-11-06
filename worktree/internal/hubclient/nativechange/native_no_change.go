package nativechange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/nochange"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) prepareNoChange(diff tracker.AttemptDiffRequest, detail tracker.ChangeDetail) bool {
	change := nochange.Prepare(diff, detail, nochange.State{Message: e.preparedMessage, Worktree: e.worktreeState, Repository: e.repository, PolicyID: e.data.PolicyID, Validation: e.validation, UncertainEffect: e.data.Handoff.UncertainForgeEffect()})
	if change == nil {
		return false
	}
	e.change = change
	return true
}

func (e *nativeExecution) noChangeVersion() tracker.ChangeVersion {
	n := e.change.NoChange
	pinned := e.scheduler.host.PinnedExecutionPolicy(string(e.claim.lease.WorkItemID))
	return tracker.ChangeVersion{ID: n.VersionID(), Policy: pinned, ChangeVersionInput: tracker.ChangeVersionInput{Repository: e.repository, BaseSHA: n.VerifiedSHA, HeadSHA: n.VerifiedSHA, MergeBaseSHA: n.VerifiedSHA, PolicyID: n.PolicyID, AttemptID: e.data.AttemptID, RunID: e.data.RunID, Validation: e.validation}}
}

func (e *nativeExecution) noChangeValidation(ctx context.Context, stage string) (runner.NativeValidation, error) {
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.data.PolicyID); err != nil {
		return runner.NativeValidation{}, err
	}
	version := e.noChangeVersion()
	if version.Policy.ID != version.PolicyID || e.lastDiff == nil || e.lastDiff.BaseSHA != version.BaseSHA || len(e.lastDiff.Files) != 0 {
		return runner.NativeValidation{}, errors.New("no-change verification source or policy changed")
	}
	var auditEvidence *runner.NativeSecurityAuditEvidence
	if audit := e.change.SecurityAudit; stage != "security_audit" && audit != nil && audit.Verdict == "pass" {
		for i, receipt := range e.data.Steps {
			if receipt.Step == "security_audit" {
				auditEvidence = &runner.NativeSecurityAuditEvidence{ReviewID: fmt.Sprintf("%s:step:%d", e.data.AttemptID, i), ProjectID: string(e.claim.source.NativeClient().ProjectID()), WorkItemID: string(e.claim.lease.WorkItemID), VersionID: version.ID, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: audit.DiffDigest, Verdict: audit.Verdict, SessionID: audit.SessionID, ReviewerVersion: audit.Run.ReviewerVersion, ReviewerDigest: audit.Run.ReviewerDigest}
			}
		}
	}
	var commands []gate.CommandResult
	for _, result := range []*gate.ValidatorResult{e.change.ImplementationReview, e.change.Validator} {
		if result != nil {
			commands = append(commands, result.Commands...)
		}
	}
	return runner.NativeValidation{SecurityAudit: auditEvidence, NoChange: e.change.NoChange, Version: &version, Diff: &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: policy.Digest([]byte("[]"))}, Evidence: e.data.Evidence, Commands: commands, Discussion: runner.NativeReviewDiscussionForVersion(e.claim.recovery.Discussion, nil, "")}, nil
}

func (e *nativeExecution) noChangeResultMatches(result gate.ValidatorResult) bool {
	v := e.noChangeVersion()
	return result.VersionID == v.ID && result.HeadSHA == v.HeadSHA && result.BaseSHA == v.BaseSHA && result.Repository == v.Repository && result.SessionID > 0 && result.Submitted
}

func (e *nativeExecution) PrepareNoChange(ctx context.Context, base string) (string, bool, error) {
	if err := e.Validate(ctx); err != nil {
		return "", false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if base == "" || e.claim.recovery.Issue.ReleaseDelivery != nil || e.data.Handoff.UncertainForgeEffect() {
		return "", false, nil
	}
	detail, err := e.claim.source.NativeClient().CurrentChange(ctx, e.claim.lease.WorkItemID)
	if err != nil {
		return "", false, err
	}
	if detail.Summary.Status != "reviewed" || detail.Change.CurrentLanding() != nil {
		return "", false, nil
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion || version.PolicyID != e.data.PolicyID || version.Source == nil || version.Source.DiffSHA256 != tracker.ChangeSourceDigest(nil) {
			continue
		}
		for _, review := range detail.Reviews {
			v := review.Validator
			if review.VersionID != version.ID || review.Decision != "approved" || review.Actor.Kind != "runner" || v == nil || !v.Submitted || v.SessionID <= 0 || v.Verdict != "pass" || v.HeadSHA != base {
				continue
			}
			evidence, err := json.Marshal(v)
			if err != nil {
				return "", false, err
			}
			e.repository = version.Repository
			e.lastDiff = &tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: base, Files: []tracker.AttemptDiffFile{}}
			e.change = &runner.NativeChange{BaseSHA: base, HeadSHA: base, NoChange: &tracker.NativeNoChangeCompletion{ReportedSHA: v.HeadSHA, VerifiedSHA: base, Repository: version.Repository, PolicyID: e.data.PolicyID, Evidence: string(evidence), Validation: version.Validation}}
			e.finalizationSource, e.finalizationAttempt = nil, ""
			e.settled = true
			message := fmt.Sprintf("No source delta remains; independently re-verify the accepted base evidence.\n\n```detent-status\nschema: 1\nstatus: no_change_needed\nfields:\n  completion_verified_sha: %s\n  completion_evidence: %q\n```", base, string(evidence))
			e.preparedMessage, e.preparedDisposition = message, runner.NativeDispositionFromMessage(message)
			return message, true, nil
		}
	}
	return "", false, nil
}
