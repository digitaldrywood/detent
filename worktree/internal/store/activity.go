package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

const (
	defaultIssueActivityLimit = 50
	maxIssueActivityLimit     = 200
)

var _ ActivityStore = (*sqliteStore)(nil)

func (s *sqliteStore) ListIssueActivity(ctx context.Context, query IssueActivityQuery) ([]IssueActivityEvent, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultIssueActivityLimit
	}
	if limit > maxIssueActivityLimit {
		limit = maxIssueActivityLimit
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := s.queries.ListIssueActivityEvents(ctx, sqlc.ListIssueActivityEventsParams{
		ProjectID:      strings.TrimSpace(query.ProjectID),
		IssueID:        strings.TrimSpace(query.IssueID),
		Identifier:     strings.TrimSpace(query.Identifier),
		IssueURL:       strings.TrimSpace(query.IssueURL),
		IncludeVerbose: boolInt64(query.IncludeVerbose),
		Limit:          int64(limit),
		Offset:         int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("listing issue activity: %w", err)
	}

	events := make([]IssueActivityEvent, 0, len(rows))
	for _, row := range rows {
		at, err := parseTimestamp("event_at", row.EventAt)
		if err != nil {
			return nil, err
		}
		events = append(events, IssueActivityEvent{
			ID:            row.EventID.String,
			Source:        row.Source,
			Kind:          row.Kind,
			Name:          row.Name,
			At:            at,
			AttemptNumber: int(row.AttemptNumber),
			SessionID:     row.SessionID,
			Detail:        row.Detail,
			Reason:        row.Reason,
			Status:        row.Status,
			Model:         row.Model,
			Turns:         row.Turns,
			TotalTokens:   row.TotalTokens,
			Verbose:       row.Verbose != 0,
		})
	}
	return events, nil
}

func (s *sqliteStore) LatestIssueAgentSession(ctx context.Context, identity IssueIdentity) (IssueAgentSession, error) {
	identity = normalizeIssueIdentity(identity)
	if identity.ProjectID == "" {
		return IssueAgentSession{}, ErrProjectRequired
	}
	if identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		return IssueAgentSession{}, ErrNotFound
	}

	row, err := s.queries.GetLatestIssueAgentSession(ctx, sqlc.GetLatestIssueAgentSessionParams{
		ProjectID:  nullString(identity.ProjectID),
		IssueID:    identity.IssueID,
		Identifier: identity.Identifier,
		IssueURL:   identity.IssueURL,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IssueAgentSession{}, ErrNotFound
		}
		return IssueAgentSession{}, fmt.Errorf("reading latest issue agent session: %w", err)
	}
	completedAt, err := parseTimestamp("completed_at", row.CompletedAt)
	if err != nil {
		return IssueAgentSession{}, err
	}
	return IssueAgentSession{
		ProjectID:         strings.TrimSpace(row.ProjectID),
		DetentSessionID:   row.ID,
		ProviderThreadID:  strings.TrimSpace(row.ProviderThreadID),
		ProviderSessionID: strings.TrimSpace(row.ProviderSessionID),
		AgentBackendKind:  strings.TrimSpace(row.AgentBackendKind),
		CompletedAt:       completedAt,
	}, nil
}
