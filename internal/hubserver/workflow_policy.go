package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
)

func (d *database) applyObservedDefaultBranchPolicy(ctx context.Context, scope string, observation policy.Observation) (resultErr error) {
	source := observation.Source
	descriptor := observation.Descriptor
	if strings.HasPrefix(scope, "repository:") {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	approval, err := readProjectPolicy(ctx, tx, scope)
	if err != nil {
		var nativeErr *nativeError
		if !errors.As(err, &nativeErr) || nativeErr.Code != "policy_mismatch" {
			return err
		}
	}
	current := approval.Policy.ID
	carryover := current != "" && descriptor.SameAuthoredInputs(approval.Policy)
	if current != "" && !carryover {
		return tx.Commit()
	}
	if descriptor.Workflow != nil {
		if source == nil || !validCommitID(source.Commit) || !validCommitID(source.DefaultBranchHead) || source.DefaultBranch == "" || source.Commit != descriptor.Workflow.Revision {
			return d.commit(ctx, tx)
		}
		repository, err := policyRepository(ctx, tx, scope)
		if err != nil {
			return err
		}
		if repository == "" || !strings.EqualFold(repository, source.Repository) {
			return d.commit(ctx, tx)
		}
		if !carryover && !source.DefaultBranchReachable {
			return d.commit(ctx, tx)
		}
	} else {
		if !carryover {
			return d.commit(ctx, tx)
		}
		if err := validateWorkflowPolicy(ctx, tx, scope, descriptor); err != nil {
			return err
		}
	}
	if carryover && descriptor.Authored.Version < approval.Policy.Authored.Version {
		return d.commit(ctx, tx)
	}
	if current != descriptor.ID {
		actor := approval.ApprovedBy
		if actor == "" || strings.HasPrefix(actor, "runner_") {
			actor = "repository-default-branch"
		}
		if _, err := d.approvePolicyInTx(ctx, tx, scope, actor, policy.Change{ExpectedID: current, Policy: descriptor}); err != nil {
			return err
		}
	}
	return d.commit(ctx, tx)
}

func policyRepository(ctx context.Context, query policyQuerier, scope string) (string, error) {
	if repository, ok := strings.CutPrefix(scope, "repository:"); ok {
		return repository, nil
	}
	organization, project, ok := strings.Cut(scope, "/")
	if !ok {
		return "", nil
	}
	var repository string
	err := query.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(p.checkout_repository, ''), r.github_owner || '/' || r.github_name, '')
FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id WHERE p.organization_id = ? AND p.id = ?`, organization, project).Scan(&repository)
	return repository, err
}

func recordWorkflowApply(ctx context.Context, tx *sql.Tx, scope, actor, current string, descriptor policy.Descriptor, now time.Time) error {
	entry := policy.WorkflowApply{DefinitionDigest: descriptor.SourceDigest, AppliedBy: actor, AppliedAt: formatHubTime(now)}
	if descriptor.Workflow != nil {
		entry.Commit = descriptor.Workflow.Revision
	}
	repository, err := policyRepository(ctx, tx, scope)
	if err != nil {
		return err
	}
	entry.Repository = repository
	if current != "" {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT metadata_json FROM policy_revisions WHERE scope = ? AND policy_id = ?", scope, current).Scan(&raw); err != nil {
			return err
		}
		var previous policy.Descriptor
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return err
		}
		entry.PreviousDefinitionDigest = previous.SourceDigest
	}
	err = tx.QueryRowContext(ctx, `SELECT runner_id FROM project_observed_policies WHERE scope = ? AND policy_id = ? ORDER BY observed_at DESC, runner_id LIMIT 1`, scope, descriptor.ID).Scan(&entry.RunnerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.HasPrefix(actor, "runner_") {
		entry.RunnerID = actor
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_workflow_applies
(scope, repository, source_commit, previous_definition_digest, definition_digest, runner_id, applied_by, applied_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, scope, entry.Repository, entry.Commit, entry.PreviousDefinitionDigest, entry.DefinitionDigest, entry.RunnerID, entry.AppliedBy, entry.AppliedAt)
	return err
}

func readProjectPolicyWithHistory(ctx context.Context, query nativeQueryer, scope string, limit int, after string) (policy.Approval, error) {
	return readProjectPolicyHistory(ctx, query, scope, limit, after, true)
}

func readProjectPolicyHistory(ctx context.Context, query nativeQueryer, scope string, limit int, after string, definitions bool) (result policy.Approval, resultErr error) {
	if limit == 0 {
		limit = defaultAPIPageLimit
	}
	if limit < 1 || limit > maxAPIPageLimit {
		return result, nativeInvalid("History page limit is invalid")
	}
	var before int64
	if after != "" {
		var err error
		before, err = strconv.ParseInt(after, 10, 64)
		if err != nil || before < 1 {
			return result, nativeInvalid("History cursor is invalid")
		}
	}
	result, err := readProjectPolicy(ctx, query, scope)
	if err != nil {
		return result, err
	}
	rows, err := query.QueryContext(ctx, `SELECT a.id, a.repository, a.source_commit, a.previous_definition_digest, a.definition_digest, a.runner_id, a.applied_by, a.applied_at,
CASE WHEN ? THEN (SELECT r.metadata_json FROM policy_revisions r WHERE r.scope = a.scope AND json_extract(r.metadata_json, '$.source_digest') = a.previous_definition_digest ORDER BY r.approved_at DESC, r.policy_id LIMIT 1) END,
CASE WHEN ? THEN (SELECT r.metadata_json FROM policy_revisions r WHERE r.scope = a.scope AND json_extract(r.metadata_json, '$.source_digest') = a.definition_digest AND COALESCE(json_extract(r.metadata_json, '$.workflow.revision'), '') = a.source_commit ORDER BY r.approved_at DESC, r.policy_id LIMIT 1) END
FROM project_workflow_applies a WHERE a.scope = ? AND (? = 0 OR a.id < ?) ORDER BY a.id DESC LIMIT ?`, definitions, definitions, scope, before, before, limit+1)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var entry policy.WorkflowApply
		var previous, definition sql.NullString
		if err := rows.Scan(&entry.ID, &entry.Repository, &entry.Commit, &entry.PreviousDefinitionDigest, &entry.DefinitionDigest, &entry.RunnerID, &entry.AppliedBy, &entry.AppliedAt, &previous, &definition); err != nil {
			return result, err
		}
		if len(result.History) == limit {
			result.HistoryNext = strconv.FormatInt(result.History[limit-1].ID, 10)
			break
		}
		if previous.Valid {
			if err := json.Unmarshal([]byte(previous.String), &entry.PreviousDefinition); err != nil {
				return result, err
			}
		}
		if definition.Valid {
			if err := json.Unmarshal([]byte(definition.String), &entry.Definition); err != nil {
				return result, err
			}
		}
		result.History = append(result.History, entry)
	}
	return result, rows.Err()
}
