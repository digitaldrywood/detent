package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const nativeCandidateDependenciesSatisfied = `(p.require_dependencies = 0 OR NOT EXISTS (
 SELECT 1 FROM issue_dependencies dependency JOIN issues blocker ON blocker.id = dependency.blocker_issue_id
 LEFT JOIN workflow_states blocker_state ON blocker_state.id = blocker.workflow_state_id
 WHERE dependency.dependent_issue_id = i.id AND (blocker_state.id IS NULL OR blocker_state.terminal = 0)))`

type nativeCandidateSnapshot struct {
	AnsweredExclusion      *tracker.NativeAnsweredExclusion
	StateExclusion         string
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
	var dispatchable, terminal bool
	var kind, attemptID string
	err = q.QueryRowContext(ctx, `SELECT `+nativeCandidateDependenciesSatisfied+`,
 CASE WHEN answered.id IS NULL THEN '' ELSE `+nativeAnsweredClassification+` END,
 COALESCE(answered.id, ''), ws.dispatchable, ws.terminal
FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
JOIN workflow_states ws ON ws.id = i.workflow_state_id
LEFT JOIN native_attempts answered ON answered.id = (SELECT answered.id `+nativeAnsweredAttemptFrom+`
 AND p.profile = 'native' AND lower(trim(ws.detent_state)) <> 'merging')
WHERE i.id = ? AND i.organization_id = ? AND i.project_id = ?`, query.WorkItemID, query.NativeScope.organization, query.NativeScope.project).Scan(&dependenciesSatisfied, &kind, &attemptID, &dispatchable, &terminal)
	if err != nil {
		return snapshot, err
	}
	if terminal {
		snapshot.StateExclusion = fmt.Sprintf("Native workflow state %s is terminal", issue.State)
	} else if !dispatchable {
		snapshot.StateExclusion = fmt.Sprintf("Native workflow state %s is not dispatchable", issue.State)
	}
	if kind == "" {
		legacy, err := readUnknownAnsweredClaim(ctx, q, query.WorkItemID)
		if err != nil {
			return snapshot, err
		}
		kind, attemptID = legacy.Kind, legacy.AttemptID
	}
	if kind != "" {
		snapshot.AnsweredExclusion = &tracker.NativeAnsweredExclusion{Kind: kind, AttemptID: attemptID}
	}
	if snapshot.AnsweredExclusion == nil {
		review, attempt, err := readPublishedReview(ctx, q, query.WorkItemID, time.Now().UTC())
		if err != nil {
			return snapshot, err
		}
		if review.AwaitingEvidence() {
			snapshot.AnsweredExclusion = &tracker.NativeAnsweredExclusion{Kind: string(gate.ReasonAutomatedReviewMissing), AttemptID: attempt}
		}
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
	if len(snapshot.UnresolvedDependencies) > 0 || snapshot.AnsweredExclusion != nil {
		snapshot.Unavailable = append(snapshot.Unavailable, "other_native_candidate_exclusions")
	} else {
		snapshot.Unavailable = append(snapshot.Unavailable, "native_candidate_exclusion")
	}
	return snapshot, nil
}

func nativeCandidateIDs(ctx context.Context, tx nativeQueryer, query claimCandidateQuery, repositoryIDs []tracker.RepositoryID, repositories, states, authors, assignees, included, excluded []string) ([]tracker.WorkItemID, error) {
	projectRanks, err := nativeCandidateProjectRanks(ctx, tx, query.NativeScope.credential.ID)
	if err != nil {
		return nil, err
	}
	args := []any{}
	bind := func(value any) string {
		args = append(args, value)
		return "?"
	}
	projectRank := "p.scheduling_rank"
	if len(projectRanks) > 0 {
		projects := make([]string, 0, len(projectRanks))
		for project := range projectRanks {
			projects = append(projects, string(project))
		}
		sort.Strings(projects)
		var rank strings.Builder
		rank.WriteString("CASE p.id")
		for _, project := range projects {
			rank.WriteString(" WHEN " + bind(project) + " THEN " + bind(projectRanks[tracker.ProjectID(project)]))
		}
		rank.WriteString(" ELSE p.scheduling_rank END")
		projectRank = rank.String()
	}
	now := query.AvailableAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	bindList := func(values []any) string {
		args = append(args, values...)
		return placeholders(len(values))
	}
	laneOrder := "CASE WHEN lower(trim(ws.detent_state)) = 'merging' THEN 0 ELSE 1 END"
	if query.NativeScope.credential.Runner.Valid() {
		laneOrder = "CASE lower(trim(ws.detent_state)) WHEN 'merging' THEN 0 WHEN 'rework' THEN 1 WHEN 'in progress' THEN 2 ELSE 3 END"
	}
	statement := strings.Join([]string{`WITH candidates AS NOT MATERIALIZED (
SELECT i.id, `, projectRank, ` AS project_rank,
 CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override + 1 ELSE 5 END AS priority,
 substr(COALESCE(i.native_created_at, i.created_at), 1, 19) || '.' || substr(CASE
  WHEN substr(COALESCE(i.native_created_at, i.created_at), 20, 1) = '.'
  THEN substr(COALESCE(i.native_created_at, i.created_at), 21, length(COALESCE(i.native_created_at, i.created_at)) - 21) || '000000000'
  ELSE '000000000' END, 1, 9) AS created,
 CASE WHEN p.aging_enabled = 1 THEN -min(3, max(0, CAST(julianday(`, bind(formatHubTime(now)), `) - julianday(COALESCE(i.dispatch_wait_since, `, bind(formatHubTime(now)), `)) AS INTEGER))) ELSE 0 END AS aging_boost,
 `, laneOrder, ` AS landing,
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
		` AND p.deleted_at IS NULL AND p.profile = 'native'
 AND i.archived = 0 AND ws.terminal = 0 AND ws.dispatchable = 1 AND lower(trim(ws.detent_state)) <> 'cancelled'
 AND NOT EXISTS (SELECT 1 FROM github_imports g WHERE g.work_item_id = i.native_id AND g.intake_pending = 1)
 AND (`,
		bind(query.WorkspaceLane),
		` = 1 OR `,
		notWorkspaceItemClause,
		`)
 AND `,
		nativeCandidateDependenciesSatisfied}, "")
	if query.NativeScope.project != "" {
		statement += ` AND i.project_id = ` + bind(query.NativeScope.project)
	} else {
		condition, grantArgs := query.NativeScope.credential.projectGrantSQL("i.organization_id", "i.project_id")
		statement += " AND (" + condition + ")"
		args = append(args, grantArgs...)
	}
	if query.Scope != "" {
		statement += ` AND q.id IS NOT NULL`
	}
	if query.WorkItemID > 0 {
		statement += ` AND i.id = ` + bind(query.WorkItemID)
	}
	if len(query.OnlyIDs) > 0 {
		statement += ` AND i.id IN (` + bindList(nativeCandidateValues(query.OnlyIDs)) + `)`
	}
	if len(query.ProviderCandidates) > 0 {
		ids := make([]any, 0, len(query.ProviderCandidates))
		for _, candidate := range query.ProviderCandidates {
			ids = append(ids, candidate.WorkItemID)
		}
		statement += ` AND i.native_id IN (` + bindList(ids) + `)`
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
			filters.WriteString(` AND ` + filter.expression + ` IN (` + bindList(nativeCandidateValues(filter.values)) + `)`)
		}
	}
	statement += filters.String()
	if len(repositoryIDs) > 0 {
		statement += ` AND i.repository_id IN (` + bindList(nativeCandidateValues(repositoryIDs)) + `)`
	}
	if len(assignees) > 0 {
		statement += ` AND EXISTS (SELECT 1 FROM issue_filter_values WHERE issue_id=i.id AND kind='assignee' AND value IN (` + bindList(nativeCandidateValues(assignees)) + `))`
	}
	filters.Reset()
	for _, label := range included {
		filters.WriteString(` AND EXISTS (SELECT 1 FROM issue_filter_values WHERE issue_id=i.id AND kind='label' AND value=` + bind(label) + `)`)
	}
	statement += filters.String()
	if len(excluded) > 0 {
		statement += ` AND NOT EXISTS (SELECT 1 FROM issue_filter_values WHERE issue_id=i.id AND kind='label' AND value IN (` + bindList(nativeCandidateValues(excluded)) + `))`
	}
	statement += `)`
	order := "priority, aging_boost, landing, project_rank, created, identifier, id"
	if query.NativeScope.credential.Runner.Valid() {
		order = "landing, project_rank, priority, aging_boost, created, identifier, id"
	}
	baseArgs := args
	readPage := func(after tracker.WorkItemID, limit int, anchor bool) ([]tracker.WorkItemID, tracker.WorkItemID, int, error) {
		prepared := claimPrepared(ctx)
		bounded := prepared != nil && !anchor && len(query.OnlyIDs) == 0
		if bounded {
			limit = min(limit, claimPreparationLimit-prepared.candidateRows)
			if limit <= 0 {
				return nil, 0, 0, nil
			}
		}

		args = append([]any(nil), baseArgs...)
		page := statement + `, page AS MATERIALIZED (SELECT * FROM candidates WHERE 1 = 1`
		if anchor {
			page += ` AND id = ` + bind(after)
		} else {
			if cursor := query.SelectionCursor; cursor != nil && cursor.ID > 0 {
				if query.NativeScope.credential.Runner.Valid() {
					page += ` AND (landing, priority, aging_boost, created, identifier, id) > (` + bind(cursor.Landing) + `,` + bind(cursor.Priority) + `,` + bind(cursor.Aging) + `,` + bind(cursor.Created) + `,` + bind(cursor.Identifier) + `,` + bind(cursor.ID) + `)`
				} else {
					page += ` AND (priority, aging_boost, landing, created, identifier, id) > (` + bind(cursor.Priority) + `,` + bind(cursor.Aging) + `,` + bind(cursor.Landing) + `,` + bind(cursor.Created) + `,` + bind(cursor.Identifier) + `,` + bind(cursor.ID) + `)`
				}
			} else if after > 0 {
				page += ` AND (` + order + `) > (SELECT ` + order + ` FROM candidates WHERE id = ` + bind(after) + `)`
			}
			if !query.AvailableAt.IsZero() {
				page += ` AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id = candidates.id AND l.released_at IS NULL AND julianday(l.expires_at) > julianday(` + bind(formatHubTime(query.AvailableAt)) + `))`
			}
		}
		page += ` ORDER BY ` + order + ` LIMIT ` + bind(limit) + `)`
		ids, last, scanned, err := readNativeCandidatePage(ctx, tx, page+nativeCandidateCompletionEvidence+` ORDER BY page.`+strings.ReplaceAll(order, ", ", ", page."), args, query.SelectionCursor)
		if bounded {
			prepared.candidateRows += scanned
		}
		return ids, last, scanned, err
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
	pageSize := 100
	if query.PageSize > 0 {
		pageSize = query.PageSize
	}
	limit := min(pageSize, max(1, query.Limit))
	if query.Limit == 0 {
		limit = pageSize
	}
	var ids []tracker.WorkItemID
	after := query.After
	scannedTotal := 0
	for len(ids) < limit {
		if claimPrepared(ctx) != nil && scannedTotal >= claimPreparationLimit {
			break
		}
		remaining := limit - len(ids)
		page, last, scanned, err := readPage(after, remaining, false)
		if err != nil {
			return nil, err
		}
		scannedTotal += scanned
		if query.SelectionCursor != nil {
			return page, nil
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
 SELECT page.id, page.priority, page.aging_boost, page.landing, page.created, page.identifier, ` + notAlreadyAnsweredClause + `, i.project_id,
 COALESCE(COALESCE(NULLIF(a.payload_object_key,''),a.data_json), '{}') AS completion_evidence, COALESCE(COALESCE(NULLIF(a.checkpoint_object_key,''),a.checkpoint_json),'null'), a.claim_facts_known
 FROM page JOIN issues i ON i.id = page.id
 JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
 JOIN workflow_states ws ON ws.id = i.workflow_state_id
 LEFT JOIN native_attempts a ON a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
 AND lower(trim(ws.detent_state)) <> 'merging'
 AND a.status = 'succeeded' AND a.work_item_revision >= i.revision AND a.dispatch_generation >= i.dispatch_generation
 AND a.fencing_token = (SELECT max(latest.fencing_token) FROM native_attempts latest
   WHERE latest.organization_id = a.organization_id AND latest.project_id = a.project_id AND latest.work_item_id = a.work_item_id)`

func readNativeCandidatePage(ctx context.Context, q nativeQueryer, statement string, args []any, cursor *runnerSelectionCursor) ([]tracker.WorkItemID, tracker.WorkItemID, int, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("query native candidates: %w", err)
	}
	defer rows.Close()
	var ids []tracker.WorkItemID
	var last tracker.WorkItemID
	scanned := 0
	var position runnerSelectionCursor
	for rows.Next() {
		var unanswered bool
		var project, runRaw, checkpointRaw string
		var known sql.NullInt64
		if err := rows.Scan(&last, &position.Priority, &position.Aging, &position.Landing, &position.Created, &position.Identifier, &unanswered, &project, &runRaw, &checkpointRaw, &known); err != nil {
			return nil, 0, 0, err
		}
		scanned++
		position.ID = last
		if !unanswered {
			continue
		}
		var run tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(runRaw), &run); err != nil {
			return nil, 0, 0, err
		}
		if !known.Valid && run.AttemptID != "" {
			var checkpoint *tracker.NativeCheckpoint
			if checkpoint, err = decodeRunCheckpoint(ctx, q, checkpointRaw); err != nil {
				return nil, 0, 0, err
			}
			if answeredClaimKind(run, checkpoint) != "" {
				continue
			}
		}
		if run.Disposition == nil {
			if err := decodeRunDetail(ctx, q, runRaw, &run); err != nil {
				return nil, 0, 0, err
			}
			if signal, reported := workpad.SignalFromComment(run.CompletionBody, "", project); reported && signal != nil && signal.Invalid == nil && (signal.HumanAction != "" || signal.ReasonCode == "permission_wait") {
				continue
			}
		}
		ids = append(ids, last)
	}
	if err := rows.Err(); err != nil {
		return nil, last, scanned, err
	}
	if err := rows.Close(); err != nil {
		return nil, last, scanned, err
	}
	runnable := ids[:0]
	for _, id := range ids {
		review, _, err := readPublishedReview(ctx, q, id, time.Now().UTC())
		if err != nil {
			return nil, last, scanned, err
		}
		if !review.AwaitingEvidence() {
			runnable = append(runnable, id)
		}
	}
	if cursor != nil {
		*cursor = position
	}
	return runnable, last, scanned, nil
}

func readPublishedReview(ctx context.Context, q nativeQueryer, id tracker.WorkItemID, now time.Time) (connector.ChangeReview, string, error) {
	var scope nativeScope
	var item, attempt, raw, checkpoint string
	err := q.QueryRowContext(ctx, `SELECT i.organization_id, i.project_id, i.native_id, a.id, COALESCE(NULLIF(a.payload_object_key,''),a.data_json), COALESCE(COALESCE(NULLIF(a.checkpoint_object_key,''),a.checkpoint_json), 'null')
 FROM issues i JOIN projects p ON p.id = i.project_id AND p.organization_id = i.organization_id
 JOIN workflow_states ws ON ws.id = i.workflow_state_id
 JOIN native_attempts a ON a.organization_id = i.organization_id AND a.project_id = i.project_id AND a.work_item_id = i.native_id
 WHERE i.id = ? AND p.profile = 'native' AND lower(trim(ws.detent_state)) <> 'merging'
 AND a.status = 'succeeded' AND a.work_item_revision >= i.revision AND a.dispatch_generation >= i.dispatch_generation
 AND a.fencing_token = (SELECT max(latest.fencing_token) FROM native_attempts latest WHERE latest.organization_id = i.organization_id AND latest.project_id = i.project_id AND latest.work_item_id = i.native_id)
 LIMIT 1`, id).Scan(&scope.organization, &scope.project, &item, &attempt, &raw, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return connector.ChangeReview{}, "", nil
	}
	if err != nil {
		return connector.ChangeReview{}, "", err
	}
	var data tracker.NativeRunData
	var source *tracker.NativeCheckpoint
	if err := decodeRunDetail(ctx, q, raw, &data); err != nil {
		return connector.ChangeReview{}, "", err
	}
	if source, err = decodeRunCheckpoint(ctx, q, checkpoint); err != nil {
		return connector.ChangeReview{}, "", err
	}
	disposition := data.Disposition
	if disposition == nil {
		signal, reported := workpad.SignalFromComment(data.CompletionBody, "", string(scope.project))
		if !reported || signal == nil || signal.Invalid != nil || signal.Fields["completion_kind"] == "operational" {
			return connector.ChangeReview{}, "", nil
		}
		disposition = &tracker.NativeDisposition{Status: signal.Status, Blockers: len(signal.Blockers) != 0, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode}
	}
	if disposition.Status != workpad.StatusComplete || disposition.Blockers || disposition.HumanAction || disposition.ReasonCode != "" || len(disposition.BlockerEvidence) != 0 {
		return connector.ChangeReview{}, "", nil
	}
	if source.WorktreeState != "clean" && source.WorktreeState != "unpushed" || !validCommitID(source.HeadSHA) {
		return connector.ChangeReview{}, "", nil
	}
	if source == nil {
		return connector.ChangeReview{}, "", nil
	}
	if source.EffectState == "pending" || source.EffectState == "ambiguous" {
		return connector.ChangeReview{}, "", nil
	}
	finalization := data.Finalization
	if finalization == nil || !finalization.Changed || finalization.ChangeID == "" || finalization.VersionID == "" || finalization.HeadSHA != source.HeadSHA || finalization.VersionError != "" {
		return connector.ChangeReview{}, "", nil
	}
	change, err := readChange(ctx, q, scope, item, finalization.ChangeID)
	if err != nil {
		return connector.ChangeReview{}, "", err
	}
	if change.CurrentVersion != finalization.VersionID {
		return connector.ChangeReview{}, "", nil
	}
	version, err := readClaimVersion(ctx, q, change.ID, change.CurrentVersion)
	if err != nil {
		return connector.ChangeReview{}, "", err
	}
	detail, err := readClaimLandingDetail(ctx, q, scope, change, version, now)
	if err != nil {
		return connector.ChangeReview{}, "", err
	}
	return changerequest.ReviewState(detail, finalization.VersionID, source.HeadSHA), attempt, nil
}

func nativeCandidateProjectRanks(ctx context.Context, q nativeQueryer, token string) (map[tracker.ProjectID]int, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT routing_settings_json FROM runner_identities WHERE token_id = ?", token).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return map[tracker.ProjectID]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	var settings runnerSettings
	if err := decodeStorageJSON(ctx, []byte(raw), &settings); err != nil {
		return nil, err
	}
	return settings.ProjectRanks, nil
}

func nativeCandidateValues[T ~string | ~int64](values []T) []any {
	args := make([]any, len(values))
	for index, value := range values {
		args[index] = value
	}
	return args
}
