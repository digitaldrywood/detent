package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type mergeRequiredCheckRow struct {
	repository                string
	prNumber                  int
	headSHA                   string
	checkName                 string
	requiredChecksFingerprint string
	consecutiveMissing        int
}

func (s *sqliteStore) EvaluateMergeRequiredChecks(ctx context.Context, evaluation MergeRequiredCheckEvaluation) ([]MergeRequiredCheckStreak, error) {
	projectID := strings.TrimSpace(evaluation.ProjectID)
	if projectID == "" {
		return nil, errors.New("project_id is required")
	}
	issueID := strings.TrimSpace(evaluation.IssueID)
	if issueID == "" {
		return nil, errors.New("issue_id is required")
	}
	if evaluation.PRNumber <= 0 {
		return nil, errors.New("pr_number must be greater than zero")
	}
	headSHA := strings.TrimSpace(evaluation.HeadSHA)
	if headSHA == "" {
		return nil, errors.New("head_sha is required")
	}
	fingerprint := strings.TrimSpace(evaluation.RequiredChecksFingerprint)
	if fingerprint == "" {
		return nil, errors.New("required_checks_fingerprint is required")
	}
	evaluatedAt, err := requiredTimestamp("evaluated_at", evaluation.EvaluatedAt)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin merge required check evaluation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	previous, err := mergeRequiredCheckRows(ctx, tx, projectID, issueID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM merge_required_check_streaks
WHERE project_id = ? AND issue_id = ?`, projectID, issueID); err != nil {
		return nil, fmt.Errorf("reset merge required check streaks: %w", err)
	}

	repository := strings.TrimSpace(evaluation.Repository)
	missingChecks := normalizedMergeRequiredCheckNames(evaluation.MissingChecks)
	streaks := make([]MergeRequiredCheckStreak, 0, len(missingChecks))
	for _, checkName := range missingChecks {
		count := 1
		if row, ok := previous[checkName]; ok &&
			row.repository == repository &&
			row.prNumber == evaluation.PRNumber &&
			row.headSHA == headSHA &&
			row.requiredChecksFingerprint == fingerprint {
			count = row.consecutiveMissing + 1
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO merge_required_check_streaks (
  project_id, issue_id, repository, pr_number, head_sha, check_name,
  required_checks_fingerprint, consecutive_missing, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectID,
			issueID,
			repository,
			evaluation.PRNumber,
			headSHA,
			checkName,
			fingerprint,
			count,
			evaluatedAt,
		); err != nil {
			return nil, fmt.Errorf("record merge required check streak: %w", err)
		}
		streaks = append(streaks, MergeRequiredCheckStreak{
			CheckName:          checkName,
			ConsecutiveMissing: count,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit merge required check evaluation: %w", err)
	}
	committed = true
	return streaks, nil
}

func (s *sqliteStore) ClearMergeRequiredCheckStreaks(ctx context.Context, projectID string, issueID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return errors.New("project_id is required")
	}
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return errors.New("issue_id is required")
	}
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM merge_required_check_streaks
WHERE project_id = ? AND issue_id = ?`, projectID, issueID); err != nil {
		return fmt.Errorf("clear merge required check streaks: %w", err)
	}
	return nil
}

func mergeRequiredCheckRows(ctx context.Context, tx *sql.Tx, projectID string, issueID string) (map[string]mergeRequiredCheckRow, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT repository, pr_number, head_sha, check_name, required_checks_fingerprint, consecutive_missing
FROM merge_required_check_streaks
WHERE project_id = ? AND issue_id = ?`, projectID, issueID)
	if err != nil {
		return nil, fmt.Errorf("read merge required check streaks: %w", err)
	}
	defer rows.Close()

	streaks := map[string]mergeRequiredCheckRow{}
	for rows.Next() {
		var row mergeRequiredCheckRow
		if err := rows.Scan(
			&row.repository,
			&row.prNumber,
			&row.headSHA,
			&row.checkName,
			&row.requiredChecksFingerprint,
			&row.consecutiveMissing,
		); err != nil {
			return nil, fmt.Errorf("scan merge required check streak: %w", err)
		}
		streaks[row.checkName] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read merge required check streak rows: %w", err)
	}
	return streaks, nil
}

func normalizedMergeRequiredCheckNames(names []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
