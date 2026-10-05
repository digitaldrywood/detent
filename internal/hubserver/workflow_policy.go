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

func (d *database) applyObservedDefaultBranchPolicy(ctx context.Context, scope, runner string, observation policy.Observation) (resultErr error) {
	source := observation.Source
	descriptor := observation.Descriptor
	if source == nil || descriptor.Workflow == nil || !source.DefaultBranchReachable ||
		!validCommitID(source.Commit) || !validCommitID(source.DefaultBranchHead) || source.DefaultBranch == "" ||
		source.Commit != descriptor.Workflow.Revision || strings.HasPrefix(scope, "repository:") {
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
	repository, err := policyRepository(ctx, tx, scope)
	if err != nil {
		return err
	}
	if repository == "" || !strings.EqualFold(repository, source.Repository) {
		return tx.Commit()
	}
	var current string
	err = tx.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope = ?", scope).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != descriptor.ID {
		if _, err := d.approvePolicyInTx(ctx, tx, scope, runner, policy.Change{ExpectedID: current, Policy: descriptor}); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	entry := policy.WorkflowApply{DefinitionDigest: descriptor.SourceDigest, AppliedBy: actor, AppliedAt: formatHubTime(now), Commit: descriptor.Workflow.Revision}
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

func readProjectPolicyWithHistory(ctx context.Context, query nativeQueryer, scope string, limit int, after string) (result policy.Approval, resultErr error) {
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
	rows, err := query.QueryContext(ctx, `SELECT id, repository, source_commit, previous_definition_digest, definition_digest, runner_id, applied_by, applied_at
FROM project_workflow_applies WHERE scope = ? AND (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`, scope, before, before, limit+1)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var entry policy.WorkflowApply
		if err := rows.Scan(&entry.ID, &entry.Repository, &entry.Commit, &entry.PreviousDefinitionDigest, &entry.DefinitionDigest, &entry.RunnerID, &entry.AppliedBy, &entry.AppliedAt); err != nil {
			return result, err
		}
		if len(result.History) == limit {
			result.HistoryNext = strconv.FormatInt(result.History[limit-1].ID, 10)
			break
		}
		result.History = append(result.History, entry)
	}
	return result, rows.Err()
}
