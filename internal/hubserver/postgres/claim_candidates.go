package postgres

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	facts.Assignees = assignees
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

const unclassifiedSource = `EXISTS(SELECT 1 FROM (SELECT a.checkpoint_preserves_source FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.checkpoint_object_key IS NOT NULL AND a.checkpoint_preserves_source IS DISTINCT FROM FALSE ORDER BY a.fencing_token DESC LIMIT 1) source WHERE source.checkpoint_preserves_source IS NULL)`

const currentDispatchSatisfied = `(lower(trim(ws.name)) NOT IN ('rework','merging') AND EXISTS(SELECT 1 FROM (SELECT a.id,a.status,a.work_item_revision,a.dispatch_generation,a.claim_disposition FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id ORDER BY a.fencing_token DESC LIMIT 1) latest
 WHERE latest.status='succeeded' AND latest.work_item_revision>=i.revision AND latest.dispatch_generation>=i.dispatch_generation
 AND latest.claim_disposition IS NOT NULL AND latest.claim_disposition<>'progress' AND (latest.claim_disposition<>'' OR i.dispatch_requested_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM (SELECT ev.landing_waiting FROM collaboration_events ev WHERE ev.organization_id=i.organization_id AND ev.project_id=i.project_id AND ev.work_item_id=i.native_id AND ev.attempt_id=latest.id ORDER BY ev.attempt_id,ev.sequence DESC LIMIT 1) run WHERE run.landing_waiting)))`

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
		anchorQuery.IncludeLeasedAnchor, anchorQuery.ExcludeSatisfiedAnchor = true, true
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
	const order = "(CASE WHEN @runner THEN lane ELSE 0 END),priority,aging,(CASE WHEN @runner THEN 0 ELSE landing END),created,state_rank,label_rank,unblocker_rank,rank,native_id"
	const statement = `WITH candidates AS (
 SELECT i.id,i.revision,i.number,i.native_id,lower(trim(ws.name)) AS state,f.author_login,f.repository,f.assignees,
 ARRAY(SELECT l.label FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id ORDER BY l.label) AS labels,
 COALESCE(q.rank,'') AS rank,CASE WHEN q.priority_override BETWEEN 0 AND 3 THEN q.priority_override+1 ELSE 5 END::bigint AS priority,
 CASE WHEN @rankStates THEN COALESCE(array_position(@statePriority::text[],lower(trim(ws.name))),2147483647) ELSE 0 END::bigint AS state_rank,
 CASE WHEN @rankLabels THEN COALESCE((SELECT min(array_position(@labelPriority::text[],lower(btrim(l.label)))) FROM issue_labels l
 WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND lower(btrim(l.label))=ANY(@labelPriority::text[])),2147483647) ELSE 0 END::bigint AS label_rank,
 CASE WHEN ps.aging_enabled THEN -LEAST(3,GREATEST(0,FLOOR(EXTRACT(EPOCH FROM (@availableAt::timestamptz-COALESCE(f.dispatch_wait_since,@availableAt)))/86400)::bigint)) ELSE 0 END AS aging,
 CASE WHEN lower(trim(ws.name))='merging' THEN 0 ELSE 1 END::bigint AS landing,
 CASE lower(trim(ws.name)) WHEN 'merging' THEN 0 WHEN 'rework' THEN 1 WHEN 'in progress' THEN 2 ELSE 3 END::bigint AS lane,
 i.native_created_at AS created,f.dispatch_wait_since,(SELECT count(*) FROM issue_dependencies d JOIN issues dependent ON dependent.organization_id=d.organization_id AND dependent.project_id=d.dependent_project_id AND dependent.id=d.dependent_issue_id JOIN project_workflow_states dependent_state ON dependent_state.organization_id=dependent.organization_id AND dependent_state.project_id=dependent.project_id AND dependent_state.name=dependent.workflow_state_name WHERE d.organization_id=i.organization_id AND d.blocker_project_id=i.project_id AND d.blocker_issue_id=i.id AND dependent.archived=false AND dependent_state.terminal=false) AS unblockers,
 (@unclassified AND ` + unclassifiedSource + `) AS source_unclassified
 FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 JOIN project_workflow_states ws ON ws.organization_id=i.organization_id AND ws.project_id=i.project_id AND ws.name=i.workflow_state_name
 JOIN issue_claim_facts f ON f.organization_id=i.organization_id AND f.project_id=i.project_id AND f.work_item_id=i.native_id
 LEFT JOIN LATERAL (SELECT e.scope,e.rank,e.priority_override FROM queue_entries e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND (@scope='' OR e.scope=@scope) ORDER BY CASE WHEN e.scope=@scope THEN 0 ELSE 1 END,e.scope LIMIT 1) q ON true
 WHERE i.organization_id=@org AND i.project_id=@project AND (p.profile='native' OR (p.profile='github_compatible' AND ps.repository_enabled AND lower(f.repository)=lower(ps.repository))) AND p.deleted_at IS NULL AND i.archived=false AND ws.dispatchable=true AND ws.terminal=false AND lower(trim(ws.name))<>'cancelled'
 AND (NOT p.require_dependencies OR NOT EXISTS(SELECT 1 FROM issue_dependencies d JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.project_id=d.blocker_project_id AND blocker.id=d.blocker_issue_id LEFT JOIN project_workflow_states blocker_state ON blocker_state.organization_id=blocker.organization_id AND blocker_state.project_id=blocker.project_id AND blocker_state.name=blocker.workflow_state_name WHERE d.organization_id=i.organization_id AND d.dependent_project_id=i.project_id AND d.dependent_issue_id=i.id AND (blocker_state.name IS NULL OR blocker_state.terminal=false)))
 AND NOT EXISTS(SELECT 1 FROM github_imports g WHERE g.organization_id=i.organization_id AND g.project_id=i.project_id AND g.work_item_id=i.native_id AND g.work_item_id<>'' AND g.intake_pending)
 AND (q.scope IS NOT NULL OR @scope='')
 AND ((@workspace AND EXISTS(SELECT 1 FROM workspace_items wi WHERE wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id AND wi.closed_at IS NULL))
 OR (NOT @workspace AND NOT EXISTS(SELECT 1 FROM workspace_items wi WHERE wi.organization_id=i.organization_id AND wi.project_id=i.project_id AND wi.work_item_id=i.native_id)))
 AND (@includeLeased OR NOT EXISTS(SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>@availableAt))
 AND (@unclassified OR NOT ` + unclassifiedSource + `) AND (NOT @excludeSatisfied OR NOT ` + currentDispatchSatisfied + `)
 AND (cardinality(@states::text[])=0 OR lower(trim(ws.name))=ANY(@states))
 AND (cardinality(@authors::text[])=0 OR f.author_login=ANY(@authors))
 AND (cardinality(@repositories::text[])=0 OR f.repository=ANY(@repositories))
 AND (NOT @filterAssignees OR f.assignees && @assignees::text[])
 AND NOT EXISTS (SELECT 1 FROM unnest(@includeLabels::text[]) required(label) WHERE NOT EXISTS
 (SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND lower(btrim(l.label))=required.label))
 AND NOT EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND lower(btrim(l.label))=ANY(@excludeLabels::text[]))
 AND (@item::text='' OR i.native_id=@item) AND (coalesce(cardinality(@items::text[]),0)=0 OR i.native_id=ANY(@items))
 ), ranked AS (SELECT candidates.*,CASE WHEN @prioritizeUnblockers THEN -unblockers ELSE 0 END AS unblocker_rank FROM candidates)
 SELECT id,revision,number,native_id,state,author_login,repository,assignees,labels,rank,priority,state_rank,label_rank,aging,landing,lane,created,dispatch_wait_since,unblockers,unblocker_rank,source_unclassified
 FROM ranked WHERE @afterItem::text='' OR (` + order + `) > (CASE WHEN @runner THEN @afterLane::bigint ELSE 0 END,@afterPriority::bigint,@afterAging::bigint,
 CASE WHEN @runner THEN 0 ELSE @afterLanding::bigint END,@afterCreated::timestamptz,@afterState::bigint,@afterLabel::bigint,@afterUnblocker::bigint,@afterRank::text,@afterItem)
 ORDER BY ` + order + ` LIMIT @limit`
	rows, err := s.db.Query(ctx, statement, pgx.NamedArgs{
		"org": s.organization, "project": query.ProjectID, "scope": query.Scope, "workspace": query.WorkspaceLane,
		"rankStates": len(query.StatePriority) > 0, "statePriority": normalizedClaimValues(query.StatePriority),
		"rankLabels": len(query.LabelPriority) > 0, "labelPriority": normalizedClaimValues(query.LabelPriority),
		"availableAt": query.AvailableAt, "unclassified": query.IncludeUnclassifiedSource, "includeLeased": query.IncludeLeasedAnchor,
		"excludeSatisfied": !query.IncludeLeasedAnchor && query.WorkItemID == "" || query.ExcludeSatisfiedAnchor,
		"states":           normalizedClaimValues(query.States), "authors": normalizedClaimValues(query.Authors), "repositories": normalizedClaimValues(query.Repositories),
		"filterAssignees": len(query.Assignees) > 0, "assignees": normalizedClaimValues(query.Assignees), "includeLabels": normalizedClaimValues(query.IncludeLabels), "excludeLabels": normalizedClaimValues(query.ExcludeLabels),
		"item": query.WorkItemID, "items": query.WorkItemIDs, "runner": query.RunnerSelection, "prioritizeUnblockers": query.PrioritizeUnblockers,
		"afterItem": query.After.WorkItemID, "afterLane": query.After.LaneRank, "afterPriority": query.After.Priority, "afterAging": query.After.AgingBoost,
		"afterLanding": query.After.LandingRank, "afterCreated": query.After.Created, "afterState": query.After.StateRank, "afterLabel": query.After.LabelRank,
		"afterUnblocker": query.After.UnblockerRank, "afterRank": query.After.Rank, "limit": query.Limit,
	})
	if err != nil {
		return claimstore.CandidatePage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := claimstore.CandidatePage{}
	for rows.Next() {
		var item claimstore.Candidate
		var assignees, labels []string
		var wait pgtype.Timestamptz
		if err := rows.Scan(&item.ID, &item.Revision, &item.Number, &item.WorkItemID, &item.State, &item.Author, &item.Repository, &assignees, &labels, &item.Rank, &item.Priority, &item.StateRank, &item.LabelRank, &item.AgingBoost, &item.LandingRank, &item.LaneRank, &item.CreatedAt, &wait, &item.Unblockers, &item.UnblockerRank, &item.SourceUnclassified); err != nil {
			return claimstore.CandidatePage{}, mapError(ctx, err)
		}
		item.OrganizationID = s.organization
		item.ProjectID = query.ProjectID
		item.Assignees = assignees
		item.Labels = labels
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
	return s.LandingCapacityUsageExcludingLease(ctx, project, now, "")
}

func (s *session) LandingCapacityUsageExcludingLease(ctx context.Context, project tracker.ProjectID, now time.Time, lease string) (claimstore.LandingCapacityUsage, error) {
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
 JOIN collaboration_events event ON event.organization_id=attempt.organization_id AND event.project_id=i.project_id AND event.work_item_id=i.native_id AND event.attempt_id=attempt.id
 WHERE attempt.organization_id=l.organization_id AND attempt.issue_id=i.id AND attempt.lease_id=l.lease_id
 ORDER BY event.sequence DESC LIMIT 1
) pending ON true
WHERE l.organization_id=$1 AND i.project_id=$2 AND l.released_at IS NULL AND l.expires_at>$3 AND l.lease_id<>$4`
	var usage claimstore.LandingCapacityUsage
	err := s.db.QueryRow(ctx, statement, s.organization, project, recordTime(now), lease).Scan(&usage.Active, &usage.Limit, &usage.Unknown)
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

func (s *session) QueueListPage(ctx context.Context, query claimstore.QueueListQuery) ([]claimstore.QueueListRow, error) {
	if query.ProjectID == "" || strings.TrimSpace(query.Scope) == "" || query.Limit < 1 || query.Limit > corestore.MaxPageSize+1 || query.After.Priority < 0 || query.After.Priority > 4 {
		return nil, claimstore.ErrInvalidCandidateQuery
	}
	if query.After.RankMissing < 0 || query.After.RankMissing > 1 || query.After.Number < 0 || query.After.WorkItemID != "" && !query.IdentifierOrder && query.After.Created.IsZero() {
		return nil, claimstore.ErrInvalidCandidateQuery
	}
	for _, values := range [][]string{query.States, query.Labels, query.Assignees, query.Repositories} {
		if len(values) > 32 || len(strings.Join(values, "\x00")) > 4096 {
			return nil, claimstore.ErrInvalidCandidateQuery
		}
	}
	if len(query.Priorities) > 32 {
		return nil, claimstore.ErrInvalidCandidateQuery
	}
	for _, priority := range query.Priorities {
		if priority < -1 || priority > 3 {
			return nil, claimstore.ErrInvalidCandidateQuery
		}
	}
	const queueOrder = "q.list_priority,q.list_rank_missing,q.list_rank,q.created_at,q.work_item_id"
	order := queueOrder
	where := "q.organization_id=@org AND q.project_id=@project AND q.scope=@scope"
	if query.After.WorkItemID != "" {
		where += " AND (" + queueOrder + ")>(@priority::integer,@rankMissing::integer,@rank::text,@created::timestamptz,@afterItem::text)"
	}
	source := "SELECT q.* FROM queue_entries q WHERE " + where + " ORDER BY " + queueOrder + " LIMIT @limit"
	from := "raw q LEFT JOIN LATERAL (SELECT selected.* FROM issues selected WHERE selected.organization_id=q.organization_id AND selected.project_id=q.project_id AND selected.native_id=q.work_item_id LIMIT 1) i ON true"
	positionID, positionNumber := "q.work_item_id", "coalesce(i.number,0)"
	if query.IdentifierOrder {
		where = "i.organization_id=@org AND i.project_id=@project"
		if query.After.WorkItemID != "" {
			where += " AND i.number>@number::bigint"
		}
		source = "SELECT i.* FROM issues i WHERE " + where + " ORDER BY i.number LIMIT @limit"
		from = "raw i LEFT JOIN LATERAL (SELECT selected.* FROM queue_entries selected WHERE selected.organization_id=i.organization_id AND selected.project_id=i.project_id AND selected.work_item_id=i.native_id AND selected.scope=@scope LIMIT 1) q ON true"
		order, positionID, positionNumber = "i.number", "i.native_id", "i.number"
	}
	statement := "WITH raw AS MATERIALIZED (" + source + ") SELECT coalesce(i.id,0),coalesce(i.revision,0)," + positionID + "," + positionNumber + `,coalesce(q.scope,''),coalesce(q.state,''),coalesce(q.rank,''),q.priority_override,coalesce(q.created_at,i.native_created_at,i.created_at,'epoch'::timestamptz),coalesce(q.updated_at,i.native_updated_at,i.updated_at,'epoch'::timestamptz),
 coalesce(i.id IS NOT NULL AND NOT i.archived
 AND (CASE WHEN @unqueued THEN q.scope IS NULL ELSE @identifier OR q.scope IS NOT NULL END)
 AND (@principal::text='' OR i.actor_principal_id=@principal)
 AND (coalesce(cardinality(@states::text[]),0)=0 OR i.workflow_state_name=ANY(@states))
 AND (cardinality(@repositories::text[])=0 OR f.repository=ANY(@repositories))
 AND (coalesce(cardinality(@priorities::bigint[]),0)=0 OR coalesce(q.priority_override,-1)=ANY(@priorities))
 AND (NOT @filterAssignees OR f.assignees && @assignees::text[])
 AND (coalesce(cardinality(@labels::text[]),0)=0 OR EXISTS(SELECT 1 FROM issue_labels l WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id AND l.label=ANY(@labels))),false)
 FROM ` + from + ` LEFT JOIN LATERAL (SELECT selected.* FROM issue_claim_facts selected WHERE selected.organization_id=i.organization_id AND selected.project_id=i.project_id AND selected.work_item_id=i.native_id LIMIT 1) f ON true ORDER BY ` + order
	rows, err := s.db.Query(ctx, statement, pgx.NamedArgs{
		"org": s.organization, "project": query.ProjectID, "scope": query.Scope, "limit": query.Limit,
		"priority": query.After.Priority, "rankMissing": query.After.RankMissing, "rank": query.After.Rank, "created": query.After.Created, "afterItem": query.After.WorkItemID, "number": query.After.Number,
		"unqueued": query.Unqueued, "identifier": query.IdentifierOrder, "principal": query.Principal, "states": query.States, "repositories": normalizedClaimValues(query.Repositories),
		"priorities": query.Priorities, "filterAssignees": len(query.Assignees) > 0, "assignees": normalizedClaimValues(query.Assignees), "labels": query.Labels,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []claimstore.QueueListRow{}
	for rows.Next() {
		var row claimstore.QueueListRow
		var priority pgtype.Int8
		row.Entry.OrganizationID, row.Entry.ProjectID = s.organization, query.ProjectID
		if err := rows.Scan(&row.ID, &row.Revision, &row.Entry.WorkItemID, &row.Position.Number, &row.Entry.Scope, &row.Entry.State, &row.Entry.Rank, &priority, &row.Entry.CreatedAt, &row.Entry.UpdatedAt, &row.Matches); err != nil {
			return nil, mapError(ctx, err)
		}
		row.Position.Priority = 4
		if priority.Valid {
			row.Entry.Priority = &priority.Int64
			row.Position.Priority = priority.Int64
		}
		row.Position.Rank = strings.TrimSpace(row.Entry.Rank)
		if row.Position.Rank == "" {
			row.Position.RankMissing = 1
		}
		row.Position.Created, row.Position.WorkItemID = row.Entry.CreatedAt, row.Entry.WorkItemID
		if query.Unqueued {
			row.Position.Stage = 1
		}
		out = append(out, row)
	}
	return out, mapError(ctx, rows.Err())
}
