package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (execution SecurityAuditExecution) auditRun(snapshot securityaudit.Snapshot) securityaudit.Run {
	return securityaudit.Run{
		ID:                 execution.SessionID,
		InvocationID:       execution.InvocationID,
		ProjectID:          snapshot.ProjectID,
		IssueID:            snapshot.IssueID,
		Identifier:         snapshot.Identifier,
		IssueURL:           snapshot.IssueURL,
		Repository:         snapshot.Repository,
		PRNumber:           snapshot.PRNumber,
		BaseSHA:            snapshot.BaseSHA,
		HeadSHA:            snapshot.HeadSHA,
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: execution.AuthenticationMode,
		WorkerPID:          execution.WorkerProcess.PID,
		WorkerPGID:         execution.WorkerProcess.GroupID,
		WorkerStartedAt:    execution.WorkerProcess.StartedAt,
		ProviderThreadID:   execution.ProviderThreadID,
		ProviderSessionID:  execution.ProviderSessionID,
		ExitStatus:         securityaudit.ExitStatusSuccess,
		OutputDigest:       securityaudit.OutputDigest(execution.Output),
		OutputBytes:        len(execution.Output),
		Verdict:            execution.Result.Verdict,
		Summary:            execution.Result.Summary,
		Findings:           execution.Result.Findings,
		Attempt:            1,
		StartedAt:          execution.StartedAt,
		CompletedAt:        execution.CompletedAt,
		RecordedAt:         execution.CompletedAt,
	}
}

func (r *Runner) auditNativeChange(ctx context.Context, req RunRequest) (approved bool, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: security audit: %w", ErrValidatorInfrastructure, err)
		}
	}()

	workflow, _, _, _ := r.runtimeSnapshot()
	cfg := gateSecurityAuditConfig(workflow.Config)
	if !cfg.Enabled {
		return true, nil
	}
	execution, ok := req.Execution.(NativeSecurityAuditExecution)
	if !ok {
		return false, errors.New("native security audit producer is unavailable")
	}
	input, err := execution.SecurityAuditVersion(ctx)
	if err != nil || input.Version == nil {
		return err == nil, err
	}
	if input.Diff == nil || input.Version.BaseSHA != input.Diff.BaseSHA || input.Version.HeadSHA != input.Diff.HeadSHA || input.Version.Repository != input.Diff.Repository {
		return false, errors.New("security audit diff differs from the immutable version")
	}
	snapshot, err := connector.SecurityAuditSnapshot(string(req.Execution.Recovery().Issue.ProjectID), req.Issue, *input.Diff)
	if err != nil {
		return false, err
	}

	var publisher ValidationEvidenceExecution
	if evidence, ok := req.Execution.(ValidationEvidenceExecution); ok {
		publisher = evidence
	}
	_, err = r.Audit(ctx, SecurityAuditRequest{Issue: req.Issue, Snapshot: snapshot, StartedAt: r.now(), SelectorContext: req.SelectorContext, OnUsageUpdate: req.OnUsageUpdate, evidence: publisher,
		complete: func(ctx context.Context, audit SecurityAuditExecution) error {
			run := audit.auditRun(snapshot)
			review := tracker.SecurityAuditReview{VersionID: input.Version.ID, Identity: audit.RuntimeIdentity, SessionID: audit.SessionID, DiffDigest: input.Diff.Digest, Run: run}
			evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: snapshot.ProjectID, Repository: run.Repository, PRNumber: run.PRNumber, BaseSHA: input.Version.BaseSHA, HeadSHA: input.Version.HeadSHA}, "", cfg.BlockOn)
			if !evaluation.Allowed && evaluation.Reason != securityaudit.ReasonUnresolvedFindings {
				return fmt.Errorf("security audit evidence is incomplete: %s", evaluation.Diagnostic())
			}
			review.Verdict = "pass"
			if !evaluation.Allowed {
				review.Verdict = "rework"
			}
			if err := execution.RecordSecurityAudit(ctx, review); err != nil {
				return err
			}
			approved = evaluation.Allowed
			return nil
		}})
	return approved, err
}

func (r *Runner) auditNativeLanding(ctx context.Context, req RunRequest, target NativeLandingTarget, files workspace.FileDiffs) (review *tracker.SecurityAuditReview, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: landing security audit: %w", ErrValidatorInfrastructure, err)
		}
	}()
	execution, ok := req.Execution.(NativeSecurityAuditExecution)
	if !ok {
		return nil, errors.New("native landing security audit producer is unavailable")
	}
	workflow, _, _, _ := r.runtimeSnapshot()
	cfg := gateSecurityAuditConfig(workflow.Config)
	raw, err := json.Marshal(files.Files)
	if err != nil {
		return nil, err
	}
	diff := connector.ValidationDiff{Repository: target.Repository, BaseSHA: files.BaseSHA, HeadSHA: files.HeadSHA, Digest: policy.Digest(raw)}
	var patch strings.Builder
	for _, file := range files.Files {
		diff.Files = append(diff.Files, file.Path)
		patch.WriteString(file.Patch)
	}
	diff.Patch = patch.String()
	issue := req.Issue
	issue.PullRequest = nil
	snapshot, err := connector.SecurityAuditSnapshot(string(req.Execution.Recovery().Issue.ProjectID), issue, diff)
	if err != nil {
		return nil, err
	}
	snapshot.DiffTruncated = snapshot.DiffTruncated || files.Truncated

	var publisher ValidationEvidenceExecution
	if evidence, ok := req.Execution.(ValidationEvidenceExecution); ok {
		publisher = evidence
	}
	_, err = r.Audit(ctx, SecurityAuditRequest{Issue: issue, Snapshot: snapshot, StartedAt: r.now(), SelectorContext: req.SelectorContext, OnUsageUpdate: req.OnUsageUpdate, evidence: publisher,
		complete: func(ctx context.Context, audit SecurityAuditExecution) error {
			run := audit.auditRun(snapshot)
			evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: snapshot.ProjectID, Repository: target.Repository, BaseSHA: files.BaseSHA, HeadSHA: files.HeadSHA}, "", cfg.BlockOn)
			if !evaluation.Allowed && evaluation.Reason != securityaudit.ReasonUnresolvedFindings {
				return fmt.Errorf("landing security audit evidence is incomplete: %s", evaluation.Diagnostic())
			}
			result := &tracker.SecurityAuditReview{VersionID: target.VersionID, Landing: true, Identity: audit.RuntimeIdentity, SessionID: audit.SessionID, DiffDigest: diff.Digest, Run: run, Verdict: "pass"}
			if !evaluation.Allowed {
				result.Verdict = "rework"
			}
			if err := execution.RecordSecurityAudit(ctx, *result); err != nil {
				return err
			}
			review = result
			return nil
		}})
	return review, err
}
