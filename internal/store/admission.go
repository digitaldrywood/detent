package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
)

func (s *sqliteStore) CreateAdmissionProposal(ctx context.Context, proposal admissionmodel.Proposal) (bool, error) {
	if err := validateAdmissionProposal(proposal); err != nil {
		return false, err
	}
	findingsJSON, err := json.Marshal(proposal.Findings)
	if err != nil {
		return false, fmt.Errorf("encoding backlog admission findings: %w", err)
	}
	createdAt, err := requiredTimestamp("created_at", proposal.CreatedAt)
	if err != nil {
		return false, err
	}
	expiresAt, err := requiredTimestamp("expires_at", proposal.ExpiresAt)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin backlog admission proposal: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var existing string
	err = tx.QueryRowContext(ctx, `
SELECT id
FROM backlog_admission_proposals
WHERE project_id = ? AND issue_id = ? AND target_state = ? AND fingerprint = ? AND status = 'open'
LIMIT 1`,
		strings.TrimSpace(proposal.ProjectID),
		strings.TrimSpace(proposal.IssueID),
		strings.TrimSpace(proposal.TargetState),
		strings.TrimSpace(proposal.Fingerprint),
	).Scan(&existing)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check backlog admission proposal idempotency: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET status = 'superseded', resolved_at = ?
WHERE project_id = ? AND issue_id = ? AND target_state = ? AND status = 'open'`,
		createdAt,
		strings.TrimSpace(proposal.ProjectID),
		strings.TrimSpace(proposal.IssueID),
		strings.TrimSpace(proposal.TargetState),
	); err != nil {
		return false, fmt.Errorf("supersede backlog admission proposals: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO backlog_admission_proposals (
  id, project_id, issue_id, issue_identifier, issue_url, target_state, fingerprint,
  criteria_section, criteria_text, findings_json, confidence, status, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?)`,
		strings.TrimSpace(proposal.ID),
		strings.TrimSpace(proposal.ProjectID),
		strings.TrimSpace(proposal.IssueID),
		strings.TrimSpace(proposal.IssueIdentifier),
		strings.TrimSpace(proposal.IssueURL),
		strings.TrimSpace(proposal.TargetState),
		strings.TrimSpace(proposal.Fingerprint),
		strings.TrimSpace(proposal.CriteriaSection),
		proposal.CriteriaText,
		string(findingsJSON),
		proposal.Confidence,
		createdAt,
		expiresAt,
	); err != nil {
		return false, fmt.Errorf("create backlog admission proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit backlog admission proposal: %w", err)
	}
	committed = true
	return true, nil
}

func (s *sqliteStore) OpenAdmissionProposals(ctx context.Context, projectID string, limit int) ([]admissionmodel.Proposal, error) {
	query := `
SELECT id, project_id, issue_id, issue_identifier, issue_url, target_state, fingerprint,
       criteria_section, criteria_text, findings_json, confidence, status, created_at,
       expires_at, COALESCE(resolved_at, ''), COALESCE(commented_at, '')
FROM backlog_admission_proposals
WHERE project_id = ? AND status = 'open'
ORDER BY created_at, id`
	args := []any{strings.TrimSpace(projectID)}
	if limit > 0 {
		query += "\nLIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read open backlog admission proposals: %w", err)
	}
	defer rows.Close()
	return scanAdmissionProposals(rows)
}

func (s *sqliteStore) AdmissionProposalHistory(ctx context.Context, projectID string, issueID string) ([]admissionmodel.Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, project_id, issue_id, issue_identifier, issue_url, target_state, fingerprint,
       criteria_section, criteria_text, findings_json, confidence, status, created_at,
       expires_at, COALESCE(resolved_at, ''), COALESCE(commented_at, '')
FROM backlog_admission_proposals
WHERE project_id = ? AND issue_id = ?
ORDER BY created_at DESC, id DESC`,
		strings.TrimSpace(projectID),
		strings.TrimSpace(issueID),
	)
	if err != nil {
		return nil, fmt.Errorf("read backlog admission proposal history: %w", err)
	}
	defer rows.Close()
	return scanAdmissionProposals(rows)
}

func (s *sqliteStore) CountOpenAdmissionProposals(ctx context.Context, projectID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM backlog_admission_proposals
WHERE project_id = ? AND status = 'open'`, strings.TrimSpace(projectID)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count open backlog admission proposals: %w", err)
	}
	return count, nil
}

func (s *sqliteStore) ExpireAdmissionProposals(ctx context.Context, projectID string, at time.Time) (int, error) {
	resolvedAt, err := requiredTimestamp("resolved_at", at)
	if err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET status = 'expired', resolved_at = ?
WHERE project_id = ? AND status = 'open' AND expires_at <= ?`,
		resolvedAt,
		strings.TrimSpace(projectID),
		resolvedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("expire backlog admission proposals: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read expired backlog admission proposal count: %w", err)
	}
	return int(count), nil
}

func (s *sqliteStore) TransitionAdmissionProposal(
	ctx context.Context,
	id string,
	from admissionmodel.ProposalStatus,
	to admissionmodel.ProposalStatus,
	at time.Time,
) error {
	if !validAdmissionProposalStatus(from) || !validAdmissionProposalStatus(to) || from == to {
		return errors.New("invalid backlog admission proposal transition")
	}
	resolvedAt, err := requiredTimestamp("resolved_at", at)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET status = ?, resolved_at = ?
WHERE id = ? AND status = ?`,
		string(to),
		resolvedAt,
		strings.TrimSpace(id),
		string(from),
	)
	if err != nil {
		return fmt.Errorf("transition backlog admission proposal: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read backlog admission proposal transition count: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) MarkAdmissionProposalCommented(ctx context.Context, id string, at time.Time) error {
	commentedAt, err := requiredTimestamp("commented_at", at)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET commented_at = ?
WHERE id = ? AND status = 'open'`,
		commentedAt,
		strings.TrimSpace(id),
	)
	if err != nil {
		return fmt.Errorf("mark backlog admission proposal commented: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read backlog admission proposal comment count: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) RecordAdmissionRun(ctx context.Context, record admissionmodel.RunRecord) error {
	scheduledFor, err := requiredTimestamp("scheduled_for", record.ScheduledFor)
	if err != nil {
		return err
	}
	startedAt, err := requiredTimestamp("started_at", record.StartedAt)
	if err != nil {
		return err
	}
	completedAt, err := requiredTimestamp("completed_at", record.CompletedAt)
	if err != nil {
		return err
	}
	skippedJSON, err := admissionJSON(record.Skipped, map[string]int{})
	if err != nil {
		return fmt.Errorf("encoding backlog admission skipped counts: %w", err)
	}
	truncatedJSON, err := admissionJSON(record.Truncated, map[string]int{})
	if err != nil {
		return fmt.Errorf("encoding backlog admission truncation counts: %w", err)
	}
	issuesJSON, err := admissionJSON(record.Issues, []admissionmodel.IssueRecord{})
	if err != nil {
		return fmt.Errorf("encoding backlog admission issues: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO backlog_admission_runs (
  project_id, scheduled_for, started_at, completed_at, outcome, deferred_reason,
  candidates_found_count, candidates_count, proposed_count, skipped_json,
  truncated_json, issues_json, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.ProjectID),
		scheduledFor,
		startedAt,
		completedAt,
		strings.TrimSpace(record.Outcome),
		nullString(record.DeferredReason),
		nonNegative(int64(record.CandidatesFound)),
		nonNegative(int64(record.Candidates)),
		nonNegative(int64(record.Proposed)),
		skippedJSON,
		truncatedJSON,
		issuesJSON,
		nullString(record.Error),
	)
	if err != nil {
		return fmt.Errorf("record backlog admission run: %w", err)
	}
	return nil
}

func (s *sqliteStore) LatestAdmissionRun(ctx context.Context, projectID string) (admissionmodel.RunRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT project_id, scheduled_for, started_at, completed_at, outcome,
       COALESCE(deferred_reason, ''), candidates_found_count, candidates_count,
       proposed_count, skipped_json, truncated_json, issues_json, COALESCE(error, '')
FROM backlog_admission_runs
WHERE project_id = ?
ORDER BY completed_at DESC, id DESC
LIMIT 1`, strings.TrimSpace(projectID))
	record, err := scanAdmissionRun(row.Scan)
	if errors.Is(err, ErrNotFound) {
		return admissionmodel.RunRecord{}, false, nil
	}
	if err != nil {
		return admissionmodel.RunRecord{}, false, fmt.Errorf("read latest backlog admission run: %w", err)
	}
	return record, true, nil
}

func (s *sqliteStore) RecentAdmissionRuns(ctx context.Context, projectID string, limit int) ([]admissionmodel.RunRecord, error) {
	if limit <= 0 {
		return []admissionmodel.RunRecord{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT project_id, scheduled_for, started_at, completed_at, outcome,
       COALESCE(deferred_reason, ''), candidates_found_count, candidates_count,
       proposed_count, skipped_json, truncated_json, issues_json, COALESCE(error, '')
FROM backlog_admission_runs
WHERE project_id = ?
ORDER BY completed_at DESC, id DESC
LIMIT ?`, strings.TrimSpace(projectID), limit)
	if err != nil {
		return nil, fmt.Errorf("read recent backlog admission runs: %w", err)
	}
	defer rows.Close()
	records := []admissionmodel.RunRecord{}
	for rows.Next() {
		record, err := scanAdmissionRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func validateAdmissionProposal(proposal admissionmodel.Proposal) error {
	switch {
	case strings.TrimSpace(proposal.ID) == "":
		return errors.New("backlog admission proposal id is required")
	case strings.TrimSpace(proposal.ProjectID) == "":
		return errors.New("backlog admission proposal project id is required")
	case strings.TrimSpace(proposal.IssueID) == "":
		return errors.New("backlog admission proposal issue id is required")
	case strings.TrimSpace(proposal.TargetState) == "":
		return errors.New("backlog admission proposal target state is required")
	case strings.TrimSpace(proposal.Fingerprint) == "":
		return errors.New("backlog admission proposal fingerprint is required")
	case strings.TrimSpace(proposal.CriteriaSection) == "":
		return errors.New("backlog admission proposal criteria section is required")
	case strings.TrimSpace(proposal.CriteriaText) == "":
		return errors.New("backlog admission proposal criteria text is required")
	case len(proposal.Findings) == 0:
		return errors.New("backlog admission proposal findings are required")
	case proposal.Confidence < 0 || proposal.Confidence > 1:
		return errors.New("backlog admission proposal confidence must be between zero and one")
	case proposal.CreatedAt.IsZero():
		return errors.New("backlog admission proposal created at is required")
	case !proposal.ExpiresAt.After(proposal.CreatedAt):
		return errors.New("backlog admission proposal expiry must be after creation")
	}
	return nil
}

func validAdmissionProposalStatus(status admissionmodel.ProposalStatus) bool {
	switch status {
	case admissionmodel.ProposalOpen,
		admissionmodel.ProposalAccepted,
		admissionmodel.ProposalRejected,
		admissionmodel.ProposalExpired,
		admissionmodel.ProposalSuperseded:
		return true
	default:
		return false
	}
}

type admissionRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanAdmissionProposals(rows admissionRows) ([]admissionmodel.Proposal, error) {
	proposals := []admissionmodel.Proposal{}
	for rows.Next() {
		proposal, err := scanAdmissionProposal(rows.Scan)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return proposals, nil
}

type admissionScan func(...any) error

func scanAdmissionProposal(scan admissionScan) (admissionmodel.Proposal, error) {
	var proposal admissionmodel.Proposal
	var findingsJSON string
	var status string
	var createdAt string
	var expiresAt string
	var resolvedAt string
	var commentedAt string
	if err := scan(
		&proposal.ID,
		&proposal.ProjectID,
		&proposal.IssueID,
		&proposal.IssueIdentifier,
		&proposal.IssueURL,
		&proposal.TargetState,
		&proposal.Fingerprint,
		&proposal.CriteriaSection,
		&proposal.CriteriaText,
		&findingsJSON,
		&proposal.Confidence,
		&status,
		&createdAt,
		&expiresAt,
		&resolvedAt,
		&commentedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return admissionmodel.Proposal{}, ErrNotFound
		}
		return admissionmodel.Proposal{}, err
	}
	proposal.Status = admissionmodel.ProposalStatus(status)
	if err := json.Unmarshal([]byte(findingsJSON), &proposal.Findings); err != nil {
		return admissionmodel.Proposal{}, fmt.Errorf("decoding backlog admission findings: %w", err)
	}
	var err error
	if proposal.CreatedAt, err = parseTimestamp("created_at", createdAt); err != nil {
		return admissionmodel.Proposal{}, err
	}
	if proposal.ExpiresAt, err = parseTimestamp("expires_at", expiresAt); err != nil {
		return admissionmodel.Proposal{}, err
	}
	if proposal.ResolvedAt, err = parseAdmissionOptionalTimestamp("resolved_at", resolvedAt); err != nil {
		return admissionmodel.Proposal{}, err
	}
	if proposal.CommentedAt, err = parseAdmissionOptionalTimestamp("commented_at", commentedAt); err != nil {
		return admissionmodel.Proposal{}, err
	}
	return proposal, nil
}

func scanAdmissionRun(scan admissionScan) (admissionmodel.RunRecord, error) {
	var record admissionmodel.RunRecord
	var scheduledFor string
	var startedAt string
	var completedAt string
	var skippedJSON string
	var truncatedJSON string
	var issuesJSON string
	if err := scan(
		&record.ProjectID,
		&scheduledFor,
		&startedAt,
		&completedAt,
		&record.Outcome,
		&record.DeferredReason,
		&record.CandidatesFound,
		&record.Candidates,
		&record.Proposed,
		&skippedJSON,
		&truncatedJSON,
		&issuesJSON,
		&record.Error,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return admissionmodel.RunRecord{}, ErrNotFound
		}
		return admissionmodel.RunRecord{}, err
	}
	var err error
	if record.ScheduledFor, err = parseTimestamp("scheduled_for", scheduledFor); err != nil {
		return admissionmodel.RunRecord{}, err
	}
	if record.StartedAt, err = parseTimestamp("started_at", startedAt); err != nil {
		return admissionmodel.RunRecord{}, err
	}
	if record.CompletedAt, err = parseTimestamp("completed_at", completedAt); err != nil {
		return admissionmodel.RunRecord{}, err
	}
	if err := json.Unmarshal([]byte(skippedJSON), &record.Skipped); err != nil {
		return admissionmodel.RunRecord{}, fmt.Errorf("decoding backlog admission skipped counts: %w", err)
	}
	if err := json.Unmarshal([]byte(truncatedJSON), &record.Truncated); err != nil {
		return admissionmodel.RunRecord{}, fmt.Errorf("decoding backlog admission truncation counts: %w", err)
	}
	if err := json.Unmarshal([]byte(issuesJSON), &record.Issues); err != nil {
		return admissionmodel.RunRecord{}, fmt.Errorf("decoding backlog admission issues: %w", err)
	}
	return record, nil
}

func parseAdmissionOptionalTimestamp(name string, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	return parseTimestamp(name, value)
}

func admissionJSON[T any](value T, fallback T) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if string(raw) == "null" {
		raw, err = json.Marshal(fallback)
	}
	return string(raw), err
}
