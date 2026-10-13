package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	snapshot, err := connector.SecurityAuditSnapshot(r.projectID, req.Issue, *input.Diff, securityAuditMaxDiffBytes(cfg))
	if err != nil {
		return false, err
	}
	audit, err := r.Audit(ctx, SecurityAuditRequest{Issue: req.Issue, Snapshot: snapshot, StartedAt: r.now(), SelectorContext: req.SelectorContext, OnUsageUpdate: req.OnUsageUpdate})
	if err != nil {
		return false, err
	}
	run := audit.auditRun(snapshot)
	review := tracker.SecurityAuditReview{Identity: audit.RuntimeIdentity, SessionID: audit.SessionID, DiffDigest: input.Diff.Digest, Run: run}
	evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: snapshot.ProjectID, Repository: run.Repository, PRNumber: run.PRNumber, BaseSHA: input.Version.BaseSHA, HeadSHA: input.Version.HeadSHA}, "", cfg.BlockOn)
	if !evaluation.Allowed && evaluation.Reason != securityaudit.ReasonUnresolvedFindings {
		return false, errors.New("security audit evidence is incomplete")
	}
	review.Verdict = "pass"
	if !evaluation.Allowed {
		review.Verdict = "rework"
	}
	if err := execution.RecordSecurityAudit(ctx, review); err != nil {
		return false, err
	}
	return evaluation.Allowed, nil
}
