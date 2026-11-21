package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) FreezeExecutionChecklist(ctx context.Context, project tracker.ProjectID, list tracker.ExecutionChecklist) error {
	inserted, err := s.db.Exec(ctx, `INSERT INTO native_execution_checklists (organization_id, project_id, attempt_id, revision, policy_id, frozen_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, s.organization, project, list.AttemptID, list.Revision, list.PolicyID, list.FrozenAt)
	if err != nil || inserted.RowsAffected() == 0 || len(list.Checks) == 0 {
		return err
	}
	ordinals := make([]int32, len(list.Checks))
	kinds := make([]string, len(list.Checks))
	texts := make([]string, len(list.Checks))
	digests := make([]string, len(list.Checks))
	statuses := make([]string, len(list.Checks))
	for i, check := range list.Checks {
		ordinals[i] = int32(check.Ordinal)
		kinds[i] = string(check.Kind)
		texts[i] = check.Text
		digests[i] = check.CheckDigest
		statuses[i] = string(check.Status)
	}
	_, err = s.db.Exec(ctx, `INSERT INTO native_execution_checks (organization_id, project_id, attempt_id, revision, ordinal, kind, source_text, check_digest, status)
SELECT $1, $2, $3, $4, entry.ordinal, entry.kind, entry.source_text, entry.check_digest, entry.status
FROM unnest($5::integer[], $6::text[], $7::text[], $8::text[], $9::text[]) AS entry(ordinal, kind, source_text, check_digest, status)`, s.organization, project, list.AttemptID, list.Revision, ordinals, kinds, texts, digests, statuses)
	return err
}

func (s *session) ExecutionChecklist(ctx context.Context, project tracker.ProjectID, attempt string) (*tracker.ExecutionChecklist, error) {
	list := tracker.ExecutionChecklist{AttemptID: attempt, Checks: []tracker.ExecutionCheck{}}
	err := s.db.QueryRow(ctx, `SELECT revision, policy_id, frozen_at FROM native_execution_checklists
WHERE organization_id = $1 AND project_id = $2 AND attempt_id = $3 ORDER BY frozen_at DESC, revision DESC LIMIT 1`, s.organization, project, attempt).Scan(&list.Revision, &list.PolicyID, &list.FrozenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT ordinal, kind, source_text, check_digest, status, head_sha, tree_sha, receipt_id, checked_at
FROM native_execution_checks WHERE organization_id = $1 AND project_id = $2 AND attempt_id = $3 AND revision = $4 ORDER BY ordinal`,
		s.organization, project, attempt, list.Revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var check tracker.ExecutionCheck
		var checkedAt *time.Time
		if err := rows.Scan(&check.Ordinal, &check.Kind, &check.Text, &check.CheckDigest, &check.Status, &check.Head, &check.Tree, &check.Receipt, &checkedAt); err != nil {
			return nil, err
		}
		if checkedAt != nil {
			check.CheckedAt = *checkedAt
		}
		list.Checks = append(list.Checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	interventions, err := s.db.Query(ctx, `SELECT thread_id, turn_id, action, fingerprint, period, repeats, sequence, observed_at
FROM native_execution_loop_interventions WHERE organization_id = $1 AND project_id = $2 AND attempt_id = $3 AND revision = $4
ORDER BY observed_at DESC LIMIT 64`, s.organization, project, attempt, list.Revision)
	if err != nil {
		return nil, err
	}
	defer interventions.Close()
	for interventions.Next() {
		event := tracker.ExecutionLoopIntervention{Revision: list.Revision}
		if err := interventions.Scan(&event.ThreadID, &event.TurnID, &event.Action, &event.Fingerprint, &event.Period, &event.Repeats, &event.Sequence, &event.ObservedAt); err != nil {
			return nil, err
		}
		list.Interventions = append(list.Interventions, event)
	}
	return &list, interventions.Err()
}

func (s *session) ProjectGateFailureEvents(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, itemID int64, policyID, digest, head, tree string) ([]leasestore.ProjectGateFailureEvent, error) {
	rows, err := s.db.Query(ctx, `SELECT t.round_id, t.receipt_id
FROM native_execution_check_transitions AS t
JOIN native_execution_checks AS c USING (organization_id, project_id, attempt_id, revision, ordinal)
JOIN native_execution_checklists AS l USING (organization_id, project_id, attempt_id, revision)
JOIN native_attempts AS a ON a.organization_id = t.organization_id AND a.id = t.attempt_id
WHERE t.organization_id = $1 AND t.project_id = $2 AND a.issue_id = $3 AND l.policy_id = $4
  AND c.kind = 'project_gate' AND c.check_digest = $5 AND t.head_sha = $6 AND t.tree_sha = $7
  AND t.status IN ('failed', 'flaky') AND t.round_id LIKE 'evidence:%'
ORDER BY t.checked_at DESC, t.receipt_id
LIMIT 8`, s.organization, project, itemID, policyID, digest, head, tree)
	if err != nil {
		return nil, err
	}
	type reference struct{ event, receipt string }
	var refs []reference
	for rows.Next() {
		var ref reference
		if err := rows.Scan(&ref.event, &ref.receipt); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]leasestore.ProjectGateFailureEvent, 0, len(refs))
	for _, ref := range refs {
		event, err := s.RunEvent(ctx, project, item, strings.TrimPrefix(ref.event, "evidence:"))
		if err != nil {
			return nil, err
		}
		result = append(result, leasestore.ProjectGateFailureEvent{Event: event, ReceiptID: ref.receipt})
	}
	return result, nil
}

func (f *runnerEventSession) RecordExecutionCheckChanges(ctx context.Context, project tracker.ProjectID, attempt, revision, round string, checkedAt time.Time, changes []tracker.ExecutionCheckUpdate) error {
	if project != f.command.Project || attempt != f.command.AttemptID {
		return corestore.ErrTenantMismatch
	}
	s := &session{organization: f.session.organization, db: runEventQueue{f.pending}}
	return s.RecordExecutionCheckChanges(ctx, project, attempt, revision, round, checkedAt, changes)
}

func (s *session) RecordExecutionCheckChanges(ctx context.Context, project tracker.ProjectID, attempt, revision, round string, checkedAt time.Time, changes []tracker.ExecutionCheckUpdate) error {
	ordinals := make([]int32, len(changes))
	statuses := make([]string, len(changes))
	heads := make([]string, len(changes))
	trees := make([]string, len(changes))
	receipts := make([]string, len(changes))
	for i, change := range changes {
		ordinals[i], statuses[i], heads[i], trees[i], receipts[i] = int32(change.Ordinal), string(change.Status), change.Head, change.Tree, change.Receipt
	}
	_, err := s.db.Exec(ctx, `WITH changed AS (
UPDATE native_execution_checks AS c
SET status = delta.status, head_sha = delta.head, tree_sha = delta.tree, receipt_id = delta.receipt, checked_at = $5
FROM unnest($7::integer[], $8::text[], $9::text[], $10::text[], $11::text[]) AS delta(ordinal, status, head, tree, receipt)
WHERE c.organization_id = $1 AND c.project_id = $2 AND c.attempt_id = $3 AND c.revision = $4 AND c.ordinal = delta.ordinal
  AND (c.status, c.head_sha, c.tree_sha) IS DISTINCT FROM (delta.status, delta.head, delta.tree)
RETURNING c.organization_id, c.project_id, c.attempt_id, c.revision, c.ordinal, c.status, c.head_sha, c.tree_sha, c.receipt_id, c.checked_at
) INSERT INTO native_execution_check_transitions (organization_id, project_id, attempt_id, revision, ordinal, round_id, status, head_sha, tree_sha, receipt_id, checked_at)
SELECT organization_id, project_id, attempt_id, revision, ordinal, $6, status, head_sha, tree_sha, receipt_id, checked_at FROM changed`,
		s.organization, project, attempt, revision, checkedAt, round, ordinals, statuses, heads, trees, receipts)
	return err
}

func (s *session) RecordLoopIntervention(ctx context.Context, project tracker.ProjectID, attempt string, event tracker.ExecutionLoopIntervention) error {
	const statement = `INSERT INTO native_execution_loop_interventions
(organization_id, project_id, attempt_id, revision, thread_id, turn_id, action, fingerprint, period, repeats, sequence, observed_at)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
WHERE $4 = (SELECT revision FROM native_execution_checklists WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 ORDER BY frozen_at DESC, revision DESC LIMIT 1)
ON CONFLICT DO NOTHING`
	result, err := s.db.Exec(ctx, statement, s.organization, project, attempt, event.Revision, event.ThreadID, event.TurnID, event.Action, event.Fingerprint, event.Period, event.Repeats, event.Sequence, event.ObservedAt)
	if err != nil || result.RowsAffected() != 0 {
		return err
	}
	var same bool
	err = s.db.QueryRow(ctx, `SELECT (fingerprint, period, repeats) = ($8, $9, $10) FROM native_execution_loop_interventions
WHERE organization_id = $1 AND project_id = $2 AND attempt_id = $3 AND revision = $4 AND thread_id = $5 AND turn_id = $6 AND action = $7 AND sequence = $11`,
		s.organization, project, attempt, event.Revision, event.ThreadID, event.TurnID, event.Action, event.Fingerprint, event.Period, event.Repeats, event.Sequence).Scan(&same)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !same {
		return corestore.ErrRevisionConflict
	}
	return err
}
