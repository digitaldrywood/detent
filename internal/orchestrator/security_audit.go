package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
)

func (o *Orchestrator) securityAuditEvaluation(ctx context.Context, issue connector.Issue) securityaudit.Evaluation {
	cfg := gate.Effective(o.cfg.AutoPromote.Gate).SecurityAudit
	if !cfg.Enabled {
		return securityaudit.Evaluation{}
	}
	identity := o.securityAuditIdentity(issue)
	if identity.key.HeadSHA == "" {
		return securityaudit.Evaluation{Reason: securityaudit.ReasonMissing}
	}
	if o.securityAuditStore == nil || strings.TrimSpace(o.cfg.ServiceIdentity) == "" {
		return securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}
	}
	run, err := o.securityAuditStore.LatestSecurityAuditRun(ctx, identity.key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return securityaudit.Evaluation{Reason: securityaudit.ReasonMissing}
		}
		o.logSecurityAuditFailure(issue, "lookup_failed", err)
		return securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}
	}
	dispositions, err := o.securityAuditStore.ListSecurityAuditDispositions(ctx, run.ID)
	if err != nil {
		o.logSecurityAuditFailure(issue, "disposition_lookup_failed", err)
		return securityaudit.Evaluation{RunID: run.ID, Reason: securityaudit.ReasonFailed}
	}
	evaluation := securityaudit.Evaluate(run, dispositions, identity.key, o.cfg.ServiceIdentity, cfg.BlockOn)
	if !evaluation.Allowed && evaluation.Reason != securityaudit.ReasonUnresolvedFindings && run.Attempt < cfg.MaxAttempts {
		return securityaudit.Evaluation{RunID: run.ID, Reason: securityaudit.ReasonMissing}
	}
	return evaluation
}

func (o *Orchestrator) liveSecurityAuditEvaluation(ctx context.Context, issue connector.Issue) (connector.Issue, securityaudit.Evaluation) {
	cfg := gate.Effective(o.cfg.AutoPromote.Gate).SecurityAudit
	if !cfg.Enabled {
		return issue, securityaudit.Evaluation{}
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return issue, securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}
	}
	refreshed, err := hydrator.HydratePullRequest(ctx, issue)
	if err != nil {
		o.logSecurityAuditFailure(issue, "live_head_refresh_failed", err)
		return issue, securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}
	}
	if refreshed.PullRequest == nil || strings.TrimSpace(refreshed.PullRequest.HydrationUnavailableReason) != "" {
		return refreshed, securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}
	}
	return refreshed, o.securityAuditEvaluation(ctx, refreshed)
}

func (o *Orchestrator) startSecurityAuditStage(ctx context.Context, issue connector.Issue, now time.Time) {
	cfg := gate.Effective(o.cfg.AutoPromote.Gate).SecurityAudit
	if !cfg.Enabled || o.securityAuditStore == nil {
		return
	}
	identity := o.securityAuditIdentity(issue)
	if identity.cacheKey == "" {
		o.logSecurityAuditFailure(issue, "identity_unavailable", errors.New("current pull request base and head are required"))
		return
	}

	o.securityAuditMu.Lock()
	if _, running := o.securityAuditRuns[identity.cacheKey]; running {
		o.securityAuditMu.Unlock()
		return
	}
	o.securityAuditRuns[identity.cacheKey] = struct{}{}
	o.securityAuditWG.Add(1)
	o.securityAuditMu.Unlock()

	go func() {
		defer o.securityAuditWG.Done()
		defer func() {
			o.securityAuditMu.Lock()
			delete(o.securityAuditRuns, identity.cacheKey)
			o.securityAuditMu.Unlock()
		}()

		reader, readerOK := o.connector.(connector.SecurityAuditSnapshotReader)
		if !readerOK || o.securityAuditor == nil || strings.TrimSpace(o.cfg.ServiceIdentity) == "" {
			reason := errors.New("trusted security audit dependencies are unavailable")
			o.recordSecurityAuditFailure(ctx, issue, securityAuditSnapshotFromIssue(o.workflowMetricsProjectID(), issue), cfg.MaxAttempts, now, reason)
			return
		}

		snapshot, err := reader.SecurityAuditSnapshot(ctx, issue, securityAuditMaxDiffBytes(cfg))
		if err != nil {
			o.recordSecurityAuditFailure(ctx, issue, securityAuditSnapshotFromIssue(o.workflowMetricsProjectID(), issue), o.nextSecurityAuditAttempt(ctx, identity.key), now, err)
			return
		}
		snapshot.ProjectID = o.workflowMetricsProjectID()
		attempt := o.nextSecurityAuditAttempt(ctx, securityAuditKey(snapshot))
		execution, auditErr := o.securityAuditor.Audit(ctx, SecurityAuditRequest{
			Issue:           issue,
			Snapshot:        snapshot,
			StartedAt:       now.UTC(),
			SelectorContext: o.cfg.SelectorContext,
		})
		o.recordSecurityAuditExecution(ctx, snapshot, execution, attempt, auditErr)
	}()
}

type securityAuditStageIdentity struct {
	key      securityaudit.Key
	cacheKey string
}

func (o *Orchestrator) securityAuditIdentity(issue connector.Issue) securityAuditStageIdentity {
	key := securityaudit.Key{
		ProjectID:  o.workflowMetricsProjectID(),
		Repository: pullRequestRepository(issue),
		PRNumber:   pullRequestNumber(issue),
	}
	if issue.PullRequest != nil {
		key.BaseSHA = strings.TrimSpace(issue.PullRequest.BaseSHA)
		key.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	if key.ProjectID == "" || key.Repository == "" || key.PRNumber <= 0 || key.BaseSHA == "" || key.HeadSHA == "" {
		return securityAuditStageIdentity{key: key}
	}
	return securityAuditStageIdentity{
		key:      key,
		cacheKey: fmt.Sprintf("%s:%s:%d:%s:%s", key.ProjectID, strings.ToLower(key.Repository), key.PRNumber, key.BaseSHA, key.HeadSHA),
	}
}

func securityAuditKey(snapshot securityaudit.Snapshot) securityaudit.Key {
	return securityaudit.Key{
		ProjectID:  strings.TrimSpace(snapshot.ProjectID),
		Repository: strings.TrimSpace(snapshot.Repository),
		PRNumber:   snapshot.PRNumber,
		BaseSHA:    strings.TrimSpace(snapshot.BaseSHA),
		HeadSHA:    strings.TrimSpace(snapshot.HeadSHA),
	}
}

func securityAuditSnapshotFromIssue(projectID string, issue connector.Issue) securityaudit.Snapshot {
	snapshot := securityaudit.Snapshot{
		ProjectID:        strings.TrimSpace(projectID),
		IssueID:          strings.TrimSpace(issue.ID),
		Identifier:       strings.TrimSpace(issue.Identifier),
		IssueURL:         strings.TrimSpace(issue.URL),
		IssueTitle:       issue.Title,
		IssueDescription: issue.Description,
		Repository:       pullRequestRepository(issue),
		PRNumber:         pullRequestNumber(issue),
	}
	if issue.PullRequest != nil {
		snapshot.BaseSHA = strings.TrimSpace(issue.PullRequest.BaseSHA)
		snapshot.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	return snapshot
}

func securityAuditMaxDiffBytes(cfg gate.SecurityAuditConfig) int {
	if cfg.MaxDiffBytes == nil {
		return securityaudit.DefaultMaxDiffBytes
	}
	return *cfg.MaxDiffBytes
}

func (o *Orchestrator) nextSecurityAuditAttempt(ctx context.Context, key securityaudit.Key) int {
	if o.securityAuditStore == nil {
		return 1
	}
	run, err := o.securityAuditStore.LatestSecurityAuditRun(ctx, key)
	if err != nil || run.Attempt < 1 {
		return 1
	}
	return run.Attempt + 1
}

func (o *Orchestrator) recordSecurityAuditFailure(ctx context.Context, issue connector.Issue, snapshot securityaudit.Snapshot, attempt int, startedAt time.Time, failure error) {
	completedAt := o.clockNow().UTC()
	if startedAt.IsZero() {
		startedAt = completedAt
	}
	if attempt < 1 {
		attempt = 1
	}
	run := securityaudit.Run{
		InvocationID:       uuid.NewString(),
		ProjectID:          strings.TrimSpace(snapshot.ProjectID),
		IssueID:            strings.TrimSpace(snapshot.IssueID),
		Identifier:         strings.TrimSpace(snapshot.Identifier),
		IssueURL:           strings.TrimSpace(snapshot.IssueURL),
		Repository:         strings.TrimSpace(snapshot.Repository),
		PRNumber:           snapshot.PRNumber,
		BaseSHA:            strings.TrimSpace(snapshot.BaseSHA),
		HeadSHA:            strings.TrimSpace(snapshot.HeadSHA),
		ServiceIdentity:    strings.TrimSpace(o.cfg.ServiceIdentity),
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: securityaudit.AuthenticationRejected,
		ExitStatus:         securityaudit.ExitStatusFailed,
		Failure:            failure.Error(),
		Attempt:            attempt,
		StartedAt:          startedAt.UTC(),
		CompletedAt:        completedAt,
		RecordedAt:         completedAt,
	}
	if _, err := o.securityAuditStore.RecordSecurityAuditRun(ctx, run); err != nil {
		o.logSecurityAuditFailure(issue, "failure_persistence_failed", err)
	}
}

func (o *Orchestrator) recordSecurityAuditExecution(ctx context.Context, snapshot securityaudit.Snapshot, execution SecurityAuditExecution, attempt int, auditErr error) {
	completedAt := execution.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = o.clockNow().UTC()
	}
	startedAt := execution.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = completedAt
	}
	invocationID := strings.TrimSpace(execution.InvocationID)
	if invocationID == "" {
		invocationID = uuid.NewString()
	}
	authenticationMode := strings.TrimSpace(execution.AuthenticationMode)
	if authenticationMode == "" {
		authenticationMode = securityaudit.AuthenticationRejected
	}
	exitStatus := securityaudit.ExitStatusSuccess
	failure := ""
	if auditErr != nil {
		exitStatus = securityaudit.ExitStatusFailed
		failure = auditErr.Error()
	}
	outputDigest := ""
	if execution.Output != "" {
		outputDigest = securityaudit.OutputDigest(execution.Output)
	}
	run := securityaudit.Run{
		InvocationID:       invocationID,
		ProjectID:          strings.TrimSpace(snapshot.ProjectID),
		IssueID:            strings.TrimSpace(snapshot.IssueID),
		Identifier:         strings.TrimSpace(snapshot.Identifier),
		IssueURL:           strings.TrimSpace(snapshot.IssueURL),
		Repository:         strings.TrimSpace(snapshot.Repository),
		PRNumber:           snapshot.PRNumber,
		BaseSHA:            strings.TrimSpace(snapshot.BaseSHA),
		HeadSHA:            strings.TrimSpace(snapshot.HeadSHA),
		ServiceIdentity:    strings.TrimSpace(o.cfg.ServiceIdentity),
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: authenticationMode,
		WorkerPID:          execution.WorkerProcess.PID,
		WorkerPGID:         execution.WorkerProcess.GroupID,
		WorkerStartedAt:    execution.WorkerProcess.StartedAt.UTC(),
		ProviderThreadID:   strings.TrimSpace(execution.ProviderThreadID),
		ProviderSessionID:  strings.TrimSpace(execution.ProviderSessionID),
		ExitStatus:         exitStatus,
		Failure:            failure,
		OutputDigest:       outputDigest,
		OutputBytes:        len(execution.Output),
		Verdict:            execution.Result.Verdict,
		Summary:            execution.Result.Summary,
		Findings:           execution.Result.Findings,
		Attempt:            attempt,
		StartedAt:          startedAt,
		CompletedAt:        completedAt,
		RecordedAt:         completedAt,
	}
	if _, err := o.securityAuditStore.RecordSecurityAuditRun(ctx, run); err != nil {
		o.logSecurityAuditFailure(connector.Issue{ID: snapshot.IssueID, Identifier: snapshot.Identifier}, "persistence_failed", err)
	}
}

func (o *Orchestrator) logSecurityAuditFailure(issue connector.Issue, reason string, err error) {
	if o.logger == nil {
		return
	}
	o.logger.Warn(
		"trusted security audit failed",
		"reason", reason,
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", strings.TrimSpace(issue.Identifier),
		"error", err,
	)
}
