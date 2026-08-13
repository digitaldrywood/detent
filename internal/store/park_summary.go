package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ParkSummary struct {
	ProjectID                string
	IssueID                  string
	Identifier               string
	IssueURL                 string
	AttemptCount             int64
	ParkCount                int64
	AcknowledgedParkSequence int64
	AcknowledgedAt           *time.Time
	Causes                   []ParkCauseSummary
	Tokens                   ParkTokenTotals
}

type ParkCauseSummary struct {
	Cause   string
	Count   int64
	FirstAt time.Time
	LastAt  time.Time
}

type ParkTokenTotals struct {
	InputTokens           int64
	CachedInputTokens     int64
	OutputTokens          int64
	ReasoningOutputTokens int64
}

func (s ParkSummary) ReviewRecommended(threshold int) bool {
	return threshold > 0 && s.ParkCount >= int64(threshold) && s.ParkCount > s.AcknowledgedParkSequence
}

type ParkSummaryQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type parkEvent struct {
	kind       string
	id         int64
	projectID  string
	issueID    string
	identifier string
	issueURL   string
	cause      string
	at         time.Time
}

func (s *sqliteStore) IssueParkSummary(ctx context.Context, identity IssueIdentity) (ParkSummary, error) {
	summaries, err := QueryParkSummaries(ctx, s.db, identity.ProjectID)
	if err != nil {
		return ParkSummary{}, err
	}
	for _, summary := range summaries {
		if parkIdentityMatches(identity, summary) {
			return summary, nil
		}
	}
	return ParkSummary{}, ErrNotFound
}

func (s *sqliteStore) ListIssueParkSummaries(ctx context.Context, projectID string) ([]ParkSummary, error) {
	return QueryParkSummaries(ctx, s.db, projectID)
}

func (s *sqliteStore) AcknowledgeIssueParks(ctx context.Context, identity IssueIdentity, parkSequence int64, at time.Time) error {
	identity = normalizeParkIdentity(identity)
	if identity.ProjectID == "" {
		return ErrProjectRequired
	}
	key := parkIssueKey(identity.IssueID, identity.Identifier, identity.IssueURL)
	if key == "" {
		return errors.New("issue identity is required")
	}
	acknowledgedAt, err := requiredTimestamp("acknowledged_at", at)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO issue_park_acknowledgements (
  project_id, issue_key, issue_id, identifier, issue_url, park_sequence, acknowledged_at
) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)
ON CONFLICT(project_id, issue_key) DO UPDATE SET
  issue_id = excluded.issue_id,
  identifier = excluded.identifier,
  issue_url = excluded.issue_url,
  park_sequence = excluded.park_sequence,
  acknowledged_at = excluded.acknowledged_at
`, identity.ProjectID, key, identity.IssueID, identity.Identifier, identity.IssueURL, max(parkSequence, 0), acknowledgedAt)
	if err != nil {
		return fmt.Errorf("acknowledging issue parks: %w", err)
	}
	return nil
}

func QueryParkSummaries(ctx context.Context, db ParkSummaryQuerier, projectID string) ([]ParkSummary, error) {
	projectID = strings.TrimSpace(projectID)
	summaries := map[string]*ParkSummary{}
	events, err := queryParkEvents(ctx, db, projectID)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		summary := parkSummaryFor(summaries, event.projectID, event.issueID, event.identifier, event.issueURL)
		if event.kind == "attempt" {
			summary.AttemptCount++
		}
		if event.cause != "" {
			addParkCause(summary, event.cause, event.at)
		}
	}
	if err := queryParkUsage(ctx, db, projectID, summaries); err != nil {
		return nil, err
	}
	if err := queryParkAcknowledgements(ctx, db, projectID, summaries); err != nil {
		return nil, err
	}
	out := make([]ParkSummary, 0, len(summaries))
	for _, summary := range summaries {
		sort.Slice(summary.Causes, func(i, j int) bool {
			if summary.Causes[i].FirstAt.Equal(summary.Causes[j].FirstAt) {
				return summary.Causes[i].Cause < summary.Causes[j].Cause
			}
			return summary.Causes[i].FirstAt.Before(summary.Causes[j].FirstAt)
		})
		out = append(out, *summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID == out[j].ProjectID {
			return parkIssueKey(out[i].IssueID, out[i].Identifier, out[i].IssueURL) < parkIssueKey(out[j].IssueID, out[j].Identifier, out[j].IssueURL)
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out, nil
}

func queryParkEvents(ctx context.Context, db ParkSummaryQuerier, projectID string) ([]parkEvent, error) {
	args := []any{}
	filter := ""
	if projectID != "" {
		filter = " WHERE project_id = ?"
		args = append(args, projectID)
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, project_id, COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''),
       status, COALESCE(terminal_state, ''), COALESCE(error_class, ''),
       COALESCE(completed_at, ''), COALESCE(worker_metadata_json, '{}')
FROM work_attempts`+filter+`
ORDER BY project_id, completed_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying work attempts for park summaries: %w", err)
	}
	defer rows.Close()
	events := []parkEvent{}
	for rows.Next() {
		var event parkEvent
		var status, terminalState, errorClass, atRaw, metadata string
		if err := rows.Scan(&event.id, &event.projectID, &event.issueID, &event.identifier, &event.issueURL, &status, &terminalState, &errorClass, &atRaw, &metadata); err != nil {
			return nil, fmt.Errorf("scanning work attempt park summary: %w", err)
		}
		event.kind = "attempt"
		cause := attemptParkCause(status, terminalState, errorClass, metadata)
		if cause != "" && atRaw != "" {
			event.at, err = parseTimestamp("work attempt completed_at", atRaw)
			if err != nil {
				return nil, err
			}
			event.cause = cause
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating work attempt park summaries: %w", err)
	}
	workflowRows, err := db.QueryContext(ctx, `
SELECT id, project_id, COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''),
       COALESCE(reason, ''), started_at, COALESCE(metadata_json, '{}')
FROM workflow_phase_events
WHERE phase_type = 'lane' AND lower(trim(phase_name)) = 'blocked' AND lower(trim(COALESCE(status, 'entered'))) = 'entered'`+
		func() string {
			if projectID != "" {
				return " AND project_id = ?"
			}
			return ""
		}()+`
ORDER BY project_id, started_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying workflow parks: %w", err)
	}
	defer workflowRows.Close()
	for workflowRows.Next() {
		var event parkEvent
		var reason, atRaw, metadata string
		if err := workflowRows.Scan(&event.id, &event.projectID, &event.issueID, &event.identifier, &event.issueURL, &reason, &atRaw, &metadata); err != nil {
			return nil, fmt.Errorf("scanning workflow park: %w", err)
		}
		cause, orchestrated := workflowParkCause(reason, metadata)
		if !orchestrated {
			continue
		}
		event.kind = "workflow"
		event.cause = cause
		event.at, err = parseTimestamp("workflow park started_at", atRaw)
		if err != nil {
			return nil, err
		}
		for index := range events {
			candidate := &events[index]
			if candidate.kind == "attempt" && candidate.cause != "" && candidate.projectID == event.projectID && candidate.at.Equal(event.at) && parkAliasesOverlap(candidate.issueID, candidate.identifier, candidate.issueURL, event.issueID, event.identifier, event.issueURL) {
				candidate.cause = ""
			}
		}
		events = append(events, event)
	}
	if err := workflowRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating workflow parks: %w", err)
	}
	return events, nil
}

func queryParkUsage(ctx context.Context, db ParkSummaryQuerier, projectID string, summaries map[string]*ParkSummary) error {
	query := `SELECT project_id, COALESCE(issue_id, ''), COALESCE(identifier, ''),
CAST(COALESCE(SUM(input_tokens), 0) AS INTEGER), CAST(COALESCE(SUM(cached_input_tokens), 0) AS INTEGER),
CAST(COALESCE(SUM(output_tokens), 0) AS INTEGER), CAST(COALESCE(SUM(reasoning_output_tokens), 0) AS INTEGER)
FROM usage_events`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = ?"
		args = append(args, projectID)
	}
	query += " GROUP BY project_id, issue_id, identifier"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying issue token totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, issueID, identifier string
		var totals ParkTokenTotals
		if err := rows.Scan(&project, &issueID, &identifier, &totals.InputTokens, &totals.CachedInputTokens, &totals.OutputTokens, &totals.ReasoningOutputTokens); err != nil {
			return fmt.Errorf("scanning issue token totals: %w", err)
		}
		summary := parkSummaryFor(summaries, project, issueID, identifier, "")
		summary.Tokens.InputTokens += totals.InputTokens
		summary.Tokens.CachedInputTokens += totals.CachedInputTokens
		summary.Tokens.OutputTokens += totals.OutputTokens
		summary.Tokens.ReasoningOutputTokens += totals.ReasoningOutputTokens
	}
	return rows.Err()
}

func queryParkAcknowledgements(ctx context.Context, db ParkSummaryQuerier, projectID string, summaries map[string]*ParkSummary) error {
	query := `SELECT project_id, COALESCE(issue_id, ''), COALESCE(identifier, ''), COALESCE(issue_url, ''), park_sequence, acknowledged_at FROM issue_park_acknowledgements`
	args := []any{}
	if projectID != "" {
		query += " WHERE project_id = ?"
		args = append(args, projectID)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying issue park acknowledgements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, issueID, identifier, issueURL, atRaw string
		var sequence int64
		if err := rows.Scan(&project, &issueID, &identifier, &issueURL, &sequence, &atRaw); err != nil {
			return fmt.Errorf("scanning issue park acknowledgement: %w", err)
		}
		summary := parkSummaryFor(summaries, project, issueID, identifier, issueURL)
		at, err := parseTimestamp("park acknowledged_at", atRaw)
		if err != nil {
			return err
		}
		summary.AcknowledgedParkSequence = sequence
		summary.AcknowledgedAt = &at
	}
	return rows.Err()
}

func parkSummaryFor(summaries map[string]*ParkSummary, projectID, issueID, identifier, issueURL string) *ParkSummary {
	identity := normalizeParkIdentity(IssueIdentity{ProjectID: projectID, IssueID: issueID, Identifier: identifier, IssueURL: issueURL})
	for _, summary := range summaries {
		if parkIdentityMatches(identity, *summary) {
			if summary.IssueID == "" {
				summary.IssueID = identity.IssueID
			}
			if summary.Identifier == "" {
				summary.Identifier = identity.Identifier
			}
			if summary.IssueURL == "" {
				summary.IssueURL = identity.IssueURL
			}
			return summary
		}
	}
	key := identity.ProjectID + "\x00" + parkIssueKey(identity.IssueID, identity.Identifier, identity.IssueURL)
	summary := &ParkSummary{ProjectID: identity.ProjectID, IssueID: identity.IssueID, Identifier: identity.Identifier, IssueURL: identity.IssueURL, Causes: []ParkCauseSummary{}}
	summaries[key] = summary
	return summary
}

func addParkCause(summary *ParkSummary, cause string, at time.Time) {
	cause = strings.TrimSpace(cause)
	if cause == "" {
		cause = "blocked"
	}
	summary.ParkCount++
	for index := range summary.Causes {
		if summary.Causes[index].Cause != cause {
			continue
		}
		summary.Causes[index].Count++
		if at.Before(summary.Causes[index].FirstAt) {
			summary.Causes[index].FirstAt = at
		}
		if at.After(summary.Causes[index].LastAt) {
			summary.Causes[index].LastAt = at
		}
		return
	}
	summary.Causes = append(summary.Causes, ParkCauseSummary{Cause: cause, Count: 1, FirstAt: at, LastAt: at})
}

func attemptParkCause(status, terminalState, errorClass, metadata string) string {
	if !strings.EqualFold(strings.TrimSpace(status), string(WorkAttemptStatusTerminal)) {
		return ""
	}
	brake := jsonString(metadata, "brake_cause", "brake.cause", "session_brake.reason", "spend_since_progress_breaker.block_reason", "completion_progress.block_reason", "budget_refusal.code")
	switch strings.ToLower(strings.TrimSpace(terminalState)) {
	case "no_progress":
		if value := strings.TrimSpace(errorClass); value != "" {
			return value
		}
		if brake != "" {
			return brake
		}
		return "no_progress"
	case "failure":
		return brake
	default:
		return ""
	}
}

func workflowParkCause(reason, metadata string) (string, bool) {
	owner := jsonString(metadata, "blocked_recovery.owner")
	initiator := jsonString(metadata, "provenance.initiator")
	origin := jsonString(metadata, "provenance.origin")
	orchestrated := strings.EqualFold(owner, "orchestrator") || strings.EqualFold(initiator, "detent_instance") || strings.HasPrefix(strings.ToLower(origin), "detent")
	if !orchestrated {
		return "", false
	}
	cause := jsonString(metadata, "blocked_recovery.cause")
	if cause == "" {
		cause = strings.TrimSpace(reason)
	}
	if cause == "" {
		cause = "blocked"
	}
	return cause, true
}

func jsonString(raw string, paths ...string) string {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return ""
	}
	for _, path := range paths {
		current := value
		for _, part := range strings.Split(path, ".") {
			object, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = object[part]
		}
		if text, ok := current.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func normalizeParkIdentity(identity IssueIdentity) IssueIdentity {
	identity.ProjectID = strings.TrimSpace(identity.ProjectID)
	identity.IssueID = strings.TrimSpace(identity.IssueID)
	identity.Identifier = strings.TrimSpace(identity.Identifier)
	identity.IssueURL = strings.TrimSpace(identity.IssueURL)
	return identity
}

func parkIdentityMatches(identity IssueIdentity, summary ParkSummary) bool {
	identity = normalizeParkIdentity(identity)
	if identity.ProjectID != "" && identity.ProjectID != summary.ProjectID {
		return false
	}
	return parkAliasesOverlap(identity.IssueID, identity.Identifier, identity.IssueURL, summary.IssueID, summary.Identifier, summary.IssueURL)
}

func parkAliasesOverlap(issueIDA, identifierA, issueURLA, issueIDB, identifierB, issueURLB string) bool {
	return (issueIDA != "" && issueIDA == issueIDB) || (identifierA != "" && identifierA == identifierB) || (issueURLA != "" && issueURLA == issueURLB)
}

func parkIssueKey(issueID, identifier, issueURL string) string {
	if value := strings.TrimSpace(issueID); value != "" {
		return value
	}
	if value := strings.TrimSpace(identifier); value != "" {
		return value
	}
	return strings.TrimSpace(issueURL)
}
