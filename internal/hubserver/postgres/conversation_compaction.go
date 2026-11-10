package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ conversationstore.CompactionWriter = (*session)(nil)

const compactionActivity = `GREATEST(c.settled_at,c.updated_at,COALESCE(c.last_message_at,c.updated_at))`
const closedConversationCompaction = `c.deleted_at IS NULL AND c.status='settled' AND c.settled_at IS NOT NULL AND c.work_item_id IS NULL AND c.subject_work_item_id IS NULL
AND c.execution_status IN ('idle','completed','interrupted','failed')`
const conversationCompactionUnowned = `(SELECT m.id FROM conversation_messages m WHERE m.organization_id=c.organization_id AND m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','responding') ORDER BY m.id LIMIT 1) IS NULL
AND NOT EXISTS (SELECT 1 FROM conversation_questions q WHERE q.organization_id=c.organization_id AND q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=c.organization_id AND l.lease_id=c.execution_lease_id AND l.released_at IS NULL AND l.expires_at>$4)`

const compactionDue = `CASE WHEN c.compaction_content_scanned_sequence<c.event_seq THEN '-infinity'::timestamptz ELSE ` + compactionActivity + ` END`
const compactionCandidate = `c.deleted_at IS NULL AND (c.compaction_content_scanned_sequence<c.event_seq OR (` + closedConversationCompaction + ` AND c.compaction_scanned_sequence<c.event_seq))`

type conversationCompactionCursor struct {
	Dirty    bool
	Activity time.Time
	ID       string
}

func (s *session) CompactionCandidates(ctx context.Context, cursor string, before, now time.Time, limit int) (conversationstore.CompactionPage, error) {
	if before.IsZero() || now.Before(before) || limit < 1 || limit > conversationstore.CompactionConversations {
		return conversationstore.CompactionPage{}, corestore.ErrInvalidPage
	}
	var after conversationCompactionCursor
	var afterValue any
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &after) != nil || (!after.Dirty && after.Activity.IsZero()) || after.ID == "" {
			return conversationstore.CompactionPage{}, corestore.ErrInvalidPage
		}
		if after.Dirty {
			afterValue = pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
		} else {
			afterValue = after.Activity
		}
	}
	rows, err := s.db.Query(ctx, conversationSelect+` WHERE c.organization_id=$1 AND `+compactionCandidate+` AND (`+compactionDue+`)<$2
 AND ($3::timestamptz IS NULL OR ((`+compactionDue+`),c.id)>($3,$4)) ORDER BY (`+compactionDue+`),c.id LIMIT $5`, s.organization, before, afterValue, after.ID, limit+1)
	if err != nil {
		return conversationstore.CompactionPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.CompactionPage{}
	for rows.Next() {
		record, err := scanPGConversation(ctx, rows)
		if err != nil {
			return page, err
		}
		cursor := conversationCompactionCursor{Dirty: record.Compaction.ContentScannedSequence < record.EventSequence, ID: record.ID}
		if !cursor.Dirty {
			activity := record.UpdatedAt
			if record.SettledAt != nil && record.SettledAt.After(activity) {
				activity = *record.SettledAt
			}
			if record.LastMessageAt != nil && record.LastMessageAt.After(activity) {
				activity = *record.LastMessageAt
			}
			cursor.Activity = activity.UTC()
		}
		raw, err := json.Marshal(cursor)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, conversationstore.CompactionCandidate{Conversation: record, Cursor: base64.RawURLEncoding.EncodeToString(raw)})
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[len(page.Items)-1].Cursor
	}
	return page, nil
}

func (s *session) ConversationFullCompactionEligible(ctx context.Context, project tracker.ProjectID, id string, before, now time.Time) (bool, error) {
	var eligible bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversations c WHERE c.organization_id=$1 AND c.id=$2 AND c.project_id=$5 AND `+closedConversationCompaction+` AND `+compactionActivity+`<$3 AND `+conversationCompactionUnowned+`)`, s.organization, id, before, now, project).Scan(&eligible)
	return eligible, mapError(ctx, err)
}

func (s *session) UnprojectedConversationEvents(ctx context.Context, id string, limit int) (conversationstore.EventPage, error) {
	if limit < 1 || limit > conversationstore.CompactionBatch {
		return conversationstore.EventPage{}, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,conversation_id,seq,type,body_json,created_at,content_attachment_id FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND (query_metadata_version=0 OR compaction_group_key IS NULL) ORDER BY seq LIMIT $3`, s.organization, id, limit+1)
	if err != nil {
		return conversationstore.EventPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.EventPage{}
	for rows.Next() {
		var event conversationstore.Event
		if err := rows.Scan(&event.OrganizationID, &event.ConversationID, &event.Seq, &event.Type, &event.Data, &event.CreatedAt, &event.ContentReference); err != nil {
			return page, mapError(ctx, err)
		}
		page.Items = append(page.Items, event)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[len(page.Items)-1].Seq
	}
	return page, nil
}

func conversationEventMetadataArguments(event conversationstore.Event) ([]any, string, error) {
	m, err := conversationstore.ProjectEventMetadata(event.Data)
	if err != nil {
		return nil, "", fmt.Errorf("project conversation event metadata: %w", err)
	}
	group, err := m.CompactionGroup(event.Type)
	if err != nil {
		return nil, "", fmt.Errorf("group conversation event metadata: %w", err)
	}
	var linked *time.Time
	if m.LinkedAt != nil {
		at, err := time.Parse(time.RFC3339Nano, *m.LinkedAt)
		if err != nil {
			return nil, "", fmt.Errorf("parse conversation link time: %w", err)
		}
		linked = &at
	}
	return []any{m.MessageID, m.ObjectID, m.Role, m.Kind, m.AttemptID, m.Status, m.ThreadID, m.TurnID, m.Title, m.Visibility, m.Status, m.SubjectWorkItem, m.WorkItem, linked, m.DeltaOffset, m.DeltaBytes, m.MessageRevision, m.PreferencesKey, m.Model, m.Effort, m.Access, group}, group, nil
}

func (s *session) ProjectConversationEvents(ctx context.Context, id string, events []conversationstore.Event) error {
	if len(events) > conversationstore.CompactionBatch {
		return corestore.ErrInvalidPage
	}
	for _, event := range events {
		if event.OrganizationID != s.organization || event.ConversationID != id || event.Seq < 1 {
			return corestore.ErrInvalidPage
		}
		args, _, err := conversationEventMetadataArguments(event)
		if err != nil {
			return err
		}
		args = append(args, s.organization, id, event.Seq)
		if _, err := s.db.Exec(ctx, `UPDATE conversation_events SET query_metadata_version=1,
message_id=$1,object_id=$2,message_role=$3,message_kind=$4,attempt_id=$5,execution_status=$6,thread_id=$7,turn_id=$8,snapshot_title=$9,snapshot_visibility=$10,snapshot_status=$11,snapshot_subject_work_item_id=$12,snapshot_work_item_id=$13,snapshot_linked_at=$14,delta_offset=$15,delta_bytes=$16,message_revision=$17,snapshot_preferences_key=$18,snapshot_model=$19,snapshot_effort=$20,snapshot_access=$21,compaction_group_key=$22
WHERE organization_id=$23 AND conversation_id=$24 AND seq=$25 AND (query_metadata_version=0 OR compaction_group_key IS NULL)`, args...); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

const obsoleteConversationEvent = `e.query_metadata_version=1 AND (
(e.type='message.delta' AND EXISTS (SELECT 1 FROM conversation_messages m WHERE m.organization_id=e.organization_id AND m.conversation_id=e.conversation_id AND m.id=e.message_id AND m.delivery IN ('delivered','completed','interrupted','failed','rejected','unknown')))
OR ($4 AND (
e.type IN ('message.delta','heartbeat')
OR (e.type IN ('message.updated','execution.updated') AND e.compaction_group_key<>'' AND EXISTS (
 SELECT 1 FROM conversation_events n WHERE n.organization_id=e.organization_id AND n.conversation_id=e.conversation_id AND n.type=e.type AND n.compaction_group_key=e.compaction_group_key AND n.seq>e.seq))
OR (e.type='conversation.updated' AND e.compaction_group_key<>''
 AND e.compaction_group_key=(SELECT n.compaction_group_key FROM conversation_events n WHERE n.organization_id=e.organization_id AND n.conversation_id=e.conversation_id AND n.type=e.type AND n.seq>e.seq ORDER BY n.seq LIMIT 1)
 AND e.compaction_group_key=(SELECT n.compaction_group_key FROM conversation_events n WHERE n.organization_id=e.organization_id AND n.conversation_id=e.conversation_id AND n.type=e.type AND n.seq<e.seq ORDER BY n.seq DESC LIMIT 1)))))`

func (s *session) ObsoleteConversationEvents(ctx context.Context, id string, after, through int64, full bool, limit int) (conversationstore.CompactionEventPage, error) {
	if after < 0 || through < after || limit < 1 || limit > conversationstore.CompactionBatch {
		return conversationstore.CompactionEventPage{}, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT e.organization_id,e.conversation_id,e.seq,e.type,e.body_json,e.created_at,e.content_attachment_id,COALESCE((`+obsoleteConversationEvent+`),false) FROM conversation_events e WHERE e.organization_id=$1 AND e.conversation_id=$2 AND e.seq>$3 AND e.seq<=$6 ORDER BY e.seq LIMIT $5`, s.organization, id, after, full, limit+1, through)
	if err != nil {
		return conversationstore.CompactionEventPage{}, mapError(ctx, err)
	}
	defer rows.Close()
	page := conversationstore.CompactionEventPage{Complete: true}
	for rows.Next() {
		if page.ScannedCount == limit {
			page.Complete = false
			break
		}
		var event conversationstore.Event
		var obsolete bool
		if err := rows.Scan(&event.OrganizationID, &event.ConversationID, &event.Seq, &event.Type, &event.Data, &event.CreatedAt, &event.ContentReference, &obsolete); err != nil {
			return page, mapError(ctx, err)
		}
		page.ScannedCount++
		page.ScannedSequence = event.Seq
		if obsolete {
			page.Items = append(page.Items, event)
		}
	}
	return page, mapError(ctx, rows.Err())
}

func (s *session) CompactConversationEvents(ctx context.Context, project tracker.ProjectID, request conversationstore.CompactionRequest) (bool, error) {
	if request.Conversation.OrganizationID != s.organization || request.Conversation.ProjectID != project || len(request.Sequences) > conversationstore.CompactionBatch || request.Now.Before(request.Before) || request.Before.IsZero() {
		return false, corestore.ErrInvalidPage
	}
	current, err := s.LockConversation(ctx, project, request.Conversation.ID)
	if err != nil {
		return false, err
	}
	if current.EventSequence != request.Conversation.EventSequence || current.Revision != request.Conversation.Revision || current.Compaction != request.Conversation.Compaction {
		return false, nil
	}
	if request.Full {
		eligible, err := s.ConversationFullCompactionEligible(ctx, project, current.ID, request.Before, request.Now)
		if err != nil || !eligible {
			return false, err
		}
	}
	if request.State.ContentScannedSequence < current.Compaction.ContentScannedSequence || request.State.ContentScannedSequence > current.EventSequence || (request.State.ThroughSequence == 0) != (request.State.ContentReference == "") {
		return false, corestore.ErrInvalidPage
	}
	if request.State.ThroughSequence < current.Compaction.ThroughSequence || request.State.ThroughSequence > current.EventSequence || request.State.ScannedSequence < current.Compaction.ScannedSequence || request.State.ScannedSequence > current.EventSequence || (request.State.ThroughSequence > 0 && request.State.ContentReference == "") {
		return false, corestore.ErrInvalidPage
	}
	if len(request.Sequences) > 0 {
		previousScan, nextScan := current.Compaction.ContentScannedSequence, request.State.ContentScannedSequence
		if request.Full {
			previousScan, nextScan = current.Compaction.ScannedSequence, request.State.ScannedSequence
		}
		for _, sequence := range request.Sequences {
			if sequence <= previousScan || sequence > nextScan || sequence > request.State.ThroughSequence {
				return false, corestore.ErrInvalidPage
			}
		}
		result, err := s.db.Exec(ctx, `DELETE FROM conversation_events e WHERE organization_id=$1 AND conversation_id=$2 AND seq=ANY($3::bigint[]) AND (`+obsoleteConversationEvent+`)`, s.organization, current.ID, request.Sequences, request.Full)
		if err != nil {
			return false, mapError(ctx, err)
		}
		if result.RowsAffected() != int64(len(request.Sequences)) {
			return false, corestore.ErrRevisionConflict
		}
	}
	_, err = s.db.Exec(ctx, `UPDATE conversations SET compaction_through_sequence=$3,compaction_scanned_sequence=$4,compaction_content_id=$5,compaction_content_scanned_sequence=$6 WHERE organization_id=$1 AND id=$2`, s.organization, current.ID, request.State.ThroughSequence, request.State.ScannedSequence, request.State.ContentReference, request.State.ContentScannedSequence)
	return err == nil, mapError(ctx, err)
}
