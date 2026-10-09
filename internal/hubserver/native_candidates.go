package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const nativeCandidateDependenciesSatisfied = `(p.require_dependencies = 0 OR NOT EXISTS (
 SELECT 1 FROM issue_dependencies dependency JOIN issues blocker ON blocker.id = dependency.blocker_issue_id
 LEFT JOIN workflow_states blocker_state ON blocker_state.id = blocker.workflow_state_id
 WHERE dependency.dependent_issue_id = i.id AND (blocker_state.id IS NULL OR blocker_state.terminal = 0)))`

const nativeCandidateRecordedDependenciesSatisfied = `NOT EXISTS (
 SELECT 1 FROM native_attempts reported
 WHERE reported.organization_id = i.organization_id AND reported.project_id = i.project_id
 AND reported.work_item_id = i.native_id AND reported.status = 'succeeded'
 AND reported.fencing_token = (SELECT max(latest.fencing_token) FROM native_attempts latest
   WHERE latest.organization_id = i.organization_id AND latest.project_id = i.project_id AND latest.work_item_id = i.native_id)
 AND json_extract(reported.data_json, '$.disposition.status') = 'blocked'
 AND EXISTS (SELECT 1 FROM json_each(reported.data_json, '$.disposition.blocker_evidence') blocker
   WHERE json_extract(blocker.value, '$.predicate.type') = 'issue_state')
 AND NOT EXISTS (SELECT 1 FROM collaboration_events recovered
   WHERE recovered.organization_id = i.organization_id AND recovered.project_id = i.project_id AND recovered.work_item_id = i.native_id
   AND recovered.type = 'workflow.transitioned' AND julianday(recovered.recorded_at) >= julianday(reported.updated_at)
   AND ((json_extract(recovered.data_json, '$.reason') = 'dependency_ready'
     AND json_extract(recovered.data_json, '$.blocker_attempt_id') = reported.id)
     OR (json_extract(recovered.data_json, '$.reason') = 'user_requested'
       AND json_extract(recovered.actor_json, '$.kind') = 'human'))))`

type nativeCandidateSnapshot struct {
	IDs                    []tracker.WorkItemID
	UnresolvedDependencies []tracker.NativeDependency
	Unavailable            []string
}

func readNativeCandidateSnapshot(ctx context.Context, q nativeQueryer, query claimCandidateQuery, issue tracker.NativeIssue) (nativeCandidateSnapshot, error) {
	ids, err := nativeCandidateIDs(ctx, q, query, nil, nil, nil, nil, nil, nil, nil)
	snapshot := nativeCandidateSnapshot{IDs: ids}
	if err != nil || len(ids) > 0 {
		return snapshot, err
	}
	var dependenciesSatisfied bool
	err = q.QueryRowContext(ctx, `SELECT `+nativeCandidateDependenciesSatisfied+`
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
WHERE i.id = ? AND i.organization_id = ? AND i.project_id = ?`, query.WorkItemID, query.NativeScope.organization, query.NativeScope.project).Scan(&dependenciesSatisfied)
	if err != nil {
		return snapshot, err
	}
	if !dependenciesSatisfied {
		for _, dependency := range issue.Blockers {
			if !dependency.Terminal {
				if len(snapshot.UnresolvedDependencies) == 100 {
					snapshot.Unavailable = append(snapshot.Unavailable, "unresolved_dependencies")
					break
				}
				snapshot.UnresolvedDependencies = append(snapshot.UnresolvedDependencies, dependency)
			}
		}
	}
	if len(snapshot.UnresolvedDependencies) > 0 {
		snapshot.Unavailable = append(snapshot.Unavailable, "other_native_candidate_exclusions")
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "native_candidate_exclusion")
	}
	return snapshot, nil
}

func nativeCandidateIDs(ctx context.Context, tx nativeQueryer, query claimCandidateQuery, repositoryIDs []tracker.RepositoryID, repositories, states, authors, assignees, included, excluded []string) ([]tracker.WorkItemID, error) {
	args := []any{}
	bind := func(value any) string {
		args = append(args, value)
		return "?"
	}
	var encodingErr error
	jsonList := func(value any) string {
		raw, err := marshalNative(value)
		if err != nil {
			encodingErr = err
		}
		return bind(raw)
	}
	statement := strings.Join([]string{`WITH candidates AS NOT MATERIALIZED (
SELECT i.id,
 CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override + 1 ELSE 5 END AS priority,
 substr(COALESCE(i.native_created_at, i.created_at), 1, 19) || '.' || substr(CASE
  WHEN substr(COALESCE(i.native_created_at, i.created_at), 20, 1) = '.'
  THEN substr(COALESCE(i.native_created_at, i.created_at), 21, length(COALESCE(i.native_created_at, i.created_at)) - 21) || '000000000'
  ELSE '000000000' END, 1, 9) AS created,
 p.scheduling_rank AS project_rank,
 CASE WHEN lower(trim(ws.detent_state)) = 'merging' THEN 0 ELSE 1 END AS landing,
 i.project_id || '#' || i.number AS identifier
FROM issues i
JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN repositories r ON r.id = i.repository_id
LEFT JOIN queue_entries q ON q.id = (
 SELECT candidate.id FROM queue_entries candidate WHERE candidate.issue_id = i.id
 AND (`,
		bind(query.Scope),
		` = '' OR candidate.scope = `,
		bind(query.Scope),
		`)
 ORDER BY CASE WHEN candidate.scope = `,
		bind(query.Scope),
		` THEN 0 ELSE 1 END, candidate.scope, candidate.id LIMIT 1)
WHERE i.organization_id = `,
		bind(query.NativeScope.organization),
		` AND p.profile = 'native'
 AND i.archived = 0 AND ws.terminal = 0 AND ws.dispatchable = 1 AND lower(trim(ws.detent_state)) <> 'cancelled'
 AND NOT EXISTS (SELECT 1 FROM github_imports g WHERE g.work_item_id = i.native_id AND g.intake_pending = 1)
 AND (`,
		bind(query.WorkspaceLane),
		` = 1 OR `,
		notWorkspaceItemClause,
		`)
 AND `,
		nativeCandidateDependenciesSatisfied,
		` AND `,
		nativeCandidateRecordedDependenciesSatisfied}, "")
	if query.NativeScope.project != "" {
		statement += ` AND i.project_id = ` + bind(query.NativeScope.project)
	} else {
		statement += ` AND i.project_id IN (SELECT project_id FROM token_grants WHERE token_id = ` + bind(query.NativeScope.credential.ID) + ` AND organization_id = ` + bind(query.NativeScope.organization) + `)`
	}
	if query.Scope != "" {
		statement += ` AND q.id IS NOT NULL`
	}
	if query.WorkItemID > 0 {
		statement += ` AND i.id = ` + bind(query.WorkItemID)
	}
	if len(query.OnlyIDs) > 0 {
		statement += ` AND i.id IN (SELECT value FROM json_each(` + jsonList(query.OnlyIDs) + `))`
	}
	if len(query.ProviderCandidates) > 0 {
		ids := make([]tracker.NativeWorkItemID, 0, len(query.ProviderCandidates))
		for _, candidate := range query.ProviderCandidates {
			ids = append(ids, candidate.WorkItemID)
		}
		statement += ` AND i.native_id IN (SELECT value FROM json_each(` + jsonList(ids) + `))`
	}
	var filters strings.Builder
	for _, filter := range []struct {
		expression string
		values     []string
	}{
		{"lower(trim(ws.detent_state))", states}, {"lower(trim(i.author_login))", authors},
		{"lower(trim(r.github_owner)) || '/' || lower(trim(r.github_name))", repositories},
	} {
		if len(filter.values) > 0 {
			filters.WriteString(` AND ` + filter.expression + ` IN (SELECT value FROM json_each(` + jsonList(filter.values) + `))`)
		}
	}
	statement += filters.String()
	if len(repositoryIDs) > 0 {
		statement += ` AND i.repository_id IN (SELECT value FROM json_each(` + jsonList(repositoryIDs) + `))`
	}
	if len(assignees) > 0 {
		statement += ` AND EXISTS (SELECT 1 FROM json_each(i.assignees_json) a WHERE lower(trim(a.value)) IN (SELECT value FROM json_each(` + jsonList(assignees) + `)))`
	}
	if len(included) > 0 {
		statement += ` AND NOT EXISTS (SELECT 1 FROM json_each(` + jsonList(included) + `) required WHERE NOT EXISTS (SELECT 1 FROM json_each(i.labels_json) l WHERE lower(trim(l.value)) = required.value))`
	}
	if len(excluded) > 0 {
		statement += ` AND NOT EXISTS (SELECT 1 FROM json_each(i.labels_json) l WHERE lower(trim(l.value)) IN (SELECT value FROM json_each(` + jsonList(excluded) + `)))`
	}
	if encodingErr != nil {
		return nil, encodingErr
	}
	statement += `)`
	order := "priority, landing, project_rank, created, identifier, id"
	baseArgs := args
	readPage := func(after tracker.WorkItemID, limit int, anchor bool) ([]tracker.WorkItemID, tracker.WorkItemID, int, error) {
		args = append([]any(nil), baseArgs...)
		page := statement + `, page AS MATERIALIZED (SELECT * FROM candidates WHERE 1 = 1`
		if anchor {
			page += ` AND id = ` + bind(after)
		} else {
			if after > 0 {
				page += ` AND (` + order + `) > (SELECT ` + order + ` FROM candidates WHERE id = ` + bind(after) + `)`
			}
			if !query.AvailableAt.IsZero() {
				page += ` AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id = candidates.id AND l.released_at IS NULL AND julianday(l.expires_at) > julianday(` + bind(formatHubTime(query.AvailableAt)) + `))`
			}
		}
		page += ` ORDER BY ` + order + ` LIMIT ` + bind(limit) + `)`
		return readNativeCandidatePage(ctx, tx, page+nativeCandidateCompletionEvidence+` ORDER BY page.`+strings.ReplaceAll(order, ", ", ", page."), args)
	}
	if query.After > 0 {
		ids, _, _, err := readPage(query.After, 1, true)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, providerWait("provider_candidate_changed", "Queue changed during provider selection; refresh the candidate page")
		}
	}
	limit := min(100, max(1, query.Limit))
	if query.Limit == 0 {
		limit = 100
	}
	var ids []tracker.WorkItemID
	after := query.After
	for len(ids) < limit {
		remaining := limit - len(ids)
		page, last, scanned, err := readPage(after, remaining, false)
		if err != nil {
			return nil, err
		}
		ids = append(ids, page...)
		if scanned < remaining {
			break
		}
		after = last
	}
	return ids, nil
}

const nativeCandidateCompletionEvidence = `
 SELECT page.id, ` + notAlreadyAnsweredClause + `, i.project_id,
 json_type(a.data_json, '$.disposition') = 'object',
 CASE WHEN COALESCE(json_type(a.data_json, '$.disposition'), 'null') = 'null'
   AND length(CAST(COALESCE(json_extract(a.data_json, '$.completion_body'), '') AS BLOB)) <= 65536
   THEN COALESCE(json_extract(a.data_json, '$.completion_body'), '') ELSE '' END AS completion_body,
 COALESCE(json_extract(a.checkpoint_json, '$.head_sha'), ''),
 COALESCE(json_extract(a.checkpoint_json, '$.workspace_digest'), '')
 FROM page JOIN issues i ON i.id = page.id
 JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
 JOIN workflow_states ws ON ws.id = i.workflow_state_id
 LEFT JOIN native_attempts a ON a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
 AND lower(trim(ws.detent_state)) <> 'merging'
 AND a.status = 'succeeded' AND a.work_item_revision >= i.revision AND a.dispatch_generation >= i.dispatch_generation
 AND a.fencing_token = (SELECT max(latest.fencing_token) FROM native_attempts latest
   WHERE latest.organization_id = a.organization_id AND latest.project_id = a.project_id AND latest.work_item_id = a.work_item_id)
 AND (COALESCE(json_type(a.data_json, '$.disposition'), 'null') = 'null'
   OR (json_extract(a.data_json, '$.disposition.status') = 'complete'
     AND json_extract(a.data_json, '$.disposition.blockers') = 0
     AND json_extract(a.data_json, '$.disposition.human_action') = 0
     AND COALESCE(json_extract(a.data_json, '$.disposition.reason_code'), '') = ''
     AND COALESCE(json_array_length(a.data_json, '$.disposition.blocker_evidence'), 0) = 0))
 AND json_extract(a.checkpoint_json, '$.worktree_state') IN ('dirty', 'unpushed')
 AND json_extract(a.checkpoint_json, '$.resume') = 'resume_session'
 AND json_extract(a.checkpoint_json, '$.availability') = 'available'
 AND json_extract(a.checkpoint_json, '$.storage') = 'local_only'
 AND json_extract(a.checkpoint_json, '$.external_effect') = 'none'
 AND json_extract(a.checkpoint_json, '$.effect_state') = 'none'
 AND COALESCE(json_extract(a.checkpoint_json, '$.effect_id'), '') = ''
 AND json_extract(a.checkpoint_json, '$.head_sha') IS NOT NULL
 AND json_extract(a.checkpoint_json, '$.workspace_digest') IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
   WHERE l.organization_id = a.organization_id AND l.project_id = a.project_id AND l.work_item_id = a.work_item_id
   AND json_extract(v.record_json, '$.head_sha') = json_extract(a.checkpoint_json, '$.head_sha'))`

func readNativeCandidatePage(ctx context.Context, q nativeQueryer, statement string, args []any) ([]tracker.WorkItemID, tracker.WorkItemID, int, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("query native candidates: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	var last tracker.WorkItemID
	scanned := 0
	for rows.Next() {
		var unanswered bool
		var project, body, head, digest string
		var typed sql.NullBool
		if err := rows.Scan(&last, &unanswered, &project, &typed, &body, &head, &digest); err != nil {
			return nil, 0, 0, err
		}
		scanned++
		if !unanswered {
			if !validCommitID(head) || !validCommitID(digest) {
				continue
			}
			if !typed.Bool {
				signal, reported := workpad.SignalFromComment(body, "", project)
				if !reported || signal == nil || signal.Invalid != nil || signal.Status != workpad.StatusComplete || len(signal.Blockers) != 0 ||
					signal.HumanAction != "" || signal.ReasonCode != "" || signal.Fields["completion_kind"] == "operational" {
					continue
				}
			}
		}
		ids = append(ids, last)
	}
	return ids, last, scanned, rows.Err()
}
