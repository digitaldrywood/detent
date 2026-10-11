package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const eventRetention = 30 * 24 * time.Hour
const eventCompactionBatch = 500

const compactableWork = `i.archived=1
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE((SELECT scanned_sequence FROM event_compactions c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.aggregate_kind='work_item' AND c.aggregate_id=i.native_id),0)<i.event_sequence`

const compactableConversation = `c.status='settled'
AND max(julianday(c.settled_at),julianday(c.updated_at),julianday(COALESCE(c.last_message_at,c.updated_at)))<julianday(?)
AND json_extract(c.execution_json,'$.status') IN ('idle','completed','interrupted','failed')
AND NOT EXISTS (SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','responding'))
AND NOT EXISTS (SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))
AND NOT EXISTS (SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND (i.native_id=c.work_item_id OR l.lease_id=json_extract(c.execution_json,'$.lease_id')) AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE((SELECT scanned_sequence FROM event_compactions s WHERE s.organization_id=c.organization_id AND s.project_id=c.project_id AND s.aggregate_kind='conversation' AND s.aggregate_id=c.id),0)<c.event_seq`

type eventCompactionCandidate struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	kind, id     string
	head         int64
}

func readEventCompaction(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, project tracker.ProjectID, kind, id string) (tracker.EventCompaction, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT summary_json FROM event_compactions WHERE organization_id=? AND project_id=? AND aggregate_kind=? AND aggregate_id=?`, organization, project, kind, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.EventCompaction{}, nil
	}
	if err != nil {
		return tracker.EventCompaction{}, err
	}
	var summary tracker.EventCompaction
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return tracker.EventCompaction{}, err
	}
	return summary, nil
}

func compactEvents(ctx context.Context, tx *sql.Tx, now time.Time) ([]string, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM (SELECT i.organization_id,i.project_id,'work_item',i.native_id,i.event_sequence FROM issues i WHERE `+compactableWork+` LIMIT 4)
UNION ALL SELECT * FROM (SELECT c.organization_id,c.project_id,'conversation',c.id,c.event_seq FROM conversations c WHERE `+compactableConversation+` LIMIT 4)`, formatHubTime(now), formatHubTime(now.Add(-eventRetention)), formatHubTime(now))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var candidates []eventCompactionCandidate
	for rows.Next() {
		var candidate eventCompactionCandidate
		if err := rows.Scan(&candidate.organization, &candidate.project, &candidate.kind, &candidate.id, &candidate.head); err != nil {
			return nil, 0, err
		}
		candidates = append(candidates, candidate)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	if len(candidates) == 0 {
		return nil, 0, nil
	}
	var captureSize bool
	if err := tx.QueryRowContext(ctx, `SELECT event_compaction_storage_before='' FROM attempt_diff_body_migration WHERE singleton=1`).Scan(&captureSize); err != nil {
		return nil, 0, err
	}
	var before string
	if captureSize {
		size, err := tenantEventStorage(ctx, tx)
		if err != nil {
			return nil, 0, err
		}
		before, err = marshalNative(size)
		if err != nil {
			return nil, 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER collaboration_events_no_delete`); err != nil {
		return nil, 0, err
	}
	var conversations []string
	var removed int64
	for _, candidate := range candidates {
		count, err := compactEventAggregate(ctx, tx, candidate, now)
		if err != nil {
			return nil, 0, err
		}
		removed += count
		if count > 0 && candidate.kind == "conversation" {
			conversations = append(conversations, candidate.id)
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END`); err != nil {
		return nil, 0, err
	}
	if removed > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE attempt_diff_body_migration SET vacuum_pending=CASE WHEN event_compaction_storage_before='' THEN 1 ELSE vacuum_pending END,event_compaction_storage_before=CASE WHEN event_compaction_storage_before='' THEN ? ELSE event_compaction_storage_before END WHERE singleton=1`, before); err != nil {
			return nil, 0, err
		}
	}
	return conversations, removed, nil
}

func compactEventAggregate(ctx context.Context, tx *sql.Tx, candidate eventCompactionCandidate, now time.Time) (int64, error) {
	statement := `SELECT e.sequence,e.type,length(CAST(e.data_json AS BLOB))+length(CAST(e.actor_json AS BLOB))
FROM collaboration_events e WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.type='run.observed'
AND e.sequence < (SELECT max(latest.sequence) FROM collaboration_events latest
WHERE latest.organization_id=e.organization_id AND latest.project_id=e.project_id AND latest.work_item_id=e.work_item_id
AND latest.type='run.observed' AND json_extract(latest.data_json,'$.run.attempt_id') IS json_extract(e.data_json,'$.run.attempt_id'))
AND json_extract(e.data_json,'$.run.finalization') IS NULL
AND json_extract(e.data_json,'$.run.handoff') IS NULL
AND json_extract(e.data_json,'$.run.runtime.validation') IS NULL
AND json_extract(e.data_json,'$.run.runtime.completion') IS NULL
AND json_extract(e.data_json,'$.run.runtime.landing') IS NULL
ORDER BY e.sequence LIMIT ?`
	args := []any{candidate.organization, candidate.project, candidate.id, eventCompactionBatch}
	if candidate.kind == "conversation" {
		statement = `SELECT e.seq,e.type,length(CAST(e.body_json AS BLOB))
FROM conversation_events e WHERE e.conversation_id=? AND (
e.type IN ('message.delta','heartbeat')
OR (e.type='message.updated' AND json_extract(e.body_json,'$.id') IS NOT NULL AND e.seq < (
SELECT max(latest.seq) FROM conversation_events latest WHERE latest.conversation_id=e.conversation_id AND latest.type='message.updated'
AND json_extract(latest.body_json,'$.id') IS json_extract(e.body_json,'$.id')))
OR (e.type='execution.updated' AND e.seq < (
SELECT max(latest.seq) FROM conversation_events latest WHERE latest.conversation_id=e.conversation_id AND latest.type='execution.updated'
AND json_extract(latest.body_json,'$.attempt_id') IS json_extract(e.body_json,'$.attempt_id')
AND json_extract(latest.body_json,'$.status') IS json_extract(e.body_json,'$.status')
AND json_extract(latest.body_json,'$.thread_id') IS json_extract(e.body_json,'$.thread_id')
AND json_extract(latest.body_json,'$.turn_id') IS json_extract(e.body_json,'$.turn_id')))
OR (e.type='conversation.updated'
AND json_extract(e.body_json,'$.title','$.visibility','$.status','$.preferences','$.subject_work_item_id','$.work_item_id','$.linked_at') IS (
SELECT json_extract(body_json,'$.title','$.visibility','$.status','$.preferences','$.subject_work_item_id','$.work_item_id','$.linked_at') FROM conversation_events
WHERE conversation_id=e.conversation_id AND type='conversation.updated' AND seq>e.seq ORDER BY seq LIMIT 1)
AND json_extract(e.body_json,'$.title','$.visibility','$.status','$.preferences','$.subject_work_item_id','$.work_item_id','$.linked_at') IS (
SELECT json_extract(body_json,'$.title','$.visibility','$.status','$.preferences','$.subject_work_item_id','$.work_item_id','$.linked_at') FROM conversation_events
WHERE conversation_id=e.conversation_id AND type='conversation.updated' AND seq<e.seq ORDER BY seq DESC LIMIT 1)))
ORDER BY e.seq LIMIT ?`
		args = []any{candidate.id, eventCompactionBatch}
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type obsoleteEvent struct {
		sequence, bytes int64
		kind            string
	}
	var events []obsoleteEvent
	for rows.Next() {
		var event obsoleteEvent
		if err := rows.Scan(&event.sequence, &event.kind, &event.bytes); err != nil {
			return 0, err
		}
		events = append(events, event)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	summary, err := readEventCompaction(ctx, tx, candidate.organization, candidate.project, candidate.kind, candidate.id)
	if err != nil {
		return 0, err
	}
	if summary.RemovedByType == nil {
		summary.RemovedByType = map[string]int64{}
	}
	for _, event := range events {
		if candidate.kind == "work_item" {
			_, err = tx.ExecContext(ctx, `DELETE FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence=?`, candidate.organization, candidate.project, candidate.id, event.sequence)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM conversation_events WHERE conversation_id=? AND seq=?`, candidate.id, event.sequence)
		}
		if err != nil {
			return 0, err
		}
		summary.RemovedEvents++
		summary.RemovedBytes += event.bytes
		summary.RemovedByType[event.kind]++
		summary.ThroughSequence = max(summary.ThroughSequence, event.sequence)
	}
	scanned := int64(0)
	if len(events) < eventCompactionBatch {
		scanned = candidate.head
	}
	if len(events) > 0 {
		summary.CompactedAt = now.UTC()
	}
	countStatement := `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=?`
	countArgs := []any{candidate.organization, candidate.project, candidate.id}
	if candidate.kind == "conversation" {
		countStatement = `SELECT count(*) FROM conversation_events WHERE conversation_id=?`
		countArgs = []any{candidate.id}
	}
	if err := tx.QueryRowContext(ctx, countStatement, countArgs...).Scan(&summary.RetainedEvents); err != nil {
		return 0, err
	}
	raw, err := marshalNative(summary)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO event_compactions(organization_id,project_id,aggregate_kind,aggregate_id,scanned_sequence,summary_json) VALUES(?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,aggregate_kind,aggregate_id) DO UPDATE SET scanned_sequence=excluded.scanned_sequence,summary_json=excluded.summary_json`, candidate.organization, candidate.project, candidate.kind, candidate.id, scanned, raw)
	if err != nil {
		return 0, fmt.Errorf("record event compaction: %w", err)
	}
	return int64(len(events)), nil
}

type tenantEventStorageSize struct {
	DatabaseBytes      int64 `json:"database_bytes"`
	FreeBytes          int64 `json:"free_bytes"`
	CollaborationBytes int64 `json:"collaboration_events_bytes"`
	ConversationBytes  int64 `json:"conversation_events_bytes"`
	CollaborationRows  int64 `json:"collaboration_events_rows"`
	ConversationRows   int64 `json:"conversation_events_rows"`
}

func tenantEventStorage(ctx context.Context, q nativeQueryer) (tenantEventStorageSize, error) {
	var size tenantEventStorageSize
	err := q.QueryRowContext(ctx, `SELECT
(SELECT page_count FROM pragma_page_count)*(SELECT page_size FROM pragma_page_size),
(SELECT freelist_count FROM pragma_freelist_count)*(SELECT page_size FROM pragma_page_size),
(SELECT coalesce(sum(pgsize),0) FROM dbstat WHERE name='collaboration_events'),
(SELECT coalesce(sum(pgsize),0) FROM dbstat WHERE name='conversation_events'),
(SELECT count(*) FROM collaboration_events),(SELECT count(*) FROM conversation_events)`).Scan(&size.DatabaseBytes, &size.FreeBytes, &size.CollaborationBytes, &size.ConversationBytes, &size.CollaborationRows, &size.ConversationRows)
	return size, err
}
