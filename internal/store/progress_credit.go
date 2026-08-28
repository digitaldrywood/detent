package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *sqliteStore) CreditIssueProgress(ctx context.Context, identity IssueIdentity, at time.Time) (IssueProgressCredit, error) {
	identity = normalizeParkIdentity(identity)
	if identity.ProjectID == "" {
		return IssueProgressCredit{}, ErrProjectRequired
	}
	key := parkIssueKey(identity.IssueID, identity.Identifier, identity.IssueURL)
	if key == "" {
		return IssueProgressCredit{}, errors.New("issue identity is required")
	}
	creditedAt, err := requiredTimestamp("credited_at", at)
	if err != nil {
		return IssueProgressCredit{}, err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO issue_progress_credits (
  project_id, issue_key, issue_id, identifier, issue_url, credited_at
) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)
ON CONFLICT(project_id, issue_key) DO UPDATE SET
  issue_id = excluded.issue_id,
  identifier = excluded.identifier,
  issue_url = excluded.issue_url,
  credited_at = excluded.credited_at
`, identity.ProjectID, key, identity.IssueID, identity.Identifier, identity.IssueURL, creditedAt)
	if err != nil {
		return IssueProgressCredit{}, fmt.Errorf("crediting issue progress: %w", err)
	}
	return IssueProgressCredit{
		ProjectID:  identity.ProjectID,
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
		CreditedAt: at.UTC(),
	}, nil
}

func (s *sqliteStore) IssueProgressCredit(ctx context.Context, identity IssueIdentity) (IssueProgressCredit, error) {
	identity = normalizeParkIdentity(identity)
	if identity.ProjectID == "" {
		return IssueProgressCredit{}, ErrProjectRequired
	}
	if parkIssueKey(identity.IssueID, identity.Identifier, identity.IssueURL) == "" {
		return IssueProgressCredit{}, errors.New("issue identity is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT project_id, COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''), credited_at
FROM issue_progress_credits
WHERE project_id = ?
  AND (
    (? != '' AND COALESCE(issue_id, '') = ?)
    OR (? != '' AND COALESCE(identifier, '') = ?)
    OR (? != '' AND COALESCE(issue_url, '') = ?)
  )
ORDER BY credited_at DESC
LIMIT 1
`, identity.ProjectID, identity.IssueID, identity.IssueID, identity.Identifier, identity.Identifier, identity.IssueURL, identity.IssueURL)
	var credit IssueProgressCredit
	var creditedAt string
	if err := row.Scan(&credit.ProjectID, &credit.IssueID, &credit.Identifier, &credit.IssueURL, &creditedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IssueProgressCredit{}, ErrNotFound
		}
		return IssueProgressCredit{}, fmt.Errorf("reading issue progress credit: %w", err)
	}
	parsed, err := parseTimestamp("credited_at", strings.TrimSpace(creditedAt))
	if err != nil {
		return IssueProgressCredit{}, err
	}
	credit.CreditedAt = parsed.UTC()
	return credit, nil
}
