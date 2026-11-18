package postgres

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/boardviewsql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ReplaceBoardItem(ctx context.Context, u viewstore.BoardUpdate) error {
	if u.ProjectID == "" || u.WorkItemID == "" || u.ExpectedRevision < 0 || u.ExpectedEventSequence < 0 {
		return viewstore.ErrInvalidView
	}
	var project corestore.Project
	if err := s.db.QueryRow(ctx, `SELECT content_settings_revision,profile,deleted_at FROM projects WHERE organization_id=$1 AND id=$2 FOR NO KEY UPDATE`, s.organization, u.ProjectID).Scan(&project.ContentSettingsRevision, &project.Profile, &project.DeletedAt); err != nil {
		return mapError(ctx, err)
	}
	if project.ContentSettingsRevision != u.ExpectedProjectRevision {
		return corestore.ErrRevisionConflict
	}
	row, err := s.queries.LockWorkItem(ctx, pgquery.LockWorkItemParams{OrganizationID: s.organization, ProjectID: u.ProjectID, NativeID: u.WorkItemID})
	item := corestore.WorkItem(row)
	err = mapError(ctx, err)
	if errors.Is(err, corestore.ErrNotFound) {
		if u.ExpectedRevision != 0 {
			return corestore.ErrRevisionConflict
		}
	} else if err != nil {
		return err
	} else if item.Revision != u.ExpectedRevision || item.EventSequence != u.ExpectedEventSequence {
		return corestore.ErrRevisionConflict
	}
	var next *viewstore.BoardRecord
	if u.Current != nil {
		if project.DeletedAt != nil || project.Profile != "native" {
			return corestore.ErrRevisionConflict
		}
		value := *u.Current
		if value.Display.Title == "" {
			value.Display.Title = item.Title
			value.Display.Number = int(item.Number)
			if item.ActorKind != nil {
				value.Display.Actor.Kind = *item.ActorKind
			}
		}
		if value.OrganizationID != s.organization {
			return corestore.ErrTenantMismatch
		}
		if value.ProjectID != u.ProjectID || value.WorkItemID != u.WorkItemID || value.SourceRevision != u.ExpectedRevision || value.SourceEventSequence != u.ExpectedEventSequence || value.SourceProjectRevision != u.ExpectedProjectRevision || u.ExpectedRevision == 0 {
			return viewstore.ErrInvalidView
		}
		if value.Payload.Validate(string(s.organization)) != nil || value.Payload.BindingID != "" || value.Payload.CustomerOwned || value.Payload.SchemaVersion != 1 {
			return viewstore.ErrInvalidView
		}
		if value.Position.State == "" || value.Position.Identifier == "" || len(value.Labels)+len(value.Assignees) > 256 || value.ExecutionFence < 0 {
			return viewstore.ErrInvalidView
		}
		unfilterable := func(value string) bool { return len(value) > viewstore.MaxFilterValueBytes }
		value.Labels = slices.DeleteFunc(append([]string{}, value.Labels...), unfilterable)
		slices.Sort(value.Labels)
		value.Labels = slices.Compact(value.Labels)
		value.Assignees = slices.DeleteFunc(append([]string{}, value.Assignees...), unfilterable)
		slices.Sort(value.Assignees)
		value.Assignees = slices.Compact(value.Assignees)
		next = &value
	}
	old, published, err := s.boardItemVersion(ctx, u.ProjectID, u.WorkItemID)
	var previous *viewstore.BoardRecord
	if err == nil {
		previous = &old
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if u.ExpectedProjection != nil && published != u.ExpectedProjection.Published {
		return corestore.ErrRevisionConflict
	}
	counted := false
	if previous != nil {
		if err := s.db.QueryRow(ctx, "SELECT workspace_counted FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, u.ProjectID, u.WorkItemID).Scan(&counted); err != nil {
			return mapError(ctx, err)
		}
	}
	same := sameBoardContent(previous, next)
	if !same {
		if err := s.replaceBoardSummary(ctx, previous, next); err != nil {
			return err
		}
	}
	if !same && previous != nil {
		result, err := s.db.Exec(ctx, "UPDATE native_board_counts SET total=total-1 WHERE organization_id=$1 AND project_id=$2 AND archived=$3 AND state=$4 AND total>0", s.organization, u.ProjectID, previous.Archived, previous.Position.State)
		if err != nil {
			return mapError(ctx, err)
		}
		changed := result.RowsAffected()
		if changed != 1 {
			return viewstore.ErrInvalidView
		}
	}
	if next == nil {
		if _, err := s.db.Exec(ctx, "DELETE FROM native_board_cards WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, u.ProjectID, u.WorkItemID); err != nil {
			return mapError(ctx, err)
		}
		if _, err := s.db.Exec(ctx, "DELETE FROM native_board_items WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, u.ProjectID, u.WorkItemID); err != nil {
			return mapError(ctx, err)
		}
	} else {
		if err := s.putBoardItem(ctx, *next, item.Number); err != nil {
			return err
		}
		if !same {
			if _, err := s.db.Exec(ctx, `INSERT INTO native_board_counts(organization_id,project_id,archived,state,total) VALUES($1,$2,$3,$4,1) ON CONFLICT(organization_id,project_id,archived,state) DO UPDATE SET total=native_board_counts.total+1`, s.organization, u.ProjectID, next.Archived, next.Position.State); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	if !same || !counted {
		if counted && previous != nil && slices.Contains(previous.Labels, "detent:workspace") {
			if err := s.adjustBoardWorkspaceCount(ctx, *previous, -1); err != nil {
				return err
			}
		}
		if next != nil && slices.Contains(next.Labels, "detent:workspace") {
			if err := s.adjustBoardWorkspaceCount(ctx, *next, 1); err != nil {
				return err
			}
		}
	}
	if !same {
		delta := viewstore.BoardDelta{WorkItemID: u.WorkItemID, Topic: "card", EntityID: string(u.WorkItemID), EntityRevision: new(u.ExpectedRevision), Kind: "upsert"}
		if boardRelationshipsOnly(previous, next) {
			delta.Status = viewstore.BoardDeltaRelationshipsOnly
		}
		if previous != nil {
			value := previous.Payload
			delta.Previous = &value
		}
		if next != nil {
			value := next.Payload
			delta.Current = &value
		} else {
			delta.Kind = "delete"
		}
		if err := s.recordBoardDelta(u.ProjectID, delta); err != nil {
			return err
		}
		if err := s.queueBoardNeighbors(ctx, previous, next); err != nil {
			return err
		}
	}
	return s.acknowledgeBoardRefresh(ctx, u)
}

func (s *session) acknowledgeBoardRefresh(ctx context.Context, u viewstore.BoardUpdate) error {
	const clearDirty = `DELETE FROM native_board_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`
	if u.ExpectedProjection == nil {
		_, err := s.db.Exec(ctx, clearDirty, s.organization, u.ProjectID, u.WorkItemID)
		return mapError(ctx, err)
	}
	var issue, dirty uint32
	err := s.db.QueryRow(ctx, `WITH acknowledged AS (`+clearDirty+` AND xmin=$4::xid RETURNING xmin)
 SELECT `+boardIssueVersion+`, COALESCE((SELECT xmin FROM acknowledged),'0'::xid)`, s.organization, u.ProjectID, u.WorkItemID, u.ExpectedProjection.Dirty).Scan(&issue, &dirty)
	if err != nil {
		return mapError(ctx, err)
	}
	if issue != u.ExpectedProjection.Issue || dirty != u.ExpectedProjection.Dirty {
		return corestore.ErrRevisionConflict
	}
	return nil
}

type boardAppend struct {
	project tracker.ProjectID
	delta   viewstore.BoardDelta
}

func (s *session) recordBoardDelta(project tracker.ProjectID, delta viewstore.BoardDelta) error {
	if !s.flushesBoardDeltas || s.readOnly || project == "" || delta.EntityID == "" || delta.EntityRevision != nil && *delta.EntityRevision < 0 {
		return viewstore.ErrInvalidView
	}
	if delta.Topic == "card" {
		if delta.WorkItemID == "" || delta.Previous == nil && delta.Current == nil {
			return viewstore.ErrInvalidView
		}
	} else if !slices.Contains([]string{"work_item", "board", "lane", "activity", "human_action", "health", "workspace", "action_run"}, delta.Topic) || delta.Previous != nil || delta.Current != nil {
		return viewstore.ErrInvalidView
	}
	s.boardDeltas = append(s.boardDeltas, boardAppend{project: project, delta: delta})
	return nil
}

const appendBoardDeltaSQL = `WITH head AS (INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES($1,$2,1,CASE WHEN $3 THEN 1 ELSE 0 END) ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=native_board_sequences.sequence+1,replay_floor=CASE WHEN $3 THEN native_board_sequences.sequence+1 ELSE native_board_sequences.replay_floor END RETURNING sequence)
INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,previous_object_key,previous_sha256,previous_bytes,previous_content_bytes,current_object_key,current_sha256,current_bytes,current_content_bytes,topic,entity_id,entity_revision,kind,status,related_id) SELECT $1,$2,head.sequence,$4,$5,$6,$7::bigint,$8::bigint,$9,$10,$11::bigint,$12::bigint,$13,$14,$15::bigint,$16,$17,$18 FROM head`

const boardSequenceHeadSQL = `SELECT sequence FROM native_board_sequences WHERE organization_id=$1 AND project_id=$2`

func boardDeltaPayload(delta viewstore.BoardDelta) []any {
	var beforeKey, beforeHash, afterKey, afterHash *string
	var beforeSize, beforeContent, afterSize, afterContent *int64
	if delta.Previous != nil {
		beforeKey, beforeHash = &delta.Previous.Key, &delta.Previous.SHA256
		beforeSize, beforeContent = &delta.Previous.CompressedSize, &delta.Previous.UncompressedSize
	}
	if delta.Current != nil {
		afterKey, afterHash = &delta.Current.Key, &delta.Current.SHA256
		afterSize, afterContent = &delta.Current.CompressedSize, &delta.Current.UncompressedSize
	}
	return []any{delta.WorkItemID, beforeKey, beforeHash, beforeSize, beforeContent, afterKey, afterHash, afterSize, afterContent, delta.Topic, delta.EntityID, delta.EntityRevision, delta.Kind, delta.Status, delta.RelatedID}
}

func (s *session) pendingBoardDeltas() []boardAppend {
	slices.SortStableFunc(s.boardDeltas, func(a, b boardAppend) int { return strings.Compare(string(a.project), string(b.project)) })
	lastHealth := make(map[[2]string]int)
	lastPolicy := make(map[tracker.ProjectID]int)
	for index, entry := range s.boardDeltas {
		if entry.delta.Topic == "health" {
			lastHealth[[2]string{string(entry.project), entry.delta.EntityID}] = index
		}
		if entry.delta.Topic == "board" && entry.delta.Kind == "invalidate" {
			lastPolicy[entry.project] = index
		}
	}
	pending := make([]boardAppend, 0, len(s.boardDeltas))
	for index, entry := range s.boardDeltas {
		if entry.delta.Topic == "health" && lastHealth[[2]string{string(entry.project), entry.delta.EntityID}] != index {
			continue
		}
		if entry.delta.Topic == "board" && entry.delta.Kind == "invalidate" && lastPolicy[entry.project] != index {
			continue
		}
		pending = append(pending, entry)
	}
	return pending
}

func (s *session) flushBoardDeltas(ctx context.Context) error {
	projects := []tracker.ProjectID{}
	for _, entry := range s.pendingBoardDeltas() {
		delta := entry.delta
		policyReset := delta.Topic == "board" && delta.Kind == "invalidate"
		var sequence int64
		if err := s.db.QueryRow(ctx, `INSERT INTO native_board_sequences(organization_id,project_id,sequence,replay_floor) VALUES($1,$2,1,CASE WHEN $3 THEN 1 ELSE 0 END) ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=native_board_sequences.sequence+1,replay_floor=CASE WHEN $3 THEN native_board_sequences.sequence+1 ELSE native_board_sequences.replay_floor END RETURNING sequence`, s.organization, entry.project, policyReset).Scan(&sequence); err != nil {
			return mapError(ctx, err)
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,previous_object_key,previous_sha256,previous_bytes,previous_content_bytes,current_object_key,current_sha256,current_bytes,current_content_bytes,topic,entity_id,entity_revision,kind,status,related_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, append([]any{s.organization, entry.project, sequence}, boardDeltaPayload(delta)...)...); err != nil {
			return mapError(ctx, err)
		}
		if len(projects) == 0 || projects[len(projects)-1] != entry.project {
			projects = append(projects, entry.project)
		}
	}
	for _, project := range projects {
		var head int64
		if err := s.db.QueryRow(ctx, boardSequenceHeadSQL, s.organization, project).Scan(&head); err != nil {
			return mapError(ctx, err)
		}
		if err := s.trimBoardDeltas(ctx, project, head); err != nil {
			return err
		}
	}
	s.boardDeltas = nil
	return nil
}

func (s *session) queueBoardDeltas(batch *pgx.Batch) func(context.Context) error {
	type boardHead struct {
		project  tracker.ProjectID
		sequence int64
	}
	heads := []*boardHead{}
	for _, entry := range s.pendingBoardDeltas() {
		delta := entry.delta
		batch.Queue(appendBoardDeltaSQL, append([]any{s.organization, entry.project, delta.Topic == "board" && delta.Kind == "invalidate"}, boardDeltaPayload(delta)...)...)
		if len(heads) == 0 || heads[len(heads)-1].project != entry.project {
			heads = append(heads, &boardHead{project: entry.project})
		}
	}
	for _, head := range heads {
		batch.Queue(boardSequenceHeadSQL, s.organization, head.project).QueryRow(func(row pgx.Row) error { return row.Scan(&head.sequence) })
	}
	s.boardDeltas = nil
	return func(ctx context.Context) error {
		for _, head := range heads {
			if err := s.trimBoardDeltas(ctx, head.project, head.sequence); err != nil {
				return err
			}
		}
		return nil
	}
}

func sameBoardContent(a, b *viewstore.BoardRecord) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if !sameBoardPageContent(a, b) {
		return false
	}
	left, right := *a, *b
	left.PageIssue, right.PageIssue = nil, nil
	left.ListIssue, right.ListIssue = nil, nil
	left.ReporterRelations, right.ReporterRelations = nil, nil
	left.ActorDisplayName, right.ActorDisplayName = nil, nil
	left.ProjectedAt, right.ProjectedAt = time.Time{}, time.Time{}
	left.RefreshAfter, right.RefreshAfter = nil, nil
	left.SourceRevision, left.SourceEventSequence, left.SourceProjectRevision = 0, 0, 0
	right.SourceRevision, right.SourceEventSequence, right.SourceProjectRevision = 0, 0, 0
	for _, record := range []*viewstore.BoardRecord{&left, &right} {
		record.Display = comparableBoardDisplay(record.Display)
		record.CreatedAt = record.CreatedAt.UTC().Truncate(time.Microsecond)
		record.UpdatedAt = record.UpdatedAt.UTC().Truncate(time.Microsecond)
		for _, at := range []**time.Time{&record.Position.Activity, &record.ClosedAt, &record.ExpiresAt} {
			if *at != nil {
				value := (*at).UTC().Truncate(time.Microsecond)
				*at = &value
			}
		}
	}
	return reflect.DeepEqual(left, right)
}

func (s *session) putBoardItem(ctx context.Context, b viewstore.BoardRecord, number int64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO native_board_items(organization_id,project_id,work_item_id,source_revision,source_event_sequence,source_project_revision,state,archived,terminal,running,dispatchable,lane_rank,priority_rank,activity_missing,activity_at,identifier,closed_at,expires_at,execution_fence,actor_principal_id,issue_type,verification_state,priority,created_at,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,issue_number,workspace_counted)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,true)
	 ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET workspace_counted=true,issue_number=excluded.issue_number,source_revision=excluded.source_revision,source_event_sequence=excluded.source_event_sequence,source_project_revision=excluded.source_project_revision,state=excluded.state,archived=excluded.archived,terminal=excluded.terminal,running=excluded.running,dispatchable=excluded.dispatchable,lane_rank=excluded.lane_rank,priority_rank=excluded.priority_rank,activity_missing=excluded.activity_missing,activity_at=excluded.activity_at,identifier=excluded.identifier,closed_at=excluded.closed_at,expires_at=excluded.expires_at,execution_fence=excluded.execution_fence,actor_principal_id=excluded.actor_principal_id,issue_type=excluded.issue_type,verification_state=excluded.verification_state,priority=excluded.priority,created_at=excluded.created_at,updated_at=excluded.updated_at,payload_object_key=excluded.payload_object_key,payload_sha256=excluded.payload_sha256,payload_bytes=excluded.payload_bytes,payload_content_bytes=excluded.payload_content_bytes`,
		s.organization, b.ProjectID, b.WorkItemID, b.SourceRevision, b.SourceEventSequence, b.SourceProjectRevision, b.Position.State, b.Archived, b.Terminal, b.Running, b.Dispatchable, b.Position.Lane, b.Position.Priority, b.Position.ActivityMissing, b.Position.Activity, b.Position.Identifier, b.ClosedAt, b.ExpiresAt, b.ExecutionFence, b.ActorPrincipalID, b.IssueType, b.VerificationState, b.Priority, b.CreatedAt, b.UpdatedAt, b.Payload.Key, b.Payload.SHA256, b.Payload.CompressedSize, b.Payload.UncompressedSize, number)
	if err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, "UPDATE native_board_items SET page_window_mask=$4,page_refresh_after=$5,page_list_bytes=$6 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, b.ProjectID, b.WorkItemID, b.WindowMask, b.RefreshAfter, b.ListBytes); err != nil {
		return mapError(ctx, err)
	}
	if err := s.putBoardPage(ctx, b); err != nil {
		return err
	}
	if err := s.putBoardDisplay(ctx, b); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, boardviewsql.CardUpsertSQL, s.organization, b.ProjectID, b.WorkItemID); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, "DELETE FROM native_board_filter_values WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, b.ProjectID, b.WorkItemID); err != nil {
		return mapError(ctx, err)
	}
	for kind, values := range map[string][]string{"label": b.Labels, "assignee": b.Assignees} {
		for _, value := range values {
			if value == "" || len(value) > viewstore.MaxFilterValueBytes {
				return viewstore.ErrInvalidView
			}
		}
		if len(values) == 0 {
			continue
		}
		if _, err := s.db.Exec(ctx, "INSERT INTO native_board_filter_values(organization_id,project_id,work_item_id,kind,value) SELECT $1,$2,$3,$4,unnest($5::text[])", s.organization, b.ProjectID, b.WorkItemID, kind, values); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) adjustBoardWorkspaceCount(ctx context.Context, b viewstore.BoardRecord, delta int64) error {
	result, err := s.db.Exec(ctx, "UPDATE native_board_counts SET workspace_total=workspace_total+$5 WHERE organization_id=$1 AND project_id=$2 AND archived=$3 AND state=$4", s.organization, b.ProjectID, b.Archived, b.Position.State, delta)
	if err != nil {
		return mapError(ctx, err)
	}
	changed := result.RowsAffected()
	if changed != 1 {
		return viewstore.ErrInvalidView
	}
	return nil
}

func comparableBoardDisplay(display viewstore.BoardDisplay) viewstore.BoardDisplay {
	if display.Provenance != nil {
		provenance := *display.Provenance
		provenance.CreatedAt = provenance.CreatedAt.UTC().Truncate(time.Microsecond)
		provenance.UpdatedAt = provenance.UpdatedAt.UTC().Truncate(time.Microsecond)
		provenance.ObservedAt = provenance.ObservedAt.UTC().Truncate(time.Microsecond)
		display.Provenance = &provenance
	}
	if display.Attempt != nil {
		attempt := *display.Attempt
		attempt.StartedAt = attempt.StartedAt.UTC().Truncate(time.Microsecond)
		attempt.ExpiresAt = attempt.ExpiresAt.UTC().Truncate(time.Microsecond)
		display.Attempt = &attempt
	}
	if display.Execution != nil {
		execution := *display.Execution
		execution.ExpiresAt = execution.ExpiresAt.UTC().Truncate(time.Microsecond)
		if execution.StartedAt != nil {
			at := execution.StartedAt.UTC().Truncate(time.Microsecond)
			execution.StartedAt = &at
		}
		display.Execution = &execution
	}
	return display
}

func (s *session) trimBoardDeltas(ctx context.Context, project tracker.ProjectID, head int64) error {
	if head <= 2048 {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT ctid,sequence FROM native_board_deltas WHERE organization_id=$1 AND project_id=$2 AND (organization_id,project_id,sequence)>($1,$2,0::bigint) AND (organization_id,project_id,sequence)<=($1,$2,$3) ORDER BY sequence LIMIT 32`, s.organization, project, head-2048)
	if err != nil {
		return mapError(ctx, err)
	}
	type candidate struct {
		Location pgtype.TID
		Sequence int64
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByPos[candidate])
	if err != nil {
		return mapError(ctx, err)
	}
	var floor int64
	for _, row := range candidates {
		var removed int64
		err := s.db.QueryRow(ctx, `DELETE FROM native_board_deltas WHERE ctid=$3 AND organization_id=$1 AND project_id=$2 AND sequence=$4 RETURNING sequence`, s.organization, project, row.Location, row.Sequence).Scan(&removed)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return mapError(ctx, err)
		}
		floor = max(floor, removed)
	}
	if floor == 0 {
		return nil
	}
	_, err = s.db.Exec(ctx, `UPDATE native_board_sequences SET replay_floor=greatest(replay_floor,$3) WHERE organization_id=$1 AND project_id=$2`, s.organization, project, floor)
	return mapError(ctx, err)
}

func (s *session) TrimBoardDeltas(ctx context.Context, after tracker.ProjectID) (tracker.ProjectID, error) {
	if s.readOnly {
		return after, viewstore.ErrInvalidView
	}
	var project tracker.ProjectID
	err := s.db.QueryRow(ctx, `SELECT project_id FROM native_board_sequences WHERE organization_id=$1 AND project_id>$2 ORDER BY project_id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.organization, after).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return after, mapError(ctx, err)
	}
	var head int64
	if err := s.db.QueryRow(ctx, boardSequenceHeadSQL, s.organization, project).Scan(&head); err != nil {
		return after, mapError(ctx, err)
	}
	return project, s.trimBoardDeltas(ctx, project, head)
}
