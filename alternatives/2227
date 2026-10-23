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
	"github.com/digitaldrywood/detent/internal/tracker"
)

type policyLanding struct {
	ChangeID       string
	PullRequestURL string
	Actor          string
}

func readPolicyLanding(ctx context.Context, query policyQuerier, scope string, source *policy.RepositorySource) (policyLanding, error) {
	var landing policyLanding
	if source == nil {
		return landing, nil
	}
	organization, project, native := strings.Cut(scope, "/")
	if native && !strings.HasPrefix(scope, "repository:") {
		var raw string
		err := query.QueryRowContext(ctx, `SELECT c.record_json FROM change_requests c
JOIN change_versions v ON v.change_id = c.id AND v.id = json_extract(c.record_json, '$.landed.version_id')
WHERE c.organization_id = ? AND c.project_id = ?
AND json_extract(c.record_json, '$.landed.merge_sha') = ?
AND json_extract(c.record_json, '$.landed.base_ref') = ?
ORDER BY c.id LIMIT 1`, organization, project, source.Commit, source.DefaultBranch).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return landing, err
		}
		if err == nil {
			var change tracker.ChangeRequest
			if err := json.Unmarshal([]byte(raw), &change); err != nil {
				return landing, err
			}
			var repository string
			if err := query.QueryRowContext(ctx, "SELECT json_extract(record_json, '$.repository') FROM change_versions WHERE id = ?", change.Landed.VersionID).Scan(&repository); err != nil {
				return landing, err
			}
			if strings.EqualFold(validationRepository(repository), source.Repository) {
				return policyLanding{ChangeID: change.ID, Actor: change.Landed.Actor.PrincipalID}, nil
			}
		}
	}
	err := query.QueryRowContext(ctx, `SELECT pr.url, pr.merged_by FROM pull_requests pr
JOIN repositories r ON r.id = pr.repository_id
WHERE r.github_owner || '/' || r.github_name = ? COLLATE NOCASE
AND pr.merge_sha = ? AND pr.base_ref = ? AND pr.merged_at <> '' AND pr.github_state = 'closed'
AND (? LIKE 'repository:%' OR EXISTS (SELECT 1 FROM projects p WHERE p.organization_id = ? AND p.id = ? AND p.repository_id = r.id))
ORDER BY pr.id LIMIT 1`, source.Repository, source.Commit, source.DefaultBranch, scope, organization, project).Scan(&landing.PullRequestURL, &landing.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		return policyLanding{}, nil
	}
	return landing, err
}

func (d *database) applyObservedDefaultBranchPolicy(ctx context.Context, scope string, observation policy.Observation) (resultErr error) {
	source := observation.Source
	descriptor := observation.Descriptor
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	approval, _, err := readProjectPolicy(ctx, tx, scope)
	if err != nil {
		return err
	}
	current := approval.Policy.ID
	if current != "" && current != descriptor.ID {
		var previouslyApplied bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM policy_revisions WHERE scope = ? AND policy_id = ? AND approved_by <> 'runner-report')", scope, descriptor.ID).Scan(&previouslyApplied); err != nil {
			return err
		}
		if previouslyApplied {
			return d.commit(ctx, tx)
		}
	}
	var landing policyLanding
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
		landing, err = readPolicyLanding(ctx, tx, scope, source)
		if err != nil {
			return err
		}
		if !source.DefaultBranchReachable && landing.ChangeID == "" && landing.PullRequestURL == "" && !descriptor.SameAuthoredInputs(approval.Policy) {
			return d.commit(ctx, tx)
		}
	}
	if current != descriptor.ID {
		actor := "repository-default-branch"
		if landing.Actor != "" {
			actor = landing.Actor
		} else if descriptor.SameAuthoredInputs(approval.Policy) && approval.ApprovedBy != "" {
			actor = approval.ApprovedBy
		}
		if _, err := d.approvePolicyInTx(ctx, tx, scope, actor, policy.Change{ExpectedID: current, Policy: descriptor}, landing); err != nil {
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

func recordWorkflowApply(ctx context.Context, tx *sql.Tx, scope, actor, current string, descriptor policy.Descriptor, now time.Time, landing policyLanding) error {
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
	if strings.HasPrefix(actor, "runner_") && landing.ChangeID == "" && landing.PullRequestURL == "" {
		entry.RunnerID = actor
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_workflow_applies
(scope, repository, source_commit, previous_definition_digest, definition_digest, runner_id, applied_by, applied_at, change_id, pull_request_url)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope, entry.Repository, entry.Commit, entry.PreviousDefinitionDigest, entry.DefinitionDigest, entry.RunnerID, entry.AppliedBy, entry.AppliedAt, landing.ChangeID, landing.PullRequestURL)
	return err
}

func recordPolicyLandingProvenance(ctx context.Context, tx *sql.Tx, scope string, repositoryID int64, repository, commit string, landing policyLanding) error {
	if !validCommitID(commit) || repository == "" || landing.ChangeID == "" && landing.PullRequestURL == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE project_workflow_applies SET change_id = ?, pull_request_url = ?
WHERE source_commit = ? AND change_id = '' AND pull_request_url = '' AND repository = ? COLLATE NOCASE
AND (scope = ? OR scope IN (SELECT organization_id || '/' || id FROM projects WHERE repository_id = ?))`, landing.ChangeID, landing.PullRequestURL, commit, repository, scope, repositoryID)
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
		return result, nativePublicInvalid("History page limit is invalid")
	}
	var before int64
	if after != "" {
		var err error
		before, err = strconv.ParseInt(after, 10, 64)
		if err != nil || before < 1 {
			return result, nativePublicInvalid("History cursor is invalid")
		}
	}
	result, _, err := readProjectPolicy(ctx, query, scope)
	if err != nil {
		return result, err
	}
	rows, err := query.QueryContext(ctx, `SELECT a.id, a.repository, a.source_commit, a.previous_definition_digest, a.definition_digest, a.runner_id, a.applied_by, a.applied_at, a.change_id, a.pull_request_url,
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
		if err := rows.Scan(&entry.ID, &entry.Repository, &entry.Commit, &entry.PreviousDefinitionDigest, &entry.DefinitionDigest, &entry.RunnerID, &entry.AppliedBy, &entry.AppliedAt, &entry.ChangeID, &entry.PullRequestURL, &previous, &definition); err != nil {
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
