package postgres

import (
	"context"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) PutWorkItemFacts(ctx context.Context, facts claimstore.WorkItemFacts) error {
	if err := s.checkOrganization(facts.OrganizationID); err != nil {
		return err
	}
	if facts.ProjectID == "" || facts.WorkItemID == "" || facts.DispatchWaitSince != nil && facts.DispatchWaitSince.IsZero() {
		return claimstore.ErrInvalidCandidateFact
	}
	for _, raw := range facts.Assignees {
		if strings.TrimSpace(raw) == "" {
			return claimstore.ErrInvalidCandidateFact
		}
	}
	_, err := s.db.Exec(ctx, `INSERT INTO issue_claim_facts
 (organization_id,project_id,work_item_id,author_login,repository,assignees,dispatch_wait_since)
 VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET
 author_login=EXCLUDED.author_login,repository=EXCLUDED.repository,assignees=EXCLUDED.assignees,dispatch_wait_since=EXCLUDED.dispatch_wait_since`,
		s.organization, facts.ProjectID, facts.WorkItemID, strings.ToLower(strings.TrimSpace(facts.Author)), strings.ToLower(strings.TrimSpace(facts.Repository)), normalizedClaimValues(facts.Assignees), facts.DispatchWaitSince)
	if err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) CreateWorkItemWithClaimFacts(ctx context.Context, item corestore.WorkItem, labels []string, facts claimstore.WorkItemFacts, queue claimstore.QueueEntry) (corestore.WorkItem, error) {
	if item.OrganizationID != s.organization || facts.OrganizationID != s.organization || queue.OrganizationID != s.organization || item.ProjectID != facts.ProjectID || item.ProjectID != queue.ProjectID || item.NativeID != facts.WorkItemID || item.NativeID != queue.WorkItemID {
		return corestore.WorkItem{}, corestore.ErrTenantMismatch
	}
	created, err := s.CreateWorkItem(ctx, item)
	if err != nil {
		return corestore.WorkItem{}, err
	}
	if err := s.SetWorkItemLabels(ctx, item.ProjectID, item.NativeID, labels); err != nil {
		return corestore.WorkItem{}, err
	}
	if err := s.PutWorkItemFacts(ctx, facts); err != nil {
		return corestore.WorkItem{}, err
	}
	if err := s.PutQueueEntry(ctx, queue); err != nil {
		return corestore.WorkItem{}, err
	}
	return created, nil
}

func (s *session) WorkItemFacts(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (claimstore.WorkItemFacts, error) {
	var facts claimstore.WorkItemFacts
	var assignees []string
	var wait pgtype.Timestamptz
	err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,work_item_id,author_login,repository,assignees,dispatch_wait_since
	 FROM issue_claim_facts WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, project, item).
		Scan(&facts.OrganizationID, &facts.ProjectID, &facts.WorkItemID, &facts.Author, &facts.Repository, &assignees, &wait)
	if err != nil {
		return claimstore.WorkItemFacts{}, mapError(ctx, err)
	}
	facts.Assignees = []string(assignees)
	if wait.Valid {
		facts.DispatchWaitSince = &wait.Time
	}
	return facts, nil
}

func (s *session) QueueEntry(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, scope string) (claimstore.QueueEntry, error) {
	var entry claimstore.QueueEntry
	var priority pgtype.Int8
	err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,work_item_id,scope,state,rank,priority_override,created_at,updated_at
 FROM queue_entries WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND scope=$4`, s.organization, project, item, scope).
		Scan(&entry.OrganizationID, &entry.ProjectID, &entry.WorkItemID, &entry.Scope, &entry.State, &entry.Rank, &priority, &entry.CreatedAt, &entry.UpdatedAt)
	if err != nil {
		return claimstore.QueueEntry{}, mapError(ctx, err)
	}
	if priority.Valid {
		entry.Priority = &priority.Int64
	}
	return entry, nil
}

func (s *session) PutQueueEntry(ctx context.Context, entry claimstore.QueueEntry) error {
	if err := s.checkOrganization(entry.OrganizationID); err != nil {
		return err
	}
	if entry.ProjectID == "" || entry.WorkItemID == "" || strings.TrimSpace(entry.Scope) == "" || strings.TrimSpace(entry.State) == "" || entry.CreatedAt.IsZero() || entry.UpdatedAt.Before(entry.CreatedAt) || entry.Priority != nil && (*entry.Priority < 0 || *entry.Priority > 3) {
		return claimstore.ErrInvalidCandidateFact
	}
	_, err := s.db.Exec(ctx, `INSERT INTO queue_entries
 (organization_id,project_id,work_item_id,scope,state,rank,priority_override,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(organization_id,scope,work_item_id) DO UPDATE SET
 project_id=EXCLUDED.project_id,state=EXCLUDED.state,rank=EXCLUDED.rank,
 priority_override=EXCLUDED.priority_override,updated_at=EXCLUDED.updated_at`,
		s.organization, entry.ProjectID, entry.WorkItemID, entry.Scope, entry.State, entry.Rank, entry.Priority, entry.CreatedAt, entry.UpdatedAt)
	return mapError(ctx, err)
}

func (s *session) DeleteQueueEntry(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, scope string) error {
	if project == "" || item == "" || strings.TrimSpace(scope) == "" {
		return claimstore.ErrInvalidCandidateFact
	}
	result, err := s.db.Exec(ctx, `DELETE FROM queue_entries WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND scope=$4`, s.organization, project, item, scope)
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if count == 0 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) SelectionCursor(ctx context.Context, runnerID string, project tracker.ProjectID) (claimstore.SelectionCursor, error) {
	var cursor claimstore.SelectionCursor
	err := s.db.QueryRow(ctx, `SELECT organization_id,runner_id,project_id,source_revision,
 priority,state_rank,label_rank,aging_boost,landing_rank,lane_rank,unblocker_rank,rank,created_at,work_item_id,cursor_source_revision,revision
 FROM runner_selection_cursors WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runnerID, project).
		Scan(&cursor.OrganizationID, &cursor.RunnerID, &cursor.ProjectID, &cursor.SourceRevision,
			&cursor.Position.Priority, &cursor.Position.StateRank, &cursor.Position.LabelRank, &cursor.Position.AgingBoost,
			&cursor.Position.LandingRank, &cursor.Position.LaneRank, &cursor.Position.UnblockerRank, &cursor.Position.Rank,
			&cursor.Position.Created, &cursor.Position.WorkItemID, &cursor.Position.SourceRevision, &cursor.Revision)
	if err != nil {
		return claimstore.SelectionCursor{}, mapError(ctx, err)
	}
	return cursor, nil
}

func (s *session) PutSelectionCursor(ctx context.Context, cursor claimstore.SelectionCursor, expectedRevision int64) (bool, error) {
	if err := s.checkOrganization(cursor.OrganizationID); err != nil {
		return false, err
	}
	if cursor.RunnerID == "" || cursor.ProjectID == "" || expectedRevision < 0 || cursor.Revision != expectedRevision || len(cursor.SourceRevision) != 64 {
		return false, claimstore.ErrInvalidCandidateFact
	}
	if _, err := hex.DecodeString(cursor.SourceRevision); err != nil {
		return false, claimstore.ErrInvalidCandidateFact
	}
	if cursor.Position.WorkItemID == "" {
		if cursor.Position.SourceRevision != 0 {
			return false, claimstore.ErrInvalidCandidateFact
		}
	} else if cursor.Position.SourceRevision <= 0 || cursor.Position.Created.IsZero() {
		return false, claimstore.ErrInvalidCandidateFact
	}
	cursor.Revision = expectedRevision + 1
	result, err := s.db.Exec(ctx, `INSERT INTO runner_selection_cursors
 (organization_id,runner_id,project_id,source_revision,priority,state_rank,label_rank,aging_boost,landing_rank,lane_rank,unblocker_rank,rank,created_at,work_item_id,cursor_source_revision,revision)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
 ON CONFLICT(organization_id,runner_id,project_id) DO UPDATE SET
 source_revision=EXCLUDED.source_revision,priority=EXCLUDED.priority,state_rank=EXCLUDED.state_rank,label_rank=EXCLUDED.label_rank,
 aging_boost=EXCLUDED.aging_boost,landing_rank=EXCLUDED.landing_rank,lane_rank=EXCLUDED.lane_rank,
 unblocker_rank=EXCLUDED.unblocker_rank,rank=EXCLUDED.rank,created_at=EXCLUDED.created_at,work_item_id=EXCLUDED.work_item_id,
 cursor_source_revision=EXCLUDED.cursor_source_revision,revision=EXCLUDED.revision
 WHERE runner_selection_cursors.revision=$17`, s.organization, cursor.RunnerID, cursor.ProjectID, cursor.SourceRevision,
		cursor.Position.Priority, cursor.Position.StateRank, cursor.Position.LabelRank, cursor.Position.AgingBoost,
		cursor.Position.LandingRank, cursor.Position.LaneRank, cursor.Position.UnblockerRank, cursor.Position.Rank,
		recordTime(cursor.Position.Created), cursor.Position.WorkItemID, cursor.Position.SourceRevision, cursor.Revision, expectedRevision)
	if err != nil {
		return false, mapError(ctx, err)
	}
	rows := result.RowsAffected()
	return rows == 1, nil
}

func (s *session) CandidatePage(ctx context.Context, query claimstore.CandidateQuery) (claimstore.CandidatePage, error) {
	if query.ProjectID == "" || query.Limit < 1 || query.Limit > corestore.MaxPageSize || query.AvailableAt.IsZero() || len(query.WorkItemIDs) > 100 {
		return claimstore.CandidatePage{}, claimstore.ErrInvalidCandidateQuery
	}
	if query.AfterWorkItemID != "" {
		if query.After != (claimstore.CandidateCursor{}) || query.WorkItemID != "" || query.IncludeLeasedAnchor {
			return claimstore.CandidatePage{}, claimstore.ErrInvalidCandidateQuery
		}
		anchorQuery := query
		anchorQuery.WorkItemID = query.AfterWorkItemID
		anchorQuery.AfterWorkItemID = ""
		anchorQuery.IncludeLeasedAnchor = true
		anchorQuery.Limit = 1
		anchor, err := s.CandidatePage(ctx, anchorQuery)
		if err != nil {
			return claimstore.CandidatePage{}, err
		}
		if len(anchor.Items) != 1 {
			return claimstore.CandidatePage{}, corestore.ErrInvalidCursor
		}
		query.After = anchor.Next
	}
	args := []any{s.organization, query.ProjectID, query.Scope, query.Scope, query.AvailableAt}
	bind := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	statePriority := "0::bigint"
	if len(query.StatePriority) > 0 {
		statePriority = "COALESCE(array_position(" + bind(normalizedClaimValues(query.StatePriority)) + "::text[],lower(trim(ws.name))),2147483647)::bigint"
	}
	labelPriority := "0::bigint"
	if len(query.LabelPriority) > 0 {
		labelPriority = "COALESCE((SELECT min(array_position(" + bind(query.LabelPriority) + "::text[],l.label)) FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY(" + bind(query.LabelPriority) + "::text[])),2147483647)::bigint"
	}
	priority := "CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override+1 ELSE 5 END::bigint"
	rank := "COALESCE(q.rank,'')"
	created := "i.native_created_at"
	aging := "CASE WHEN ps.aging_enabled THEN -LEAST(3,GREATEST(0,FLOOR(EXTRACT(EPOCH FROM ($5-COALESCE(f.dispatch_wait_since,$5)))/86400)::bigint)) ELSE 0 END::bigint"
	landing := "CASE WHEN lower(trim(ws.name))='merging' THEN 0 ELSE 1 END::bigint"
	lane := "CASE lower(trim(ws.name)) WHEN 'merging' THEN 0 WHEN 'rework' THEN 1 WHEN 'in progress' THEN 2 ELSE 3 END::bigint"
	unblockers := `(SELECT count(*) FROM issue_dependencies d JOIN issues dependent ON dependent.organization_id=d.organization_id AND dependent.project_id=d.dependent_project_id AND dependent.id=d.dependent_issue_id JOIN project_workflow_states dependent_state ON dependent_state.organization_id=dependent.organization_id AND dependent_state.project_id=dependent.project_id AND dependent_state.name=dependent.workflow_state_name WHERE d.organization_id=i.organization_id AND d.blocker_project_id=i.project_id AND d.blocker_issue_id=i.id AND dependent.archived=false AND dependent_state.terminal=false)`
	unblockerRank := "0::bigint"
	if query.PrioritizeUnblockers {
		unblockerRank = "-(" + unblockers + ")"
	}
	order := priority + "," + aging + "," + landing + "," + statePriority + "," + labelPriority + "," + unblockerRank + "," + rank + "," + created + ",i.native_id"
	if query.RunnerSelection {
		order = lane + "," + priority + "," + aging + "," + statePriority + "," + labelPriority + "," + unblockerRank + "," + rank + "," + created + ",i.native_id"
	}
	workspaceLane := bind(query.WorkspaceLane)
	statement := `SELECT i.id,i.revision,i.number,i.native_id,lower(trim(ws.name)),f.author_login,f.repository,
	 f.assignees,
 ARRAY(SELECT l.label FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id ORDER BY l.label),
	 COALESCE(q.rank,''),` + priority + `,` + statePriority + `,` + labelPriority + `,` + aging + `,` + landing + `,` + lane + `,i.native_created_at,f.dispatch_wait_since,` + unblockers + `,` + unblockerRank + `
 FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
 JOIN issue_claim_facts f ON f.organization_id=i.organization_id AND f.project_id=i.project_id AND f.work_item_id=i.native_id
 LEFT JOIN LATERAL (SELECT e.scope,e.rank,e.priority_override FROM queue_entries e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND ($3='' OR e.scope=$4) ORDER BY CASE WHEN e.scope=$4 THEN 0 ELSE 1 END,e.scope LIMIT 1) q ON true
 WHERE i.organization_id=$1 AND i.project_id=$2 AND p.profile='native' AND p.deleted_at IS NULL AND i.archived=false AND ws.dispatchable=true AND ws.terminal=false AND lower(trim(ws.name))<>'cancelled'
 AND (NOT p.require_dependencies OR NOT EXISTS(SELECT 1 FROM issue_dependencies d JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id LEFT JOIN project_workflow_states blocker_state ON blocker_state.organization_id=blocker.organization_id AND blocker_state.project_id=blocker.project_id AND blocker_state.name=blocker.workflow_state_name WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id AND (blocker_state.name IS NULL OR blocker_state.terminal=false)))
 AND (q.scope IS NOT NULL OR $3='')
 AND (` + workspaceLane + ` OR NOT EXISTS(SELECT 1 FROM workspace_items wi WHERE wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id))`
	if !query.AvailableAt.IsZero() && !query.IncludeLeasedAnchor {
		statement += ` AND NOT EXISTS(SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>$5)`
	}
	filters := []struct {
		values []string
		expr   string
	}{
		{normalizedClaimValues(query.States), "lower(trim(ws.name))"},
		{normalizedClaimValues(query.Authors), "f.author_login"},
		{normalizedClaimValues(query.Repositories), "f.repository"},
	}
	var filterSQL strings.Builder
	for _, filter := range filters {
		if len(filter.values) > 0 {
			filterSQL.WriteString(" AND ")
			filterSQL.WriteString(filter.expr)
			filterSQL.WriteString("=ANY(")
			filterSQL.WriteString(bind(filter.values))
			filterSQL.WriteString("::text[])")
		}
	}
	statement += filterSQL.String()
	if len(query.Assignees) > 0 {
		statement += ` AND f.assignees && ` + bind(normalizedClaimValues(query.Assignees)) + `::text[]`
	}
	var labelSQL strings.Builder
	for _, label := range query.IncludeLabels {
		labelSQL.WriteString(` AND EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=`)
		labelSQL.WriteString(bind(label))
		labelSQL.WriteString(`)`)
	}
	statement += labelSQL.String()
	if len(query.ExcludeLabels) > 0 {
		statement += ` AND NOT EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY(` + bind(query.ExcludeLabels) + `::text[]))`
	}
	if query.WorkItemID != "" {
		statement += ` AND i.native_id=` + bind(query.WorkItemID)
	}
	if len(query.WorkItemIDs) > 0 {
		itemIDs := make([]string, len(query.WorkItemIDs))
		for i, id := range query.WorkItemIDs {
			itemIDs[i] = string(id)
		}
		statement += ` AND i.native_id=ANY(` + bind(itemIDs) + `::text[])`
	}
	if query.After.WorkItemID != "" {
		cursorOrder := strings.Join([]string{priority, aging, landing, statePriority, labelPriority, unblockerRank, rank, created, "i.native_id"}, ",")
		cursorValues := []any{query.After.Priority, query.After.AgingBoost, query.After.LandingRank, query.After.StateRank, query.After.LabelRank, query.After.UnblockerRank, query.After.Rank, query.After.Created, query.After.WorkItemID}
		if query.RunnerSelection {
			cursorOrder = strings.Join([]string{lane, priority, aging, statePriority, labelPriority, unblockerRank, rank, created, "i.native_id"}, ",")
			cursorValues = []any{query.After.LaneRank, query.After.Priority, query.After.AgingBoost, query.After.StateRank, query.After.LabelRank, query.After.UnblockerRank, query.After.Rank, query.After.Created, query.After.WorkItemID}
		}
		cursorPlaceholders := make([]string, len(cursorValues))
		for i, value := range cursorValues {
			cursorPlaceholders[i] = bind(value)
		}
		statement += ` AND (` + cursorOrder + `) > (` + strings.Join(cursorPlaceholders, ",") + `)`
	}
	statement += ` ORDER BY ` + order + ` LIMIT ` + bind(query.Limit)
	rows, err := s.db.Query(ctx, statement, args...)
	if err != nil {
		return claimstore.CandidatePage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := claimstore.CandidatePage{}
	for rows.Next() {
		var item claimstore.Candidate
		var assignees, labels []string
		var wait pgtype.Timestamptz
		if err := rows.Scan(&item.ID, &item.Revision, &item.Number, &item.WorkItemID, &item.State, &item.Author, &item.Repository, &assignees, &labels, &item.Rank, &item.Priority, &item.StateRank, &item.LabelRank, &item.AgingBoost, &item.LandingRank, &item.LaneRank, &item.CreatedAt, &wait, &item.Unblockers, &item.UnblockerRank); err != nil {
			return claimstore.CandidatePage{}, mapError(ctx, err)
		}
		item.OrganizationID = s.organization
		item.ProjectID = query.ProjectID
		item.Assignees = []string(assignees)
		item.Labels = []string(labels)
		if wait.Valid {
			value := wait.Time
			item.DispatchWaitSince = &value
		}
		page.Items = append(page.Items, item)
		page.Next = claimstore.CandidateCursor{Priority: item.Priority, StateRank: item.StateRank, LabelRank: item.LabelRank, AgingBoost: item.AgingBoost, LandingRank: item.LandingRank, LaneRank: item.LaneRank, UnblockerRank: item.UnblockerRank, Rank: item.Rank, Created: item.CreatedAt, WorkItemID: item.WorkItemID, SourceRevision: item.Revision}
	}
	if err := rows.Err(); err != nil {
		return claimstore.CandidatePage{}, mapError(ctx, err)
	}
	return page, nil
}

func (s *session) LandingCapacityUsage(ctx context.Context, project tracker.ProjectID, now time.Time) (claimstore.LandingCapacityUsage, error) {
	const statement = `SELECT
 count(*) FILTER (WHERE l.landing_capacity>0 OR lower(trim(COALESCE(i.workflow_state_name,'')))='merging' AND policy.gate_github_pull_request IS TRUE OR policy.gate_github_pull_request IS TRUE AND pending.landing_waiting AND pending.landing_change_id=change.id AND pending.landing_version_id=version.id AND pending.landing_head_sha=version.head_sha)::bigint,
	COALESCE(min(NULLIF(l.landing_capacity,0)) FILTER (WHERE l.landing_capacity>0 OR lower(trim(COALESCE(i.workflow_state_name,'')))='merging' AND policy.gate_github_pull_request IS TRUE OR policy.gate_github_pull_request IS TRUE AND pending.landing_waiting AND pending.landing_change_id=change.id AND pending.landing_version_id=version.id AND pending.landing_head_sha=version.head_sha),0)::bigint,
	COALESCE(bool_or(lower(trim(COALESCE(i.workflow_state_name,'')))='merging' AND change.id IS NOT NULL AND (version.id IS NULL OR policy.gate_github_pull_request IS NULL) OR pending.landing_waiting AND pending.landing_change_id=change.id AND pending.landing_version_id=version.id AND pending.landing_head_sha=version.head_sha AND policy.gate_github_pull_request IS NULL),false)
FROM leases l
JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id
LEFT JOIN LATERAL (
 SELECT c.id,c.current_version_id FROM change_issue_links link
 JOIN change_requests c ON c.organization_id=link.organization_id AND c.project_id=link.project_id AND c.id=link.change_id
 WHERE link.organization_id=i.organization_id AND link.project_id=i.project_id AND link.work_item_id=i.native_id
 ORDER BY link.created_at DESC,link.change_id DESC LIMIT 1
) change ON true
LEFT JOIN change_versions version ON version.organization_id=l.organization_id AND version.change_id=change.id AND version.id=change.current_version_id
LEFT JOIN policy_revisions policy ON policy.organization_id=l.organization_id AND policy.project_id=i.project_id AND policy.policy_id=version.policy_revision_id
LEFT JOIN LATERAL (
 SELECT event.landing_waiting,event.landing_change_id,event.landing_version_id,event.landing_head_sha
 FROM native_attempts attempt
 JOIN collaboration_events event ON event.organization_id=attempt.organization_id AND event.project_id=i.project_id AND event.attempt_id=attempt.id
 WHERE attempt.organization_id=l.organization_id AND attempt.issue_id=i.id AND attempt.lease_id=l.lease_id
 ORDER BY event.sequence DESC LIMIT 1
) pending ON true
WHERE l.organization_id=$1 AND i.project_id=$2 AND l.released_at IS NULL AND l.expires_at>$3`
	var usage claimstore.LandingCapacityUsage
	err := s.db.QueryRow(ctx, statement, s.organization, project, recordTime(now)).Scan(&usage.Active, &usage.Limit, &usage.Unknown)
	if err != nil {
		return claimstore.LandingCapacityUsage{}, mapError(ctx, err)
	}
	return usage, nil
}

func normalizedClaimValues(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			result = append(result, value)
		}
	}
	return result
}
