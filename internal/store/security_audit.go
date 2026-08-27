package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func (s *sqliteStore) RecordSecurityAuditRun(ctx context.Context, run securityaudit.Run) (securityaudit.Run, error) {
	if err := validateSecurityAuditRun(run); err != nil {
		return securityaudit.Run{}, err
	}
	startedAt, err := requiredTimestamp("started_at", run.StartedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	completedAt, err := requiredTimestamp("completed_at", run.CompletedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	recordedAt, err := requiredTimestamp("recorded_at", run.RecordedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	var workerStartedAt *time.Time
	if !run.WorkerStartedAt.IsZero() {
		workerStartedAt = &run.WorkerStartedAt
	}
	workerStarted, err := nullableTimestamp("worker_started_at", workerStartedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	findingsJSON, err := json.Marshal(run.Findings)
	if err != nil {
		return securityaudit.Run{}, fmt.Errorf("encoding security audit findings: %w", err)
	}

	id, err := s.queries.CreateSecurityAuditRun(ctx, sqlc.CreateSecurityAuditRunParams{
		InvocationID:       strings.TrimSpace(run.InvocationID),
		ProjectID:          strings.TrimSpace(run.ProjectID),
		IssueID:            strings.TrimSpace(run.IssueID),
		Identifier:         strings.TrimSpace(run.Identifier),
		IssueURL:           strings.TrimSpace(run.IssueURL),
		Repository:         strings.TrimSpace(run.Repository),
		PrNumber:           int64(run.PRNumber),
		BaseSha:            strings.TrimSpace(run.BaseSHA),
		HeadSha:            strings.TrimSpace(run.HeadSHA),
		ServiceIdentity:    strings.TrimSpace(run.ServiceIdentity),
		ReviewerVersion:    strings.TrimSpace(run.ReviewerVersion),
		ReviewerDigest:     strings.TrimSpace(run.ReviewerDigest),
		AuthenticationMode: strings.TrimSpace(run.AuthenticationMode),
		WorkerPid:          int64(run.WorkerPID),
		WorkerPgid:         int64(run.WorkerPGID),
		WorkerStartedAt:    workerStarted,
		ProviderThreadID:   strings.TrimSpace(run.ProviderThreadID),
		ProviderSessionID:  strings.TrimSpace(run.ProviderSessionID),
		ExitStatus:         strings.TrimSpace(run.ExitStatus),
		Failure:            strings.TrimSpace(run.Failure),
		OutputDigest:       strings.TrimSpace(run.OutputDigest),
		OutputBytes:        int64(run.OutputBytes),
		Verdict:            strings.TrimSpace(run.Verdict),
		Summary:            strings.TrimSpace(run.Summary),
		FindingsJson:       string(findingsJSON),
		Attempt:            int64(run.Attempt),
		StartedAt:          startedAt,
		CompletedAt:        completedAt,
		RecordedAt:         recordedAt,
	})
	if err != nil {
		return securityaudit.Run{}, fmt.Errorf("recording security audit run: %w", err)
	}
	run.ID = id
	return run, nil
}

func (s *sqliteStore) LatestSecurityAuditRun(ctx context.Context, key securityaudit.Key) (securityaudit.Run, error) {
	if err := validateSecurityAuditKey(key); err != nil {
		return securityaudit.Run{}, err
	}
	row, err := s.queries.LatestSecurityAuditRun(ctx, sqlc.LatestSecurityAuditRunParams{
		ProjectID:  strings.TrimSpace(key.ProjectID),
		Repository: strings.TrimSpace(key.Repository),
		PrNumber:   int64(key.PRNumber),
		BaseSha:    strings.TrimSpace(key.BaseSHA),
		HeadSha:    strings.TrimSpace(key.HeadSHA),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return securityaudit.Run{}, ErrNotFound
		}
		return securityaudit.Run{}, fmt.Errorf("reading security audit run: %w", err)
	}
	return securityAuditRunFromRow(row)
}

func (s *sqliteStore) LatestSecurityAuditRunForPullRequest(ctx context.Context, projectID, repository string, prNumber int) (securityaudit.Run, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repository) == "" || prNumber <= 0 {
		return securityaudit.Run{}, errors.New("project_id, repository, and pr_number are required")
	}
	row, err := s.queries.LatestSecurityAuditRunForPullRequest(ctx, sqlc.LatestSecurityAuditRunForPullRequestParams{
		ProjectID:  strings.TrimSpace(projectID),
		Repository: strings.TrimSpace(repository),
		PrNumber:   int64(prNumber),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return securityaudit.Run{}, ErrNotFound
		}
		return securityaudit.Run{}, fmt.Errorf("reading pull request security audit run: %w", err)
	}
	return securityAuditRunFromRow(row)
}

func (s *sqliteStore) RecordSecurityAuditDisposition(ctx context.Context, disposition securityaudit.Disposition) (securityaudit.Disposition, error) {
	if disposition.AuditRunID <= 0 || strings.TrimSpace(disposition.FindingID) == "" || strings.TrimSpace(disposition.Status) == "" || strings.TrimSpace(disposition.Evidence) == "" || strings.TrimSpace(disposition.ServiceIdentity) == "" {
		return securityaudit.Disposition{}, errors.New("audit_run_id, finding_id, status, evidence, and service_identity are required")
	}
	recordedAt, err := requiredTimestamp("recorded_at", disposition.RecordedAt)
	if err != nil {
		return securityaudit.Disposition{}, err
	}
	id, err := s.queries.CreateSecurityAuditDisposition(ctx, sqlc.CreateSecurityAuditDispositionParams{
		AuditRunID:      disposition.AuditRunID,
		FindingID:       strings.TrimSpace(disposition.FindingID),
		Status:          strings.TrimSpace(disposition.Status),
		Evidence:        strings.TrimSpace(disposition.Evidence),
		ServiceIdentity: strings.TrimSpace(disposition.ServiceIdentity),
		RecordedAt:      recordedAt,
	})
	if err != nil {
		return securityaudit.Disposition{}, fmt.Errorf("recording security audit disposition: %w", err)
	}
	disposition.ID = id
	return disposition, nil
}

func (s *sqliteStore) ListSecurityAuditDispositions(ctx context.Context, auditRunID int64) ([]securityaudit.Disposition, error) {
	if auditRunID <= 0 {
		return nil, errors.New("audit_run_id is required")
	}
	rows, err := s.queries.ListSecurityAuditDispositions(ctx, auditRunID)
	if err != nil {
		return nil, fmt.Errorf("listing security audit dispositions: %w", err)
	}
	result := make([]securityaudit.Disposition, 0, len(rows))
	for _, row := range rows {
		recordedAt, err := parseTimestamp("recorded_at", row.RecordedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, securityaudit.Disposition{
			ID:              row.ID,
			AuditRunID:      row.AuditRunID,
			FindingID:       row.FindingID,
			Status:          row.Status,
			Evidence:        row.Evidence,
			ServiceIdentity: row.ServiceIdentity,
			RecordedAt:      recordedAt.UTC(),
		})
	}
	return result, nil
}

func securityAuditRunFromRow(row sqlc.SecurityAuditRun) (securityaudit.Run, error) {
	startedAt, err := parseTimestamp("started_at", row.StartedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	completedAt, err := parseTimestamp("completed_at", row.CompletedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	recordedAt, err := parseTimestamp("recorded_at", row.RecordedAt)
	if err != nil {
		return securityaudit.Run{}, err
	}
	var workerStartedAt time.Time
	if row.WorkerStartedAt.Valid {
		workerStartedAt, err = parseTimestamp("worker_started_at", row.WorkerStartedAt.String)
		if err != nil {
			return securityaudit.Run{}, err
		}
	}
	findings := []securityaudit.Finding{}
	if err := json.Unmarshal([]byte(row.FindingsJson), &findings); err != nil {
		return securityaudit.Run{}, fmt.Errorf("decoding security audit findings: %w", err)
	}
	return securityaudit.Run{
		ID:                 row.ID,
		InvocationID:       row.InvocationID,
		ProjectID:          row.ProjectID,
		IssueID:            row.IssueID,
		Identifier:         row.Identifier,
		IssueURL:           row.IssueURL,
		Repository:         row.Repository,
		PRNumber:           int(row.PrNumber),
		BaseSHA:            row.BaseSha,
		HeadSHA:            row.HeadSha,
		ServiceIdentity:    row.ServiceIdentity,
		ReviewerVersion:    row.ReviewerVersion,
		ReviewerDigest:     row.ReviewerDigest,
		AuthenticationMode: row.AuthenticationMode,
		WorkerPID:          int(row.WorkerPid),
		WorkerPGID:         int(row.WorkerPgid),
		WorkerStartedAt:    workerStartedAt.UTC(),
		ProviderThreadID:   row.ProviderThreadID,
		ProviderSessionID:  row.ProviderSessionID,
		ExitStatus:         row.ExitStatus,
		Failure:            row.Failure,
		OutputDigest:       row.OutputDigest,
		OutputBytes:        int(row.OutputBytes),
		Verdict:            row.Verdict,
		Summary:            row.Summary,
		Findings:           findings,
		Attempt:            int(row.Attempt),
		StartedAt:          startedAt.UTC(),
		CompletedAt:        completedAt.UTC(),
		RecordedAt:         recordedAt.UTC(),
	}, nil
}

func validateSecurityAuditRun(run securityaudit.Run) error {
	required := map[string]string{
		"invocation_id":       run.InvocationID,
		"project_id":          run.ProjectID,
		"issue_id":            run.IssueID,
		"identifier":          run.Identifier,
		"issue_url":           run.IssueURL,
		"repository":          run.Repository,
		"base_sha":            run.BaseSHA,
		"head_sha":            run.HeadSHA,
		"service_identity":    run.ServiceIdentity,
		"reviewer_version":    run.ReviewerVersion,
		"reviewer_digest":     run.ReviewerDigest,
		"authentication_mode": run.AuthenticationMode,
		"exit_status":         run.ExitStatus,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if run.PRNumber <= 0 || run.Attempt <= 0 {
		return errors.New("pr_number and attempt must be positive")
	}
	return nil
}

func validateSecurityAuditKey(key securityaudit.Key) error {
	if strings.TrimSpace(key.ProjectID) == "" || strings.TrimSpace(key.Repository) == "" || key.PRNumber <= 0 || strings.TrimSpace(key.BaseSHA) == "" || strings.TrimSpace(key.HeadSHA) == "" {
		return errors.New("project_id, repository, pr_number, base_sha, and head_sha are required")
	}
	return nil
}
