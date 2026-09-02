package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/provenance"
)

const (
	admissionResolutionNonDeliverable = "non_deliverable_declined"
	admissionResolutionCriteriaNotMet = "criteria_not_met_declined"
	admissionDeclineCriteriaNotMet    = "criteria_not_met"
)

func (s *sqliteStore) CreateAdmissionDecline(ctx context.Context, decline admissionmodel.Decline) (bool, error) {
	if err := validateAdmissionDecline(decline); err != nil {
		return false, err
	}
	createdAt, err := requiredTimestamp("created_at", decline.CreatedAt)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin backlog admission decline: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	resolutionReason := admissionResolutionNonDeliverable
	if strings.TrimSpace(decline.Reason) == admissionDeclineCriteriaNotMet {
		resolutionReason = admissionResolutionCriteriaNotMet
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET status = 'superseded', resolved_at = ?, resolution_reason = ?,
    decision_seconds = CAST(MAX(0, (julianday(?) - julianday(created_at)) * 86400) AS INTEGER)
WHERE project_id = ? AND issue_id = ? AND status = 'open'`,
		createdAt,
		resolutionReason,
		createdAt,
		strings.TrimSpace(decline.ProjectID),
		strings.TrimSpace(decline.IssueID),
	); err != nil {
		return false, fmt.Errorf("supersede non-deliverable backlog admission proposals: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO backlog_admission_declines (
  id, project_id, issue_id, issue_identifier, issue_url, fingerprint,
  reason, detail, confidence, failed_dimension, failed_criterion, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id, issue_id, fingerprint) DO NOTHING`,
		strings.TrimSpace(decline.ID),
		strings.TrimSpace(decline.ProjectID),
		strings.TrimSpace(decline.IssueID),
		strings.TrimSpace(decline.IssueIdentifier),
		strings.TrimSpace(decline.IssueURL),
		strings.TrimSpace(decline.Fingerprint),
		strings.TrimSpace(decline.Reason),
		strings.TrimSpace(decline.Detail),
		decline.Confidence,
		strings.TrimSpace(decline.FailedDimension),
		strings.TrimSpace(decline.FailedCriterion),
		createdAt,
	)
	if err != nil {
		return false, fmt.Errorf("create backlog admission decline: %w", err)
	}
	created, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read backlog admission decline result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit backlog admission decline: %w", err)
	}
	committed = true
	return created == 1, nil
}

func (s *sqliteStore) AdmissionDecline(
	ctx context.Context,
	projectID string,
	issueID string,
	fingerprint string,
) (admissionmodel.Decline, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, project_id, issue_id, issue_identifier, issue_url, fingerprint,
       reason, detail, confidence, failed_dimension, failed_criterion,
       created_at, COALESCE(commented_at, '')
FROM backlog_admission_declines
WHERE project_id = ? AND issue_id = ? AND fingerprint = ?`,
		strings.TrimSpace(projectID),
		strings.TrimSpace(issueID),
		strings.TrimSpace(fingerprint),
	)
	decline, err := scanAdmissionDecline(row.Scan)
	if errors.Is(err, ErrNotFound) {
		return admissionmodel.Decline{}, false, nil
	}
	if err != nil {
		return admissionmodel.Decline{}, false, fmt.Errorf("read backlog admission decline: %w", err)
	}
	return decline, true, nil
}

func (s *sqliteStore) MarkAdmissionDeclineCommented(ctx context.Context, id string, at time.Time) error {
	commentedAt, err := requiredTimestamp("commented_at", at)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE backlog_admission_declines
SET commented_at = COALESCE(commented_at, ?)
WHERE id = ?`, commentedAt, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("mark backlog admission decline commented: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read backlog admission decline comment count: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

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
SET status = 'superseded', resolved_at = ?,
    decision_seconds = CAST(MAX(0, (julianday(?) - julianday(created_at)) * 86400) AS INTEGER)
WHERE project_id = ? AND issue_id = ? AND target_state = ? AND status = 'open'`,
		createdAt,
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
  criteria_section, criteria_text, findings_json, confidence, recommended_effort,
  effort_rationale, status, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?)`,
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
		strings.TrimSpace(proposal.RecommendedEffort),
		strings.TrimSpace(proposal.EffortRationale),
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
       criteria_section, criteria_text, findings_json, confidence,
       COALESCE(recommended_effort, ''), COALESCE(effort_rationale, ''),
       status, created_at,
       expires_at, COALESCE(resolved_at, ''), COALESCE(commented_at, ''),
       COALESCE(decision_comment_id, ''), COALESCE(decision_actor_login, ''),
       COALESCE(decision_actor_kind, ''), COALESCE(transition_at, ''),
       COALESCE(decision_seconds, 0), COALESCE(resolution_reason, '')
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

func (s *sqliteStore) AdmissionProposalsAwaitingDecision(
	ctx context.Context,
	projectID string,
	at time.Time,
) ([]admissionmodel.Proposal, error) {
	observedAt, err := requiredTimestamp("observed_at", at)
	if err != nil {
		return nil, err
	}
	query := `
SELECT id, project_id, issue_id, issue_identifier, issue_url, target_state, fingerprint,
       criteria_section, criteria_text, findings_json, confidence,
       COALESCE(recommended_effort, ''), COALESCE(effort_rationale, ''),
       status, created_at,
       expires_at, COALESCE(resolved_at, ''), COALESCE(commented_at, ''),
       COALESCE(decision_comment_id, ''), COALESCE(decision_actor_login, ''),
       COALESCE(decision_actor_kind, ''), COALESCE(transition_at, ''),
       COALESCE(decision_seconds, 0), COALESCE(resolution_reason, '')
FROM backlog_admission_proposals
WHERE status = 'open' AND expires_at > ?`
	args := []any{observedAt}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		query += " AND project_id = ?"
		args = append(args, projectID)
	}
	query += "\nORDER BY project_id, created_at, id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read backlog admission proposals awaiting decision: %w", err)
	}
	defer rows.Close()
	return scanAdmissionProposals(rows)
}

func (s *sqliteStore) AdmissionProposalHistory(ctx context.Context, projectID string, issueID string) ([]admissionmodel.Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, project_id, issue_id, issue_identifier, issue_url, target_state, fingerprint,
       criteria_section, criteria_text, findings_json, confidence,
       COALESCE(recommended_effort, ''), COALESCE(effort_rationale, ''),
       status, created_at,
       expires_at, COALESCE(resolved_at, ''), COALESCE(commented_at, ''),
       COALESCE(decision_comment_id, ''), COALESCE(decision_actor_login, ''),
       COALESCE(decision_actor_kind, ''), COALESCE(transition_at, ''),
       COALESCE(decision_seconds, 0), COALESCE(resolution_reason, '')
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

func (s *sqliteStore) AdmissionTargetTransitions(
	ctx context.Context,
	query admissionmodel.TargetTransitionQuery,
) ([]admissionmodel.TargetTransition, error) {
	notBefore, err := requiredTimestamp("not_before", query.NotBefore)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, started_at, metadata_json
FROM workflow_phase_events
WHERE project_id = ? AND issue_id = ? AND phase_type = 'lane' AND status = 'entered'
  AND lower(phase_name) = lower(?) AND started_at >= ?
ORDER BY started_at, id`,
		strings.TrimSpace(query.ProjectID),
		strings.TrimSpace(query.IssueID),
		strings.TrimSpace(query.TargetState),
		notBefore,
	)
	if err != nil {
		return nil, fmt.Errorf("read backlog admission target transitions: %w", err)
	}
	defer rows.Close()
	transitions := []admissionmodel.TargetTransition{}
	for rows.Next() {
		var transition admissionmodel.TargetTransition
		var enteredAt string
		var metadataJSON string
		if err := rows.Scan(&transition.EventID, &enteredAt, &metadataJSON); err != nil {
			return nil, err
		}
		transition.EnteredAt, err = parseTimestamp("started_at", enteredAt)
		if err != nil {
			return nil, err
		}
		if metadata, ok := provenance.Parse(metadataJSON); ok && metadata.Provenance.Actor != nil {
			transition.ActorLogin = metadata.Provenance.Actor.Login
			transition.ActorKind = metadata.Provenance.Actor.Kind
		}
		transitions = append(transitions, transition)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return transitions, nil
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
SET status = 'expired', resolved_at = ?,
    decision_seconds = CAST(MAX(0, (julianday(?) - julianday(created_at)) * 86400) AS INTEGER)
WHERE project_id = ? AND status = 'open' AND expires_at <= ?`,
		resolvedAt,
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

func (s *sqliteStore) ResolveAdmissionProposal(ctx context.Context, decision admissionmodel.Decision) error {
	if strings.TrimSpace(decision.ProposalID) == "" {
		return errors.New("backlog admission proposal id is required")
	}
	if decision.Outcome != admissionmodel.ProposalAccepted &&
		decision.Outcome != admissionmodel.ProposalRejected &&
		decision.Outcome != admissionmodel.ProposalSuperseded {
		return errors.New("backlog admission decision outcome must be accepted, rejected, or superseded")
	}
	if decision.DecidedAt.IsZero() {
		return errors.New("backlog admission decision time is required")
	}
	if strings.TrimSpace(decision.CommentID) == "" && !decision.Automatic && !decision.Implicit {
		return errors.New("backlog admission decision comment is required")
	}
	if decision.Outcome == admissionmodel.ProposalAccepted && decision.TransitionAt.IsZero() {
		return errors.New("backlog admission acceptance transition is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin backlog admission decision: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var proposal admissionmodel.Proposal
	var createdAt string
	if err := tx.QueryRowContext(ctx, `
SELECT project_id, issue_id, issue_identifier, issue_url, target_state, created_at
FROM backlog_admission_proposals
WHERE id = ? AND status = 'open'`,
		strings.TrimSpace(decision.ProposalID),
	).Scan(
		&proposal.ProjectID,
		&proposal.IssueID,
		&proposal.IssueIdentifier,
		&proposal.IssueURL,
		&proposal.TargetState,
		&createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read backlog admission proposal for decision: %w", err)
	}
	proposal.CreatedAt, err = parseTimestamp("created_at", createdAt)
	if err != nil {
		return err
	}
	decidedAt, err := requiredTimestamp("resolved_at", decision.DecidedAt)
	if err != nil {
		return err
	}
	transitionAt, err := optionalTimestamp("transition_at", decision.TransitionAt)
	if err != nil {
		return err
	}
	decisionSeconds := int64(decision.DecidedAt.Sub(proposal.CreatedAt) / time.Second)
	if decisionSeconds < 0 {
		decisionSeconds = 0
	}
	result, err := tx.ExecContext(ctx, `
UPDATE backlog_admission_proposals
SET status = ?, resolved_at = ?, decision_comment_id = ?, decision_actor_login = ?,
    decision_actor_kind = ?, transition_at = ?, decision_seconds = ?, resolution_reason = ?
WHERE id = ? AND status = 'open'`,
		string(decision.Outcome),
		decidedAt,
		strings.TrimSpace(decision.CommentID),
		nullString(decision.ActorLogin),
		nullString(decision.ActorKind),
		transitionAt,
		decisionSeconds,
		nullString(decision.Reason),
		strings.TrimSpace(decision.ProposalID),
	)
	if err != nil {
		return fmt.Errorf("resolve backlog admission proposal: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read backlog admission decision count: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	if decision.Outcome == admissionmodel.ProposalAccepted {
		if err := attributeAdmissionTransition(ctx, tx, proposal, decision); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO backlog_admission_downstream_outcomes (
  proposal_id, project_id, issue_id, updated_at
) VALUES (?, ?, ?, ?)
ON CONFLICT(proposal_id) DO UPDATE SET updated_at = excluded.updated_at`,
			strings.TrimSpace(decision.ProposalID),
			strings.TrimSpace(proposal.ProjectID),
			strings.TrimSpace(proposal.IssueID),
			decidedAt,
		); err != nil {
			return fmt.Errorf("initialize backlog admission downstream outcome: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backlog admission decision: %w", err)
	}
	committed = true
	return nil
}

func attributeAdmissionTransition(
	ctx context.Context,
	tx *sql.Tx,
	proposal admissionmodel.Proposal,
	decision admissionmodel.Decision,
) error {
	transitionAt, err := requiredTimestamp("transition_at", decision.TransitionAt)
	if err != nil {
		return err
	}
	var eventID int64
	var metadataJSON string
	if decision.TransitionEventID > 0 {
		err = tx.QueryRowContext(ctx, `
SELECT id, metadata_json
FROM workflow_phase_events
WHERE id = ? AND project_id = ? AND issue_id = ? AND phase_type = 'lane'
  AND status = 'entered' AND lower(phase_name) = lower(?)
LIMIT 1`,
			decision.TransitionEventID,
			strings.TrimSpace(proposal.ProjectID),
			strings.TrimSpace(proposal.IssueID),
			strings.TrimSpace(proposal.TargetState),
		).Scan(&eventID, &metadataJSON)
		if err != nil {
			return fmt.Errorf("find correlated backlog admission transition: %w", err)
		}
	} else {
		err = tx.QueryRowContext(ctx, `
SELECT id, metadata_json
FROM workflow_phase_events
WHERE project_id = ? AND issue_id = ? AND phase_type = 'lane' AND status = 'entered'
  AND lower(phase_name) = lower(?) AND started_at = ?
ORDER BY id DESC
LIMIT 1`,
			strings.TrimSpace(proposal.ProjectID),
			strings.TrimSpace(proposal.IssueID),
			strings.TrimSpace(proposal.TargetState),
			transitionAt,
		).Scan(&eventID, &metadataJSON)
	}
	actor := provenance.Actor{
		Login: strings.TrimSpace(decision.ActorLogin),
		Kind:  strings.TrimSpace(decision.ActorKind),
	}
	attribution := provenance.Attribution{
		Origin: provenance.OriginAdmission,
		Actor:  admissionActorPointer(actor),
	}
	metadataJSON = provenance.Apply(metadataJSON, attribution, &provenance.Admission{
		ProposalID: strings.TrimSpace(decision.ProposalID),
		Attributed: true,
	})
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
UPDATE workflow_phase_events
SET reason = 'admission_proposal_accepted', metadata_json = ?
WHERE id = ?`, metadataJSON, eventID); err != nil {
			return fmt.Errorf("attribute backlog admission transition: %w", err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("find backlog admission transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_phase_events (
  project_id, issue_id, identifier, issue_url, phase_type, phase_name, reason,
  status, started_at, event_day, endpoint_family, metadata_json
) VALUES (?, ?, ?, ?, 'lane', ?, 'admission_proposal_accepted', 'entered', ?, ?, 'tracker', ?)`,
		strings.TrimSpace(proposal.ProjectID),
		nullString(proposal.IssueID),
		nullString(proposal.IssueIdentifier),
		nullString(proposal.IssueURL),
		strings.TrimSpace(proposal.TargetState),
		transitionAt,
		decision.TransitionAt.UTC().Format("2006-01-02"),
		metadataJSON,
	); err != nil {
		return fmt.Errorf("record backlog admission transition: %w", err)
	}
	return nil
}

func admissionActorPointer(actor provenance.Actor) *provenance.Actor {
	if actor.Login == "" && actor.Kind == "" {
		return nil
	}
	return &actor
}

func (s *sqliteStore) RefreshAdmissionOutcomes(ctx context.Context, refresh admissionmodel.OutcomeRefresh) error {
	projectID := strings.TrimSpace(refresh.ProjectID)
	if projectID == "" {
		return errors.New("backlog admission outcome project id is required")
	}
	if refresh.ObservedAt.IsZero() {
		return errors.New("backlog admission outcome observation time is required")
	}
	proposals, err := s.acceptedAdmissionProposals(ctx, projectID)
	if err != nil {
		return err
	}
	for _, proposal := range proposals {
		if err := s.refreshAdmissionOutcome(ctx, projectID, proposal.id, proposal.issueID, proposal.resolvedAt, refresh); err != nil {
			return err
		}
	}
	return nil
}

type acceptedAdmissionProposal struct {
	id         string
	issueID    string
	resolvedAt time.Time
}

func (s *sqliteStore) acceptedAdmissionProposals(ctx context.Context, projectID string) (_ []acceptedAdmissionProposal, resultErr error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, issue_id, resolved_at
FROM backlog_admission_proposals
WHERE project_id = ? AND status = 'accepted'
ORDER BY resolved_at, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read accepted backlog admission proposals: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	proposals := []acceptedAdmissionProposal{}
	for rows.Next() {
		var proposal acceptedAdmissionProposal
		var resolvedAt string
		if err := rows.Scan(&proposal.id, &proposal.issueID, &resolvedAt); err != nil {
			return nil, err
		}
		proposal.resolvedAt, err = parseTimestamp("resolved_at", resolvedAt)
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

func (s *sqliteStore) refreshAdmissionOutcome(
	ctx context.Context,
	projectID string,
	proposalID string,
	issueID string,
	resolvedAt time.Time,
	refresh admissionmodel.OutcomeRefresh,
) error {
	resolvedAtRaw, err := requiredTimestamp("resolved_at", resolvedAt)
	if err != nil {
		return err
	}
	workflowOutcome, err := s.admissionWorkflowOutcome(ctx, projectID, issueID, resolvedAtRaw, refresh)
	if err != nil {
		return err
	}
	var spendUSD float64
	if err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(cost_usd), 0.0)
FROM usage_events
WHERE project_id = ? AND issue_id = ? AND finished_at >= ?`,
		projectID,
		strings.TrimSpace(issueID),
		resolvedAtRaw,
	).Scan(&spendUSD); err != nil {
		return fmt.Errorf("read backlog admission downstream spend: %w", err)
	}
	observedAt, err := requiredTimestamp("updated_at", refresh.ObservedAt)
	if err != nil {
		return err
	}
	completedAtRaw, err := optionalTimestamp("completed_at", workflowOutcome.completedAt)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO backlog_admission_downstream_outcomes (
  proposal_id, project_id, issue_id, completed_at, rework_count,
  review_churn_count, spend_usd, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(proposal_id) DO UPDATE SET
  completed_at = excluded.completed_at,
  rework_count = excluded.rework_count,
  review_churn_count = excluded.review_churn_count,
  spend_usd = excluded.spend_usd,
  updated_at = excluded.updated_at`,
		strings.TrimSpace(proposalID),
		projectID,
		strings.TrimSpace(issueID),
		completedAtRaw,
		workflowOutcome.reworkCount,
		workflowOutcome.reviewChurnCount,
		spendUSD,
		observedAt,
	); err != nil {
		return fmt.Errorf("record backlog admission downstream outcome: %w", err)
	}
	return nil
}

type admissionWorkflowOutcome struct {
	completedAt      time.Time
	reworkCount      int
	reviewChurnCount int
}

func (s *sqliteStore) admissionWorkflowOutcome(
	ctx context.Context,
	projectID string,
	issueID string,
	resolvedAtRaw string,
	refresh admissionmodel.OutcomeRefresh,
) (_ admissionWorkflowOutcome, resultErr error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT phase_type, phase_name, status, started_at
FROM workflow_phase_events
WHERE project_id = ? AND issue_id = ? AND started_at >= ?
ORDER BY started_at, id`, projectID, strings.TrimSpace(issueID), resolvedAtRaw)
	if err != nil {
		return admissionWorkflowOutcome{}, fmt.Errorf("read backlog admission downstream workflow: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, rows.Close())
	}()
	outcome := admissionWorkflowOutcome{}
	for rows.Next() {
		var phaseType string
		var phaseName string
		var status string
		var startedAtRaw string
		if err := rows.Scan(&phaseType, &phaseName, &status, &startedAtRaw); err != nil {
			return admissionWorkflowOutcome{}, err
		}
		startedAt, err := parseTimestamp("started_at", startedAtRaw)
		if err != nil {
			return admissionWorkflowOutcome{}, err
		}
		if strings.EqualFold(strings.TrimSpace(phaseType), "review") {
			outcome.reviewChurnCount++
		}
		if !strings.EqualFold(strings.TrimSpace(phaseType), "lane") ||
			!strings.EqualFold(strings.TrimSpace(status), "entered") {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(phaseName), strings.TrimSpace(refresh.ReworkState)) {
			outcome.reworkCount++
		}
		if outcome.completedAt.IsZero() && containsAdmissionState(refresh.TerminalStates, phaseName) {
			outcome.completedAt = startedAt
		}
	}
	if err := rows.Err(); err != nil {
		return admissionWorkflowOutcome{}, err
	}
	return outcome, nil
}

func (s *sqliteStore) AdmissionDownstreamOutcomes(ctx context.Context, projectID string) ([]admissionmodel.DownstreamOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT proposal_id, project_id, issue_id, COALESCE(completed_at, ''), rework_count,
       review_churn_count, spend_usd, updated_at
FROM backlog_admission_downstream_outcomes
WHERE project_id = ?
ORDER BY updated_at DESC, proposal_id`, strings.TrimSpace(projectID))
	if err != nil {
		return nil, fmt.Errorf("read backlog admission downstream outcomes: %w", err)
	}
	defer rows.Close()
	outcomes := []admissionmodel.DownstreamOutcome{}
	for rows.Next() {
		var outcome admissionmodel.DownstreamOutcome
		var completedAt string
		var updatedAt string
		if err := rows.Scan(
			&outcome.ProposalID,
			&outcome.ProjectID,
			&outcome.IssueID,
			&completedAt,
			&outcome.ReworkCount,
			&outcome.ReviewChurnCount,
			&outcome.SpendUSD,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		if outcome.CompletedAt, err = parseAdmissionOptionalTimestamp("completed_at", completedAt); err != nil {
			return nil, err
		}
		if outcome.UpdatedAt, err = parseTimestamp("updated_at", updatedAt); err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return outcomes, nil
}

func containsAdmissionState(states []string, state string) bool {
	for _, candidate := range states {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(state)) {
			return true
		}
	}
	return false
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
  project_id, scheduled_for, started_at, completed_at, outcome, deferred_reason, proposal_reason,
  candidates_found_count, candidates_count, proposed_count, skipped_json,
  truncated_json, issues_json, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.ProjectID),
		scheduledFor,
		startedAt,
		completedAt,
		strings.TrimSpace(record.Outcome),
		nullString(record.DeferredReason),
		nullString(record.ProposalReason),
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
       COALESCE(deferred_reason, ''), COALESCE(proposal_reason, ''), candidates_found_count, candidates_count,
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
       COALESCE(deferred_reason, ''), COALESCE(proposal_reason, ''), candidates_found_count, candidates_count,
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

func validateAdmissionDecline(decline admissionmodel.Decline) error {
	switch {
	case strings.TrimSpace(decline.ID) == "":
		return errors.New("backlog admission decline id is required")
	case strings.TrimSpace(decline.ProjectID) == "":
		return errors.New("backlog admission decline project id is required")
	case strings.TrimSpace(decline.IssueID) == "":
		return errors.New("backlog admission decline issue id is required")
	case strings.TrimSpace(decline.Fingerprint) == "":
		return errors.New("backlog admission decline fingerprint is required")
	case strings.TrimSpace(decline.Reason) == "":
		return errors.New("backlog admission decline reason is required")
	case strings.TrimSpace(decline.Detail) == "":
		return errors.New("backlog admission decline detail is required")
	case decline.Confidence != nil && (math.IsNaN(*decline.Confidence) || math.IsInf(*decline.Confidence, 0) || *decline.Confidence < 0 || *decline.Confidence > 1):
		return errors.New("backlog admission decline confidence must be between zero and one")
	case decline.Reason == admissionDeclineCriteriaNotMet && decline.Confidence == nil:
		return errors.New("backlog admission criteria decline confidence is required")
	case decline.Reason == admissionDeclineCriteriaNotMet && strings.TrimSpace(decline.FailedDimension) == "":
		return errors.New("backlog admission criteria decline failed dimension is required")
	case decline.Reason == admissionDeclineCriteriaNotMet && strings.TrimSpace(decline.FailedCriterion) == "":
		return errors.New("backlog admission criteria decline failed criterion is required")
	case decline.CreatedAt.IsZero():
		return errors.New("backlog admission decline created at is required")
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

func scanAdmissionDecline(scan admissionScan) (admissionmodel.Decline, error) {
	var decline admissionmodel.Decline
	var confidence sql.NullFloat64
	var createdAt string
	var commentedAt string
	if err := scan(
		&decline.ID,
		&decline.ProjectID,
		&decline.IssueID,
		&decline.IssueIdentifier,
		&decline.IssueURL,
		&decline.Fingerprint,
		&decline.Reason,
		&decline.Detail,
		&confidence,
		&decline.FailedDimension,
		&decline.FailedCriterion,
		&createdAt,
		&commentedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return admissionmodel.Decline{}, ErrNotFound
		}
		return admissionmodel.Decline{}, err
	}
	if confidence.Valid {
		decline.Confidence = &confidence.Float64
	}
	var err error
	decline.CreatedAt, err = parseTimestamp("created_at", createdAt)
	if err != nil {
		return admissionmodel.Decline{}, err
	}
	decline.CommentedAt, err = parseAdmissionOptionalTimestamp("commented_at", commentedAt)
	if err != nil {
		return admissionmodel.Decline{}, err
	}
	return decline, nil
}

func scanAdmissionProposal(scan admissionScan) (admissionmodel.Proposal, error) {
	var proposal admissionmodel.Proposal
	var findingsJSON string
	var status string
	var createdAt string
	var expiresAt string
	var resolvedAt string
	var commentedAt string
	var transitionAt string
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
		&proposal.RecommendedEffort,
		&proposal.EffortRationale,
		&status,
		&createdAt,
		&expiresAt,
		&resolvedAt,
		&commentedAt,
		&proposal.DecisionCommentID,
		&proposal.DecisionActorLogin,
		&proposal.DecisionActorKind,
		&transitionAt,
		&proposal.DecisionSeconds,
		&proposal.ResolutionReason,
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
	if proposal.TransitionAt, err = parseAdmissionOptionalTimestamp("transition_at", transitionAt); err != nil {
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
		&record.ProposalReason,
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
