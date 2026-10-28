package hubserver

import (
	"bytes"
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
const terminalAttemptCompactionAge = time.Hour

const closedWork = `EXISTS(SELECT 1 FROM issue_storage_archives p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id AND p.issue_revision=i.revision AND p.event_sequence=i.event_sequence AND julianday(p.published_at)<=julianday(?) AND EXISTS(SELECT 1 FROM native_archive_sections n WHERE n.organization_id=i.organization_id AND n.project_id=i.project_id AND n.work_item_id=i.native_id AND n.kind='history' AND n.first_sequence=0) AND COALESCE((SELECT max(n.last_sequence) FROM native_archive_sections n WHERE n.organization_id=i.organization_id AND n.project_id=i.project_id AND n.work_item_id=i.native_id AND n.kind='history'),0)>=COALESCE((SELECT max(e.sequence) FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type IN ('run.started','run.checkpointed','run.observed','run.finished')),0))`

const terminalAttempt = `a.status<>'running' AND julianday(a.updated_at)<julianday(?)
AND EXISTS (SELECT 1 FROM leases l WHERE l.lease_id=a.lease_id AND (l.released_at IS NOT NULL OR julianday(l.expires_at)<julianday(?)))`

const compactableWork = `NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE(s.scanned_sequence,0)<i.event_sequence
AND (` + closedWork + ` OR (EXISTS(SELECT 1 FROM workflow_states ws WHERE ws.id=i.workflow_state_id AND ws.terminal=0) AND EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND ` + terminalAttempt + `)))`

const compactableConversation = `c.work_item_id IS NULL AND c.subject_work_item_id IS NULL AND c.status='settled'
AND max(julianday(c.settled_at),julianday(c.updated_at),julianday(COALESCE(c.last_message_at,c.updated_at)))<julianday(?)
AND (c.query_metadata_version=0 OR c.execution_status IN ('idle','completed','interrupted','failed'))
AND NOT EXISTS (SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','responding'))
AND NOT EXISTS (SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))
AND NOT EXISTS (SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND (i.native_id=c.work_item_id OR l.lease_id=c.execution_lease_id) AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE((SELECT scanned_sequence FROM event_compactions s WHERE s.organization_id=c.organization_id AND s.project_id=c.project_id AND s.aggregate_kind='conversation' AND s.aggregate_id=c.id),0)<c.event_seq`

const terminalConversationBatch = `(a.status<>'running' OR l.released_at IS NOT NULL OR julianday(l.expires_at)<=julianday(?))`
const compactableConversationContent = `EXISTS (SELECT 1 FROM conversation_events e JOIN conversation_messages m ON m.conversation_id=e.conversation_id AND m.id=e.message_id WHERE e.conversation_id=c.id AND e.type='message.delta' AND m.delivery IN ('delivered','completed','interrupted','failed','rejected','unknown'))
OR EXISTS (SELECT 1 FROM conversation_starts cs JOIN conversation_turn_batches b ON b.attempt_id=cs.attempt_id JOIN native_attempts a ON a.id=b.attempt_id JOIN leases l ON l.lease_id=a.lease_id WHERE cs.conversation_id=c.id AND ` + terminalConversationBatch + `)`

type eventCompactionCandidate struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	kind, id     string
	head         int64
	closed       bool
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

type eventCompactionPlan struct {
	eventCompactionCandidate
	obsolete     []obsoleteEvent
	spans        []activitySpanRewrite
	batches      []conversationBatchReceipt
	full         bool
	scanComplete bool
}

func planEventCompaction(ctx context.Context, q nativeQueryer, now time.Time) ([]eventCompactionPlan, error) {
	closedBefore, settledBefore := formatHubTime(now), formatHubTime(now.Add(-terminalAttemptCompactionAge))
	rows, err := q.QueryContext(ctx, `SELECT * FROM (SELECT i.organization_id,i.project_id,'work_item',i.native_id,i.event_sequence,`+closedWork+` FROM issues i
LEFT JOIN event_compactions s ON s.organization_id=i.organization_id AND s.project_id=i.project_id AND s.aggregate_kind='work_item' AND s.aggregate_id=i.native_id
WHERE `+compactableWork+` ORDER BY i.event_sequence-COALESCE(s.scanned_sequence,0) DESC LIMIT 4)
UNION ALL SELECT * FROM (SELECT c.organization_id,c.project_id,'conversation',c.id,c.event_seq,0 FROM conversations c WHERE (`+compactableConversation+`) OR (`+compactableConversationContent+`) LIMIT 4)`,
		closedBefore, formatHubTime(now), closedBefore, settledBefore, settledBefore, formatHubTime(now.Add(-eventRetention)), formatHubTime(now), formatHubTime(now))
	if err != nil {
		return nil, fmt.Errorf("select compaction candidates: %w", err)
	}
	defer rows.Close()
	var plans []eventCompactionPlan
	for rows.Next() {
		var plan eventCompactionPlan
		plan.scanComplete = true
		if err := rows.Scan(&plan.organization, &plan.project, &plan.kind, &plan.id, &plan.head, &plan.closed); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for index := range plans {
		plan := &plans[index]
		if plan.kind == "conversation" {
			if err := q.QueryRowContext(ctx, "SELECT NOT EXISTS(SELECT 1 FROM conversation_events WHERE conversation_id=? AND query_metadata_version=0)", plan.id).Scan(&plan.scanComplete); err != nil {
				return nil, err
			}
			plan.full, err = conversationIsCompactable(ctx, q, plan.id, now)
			if err != nil {
				return nil, err
			}
			plan.batches, err = obsoleteConversationBatches(ctx, q, plan.id, now, eventCompactionBatch)
			if err != nil {
				return nil, err
			}
			plan.obsolete, err = obsoleteConversationEvents(ctx, q, plan.id, eventCompactionBatch, plan.full)
		} else {
			plan.obsolete, plan.spans, plan.scanComplete, err = obsoleteWorkItemEvents(ctx, q, plan.eventCompactionCandidate, now)
		}
		if err != nil {
			return nil, err
		}
	}
	return plans, nil
}

func applyEventCompaction(ctx context.Context, tx *sql.Tx, plans []eventCompactionPlan, now time.Time) ([]string, int64, error) {
	if len(plans) == 0 {
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
	var conversations []string
	var removed int64
	budget := int64(eventCompactionBatch)
	for _, plan := range plans {
		if budget <= 0 {
			break
		}
		count, processed, err := compactEventAggregate(ctx, tx, plan, now, budget)
		if err != nil {
			return nil, 0, err
		}
		budget -= processed
		removed += count
		if count > 0 && plan.kind == "conversation" {
			conversations = append(conversations, plan.id)
		}
	}
	if removed > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE attempt_diff_body_migration SET vacuum_pending=CASE WHEN event_compaction_storage_before='' THEN 1 ELSE vacuum_pending END,event_compaction_storage_before=CASE WHEN event_compaction_storage_before='' THEN ? ELSE event_compaction_storage_before END WHERE singleton=1`, before); err != nil {
			return nil, 0, err
		}
	}
	return conversations, removed, nil
}

type obsoleteEvent struct {
	sequence, bytes int64
	kind            string
	original        string
}

func compactEventAggregate(ctx context.Context, tx *sql.Tx, plan eventCompactionPlan, now time.Time, budget int64) (int64, int64, error) {
	var retired int64
	receiptWork := min(int64(len(plan.batches)), budget)
	for _, batch := range plan.batches[:receiptWork] {
		result, err := tx.ExecContext(ctx, `DELETE FROM conversation_turn_batches WHERE attempt_id=? AND batch_key=? AND EXISTS(SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id WHERE a.id=conversation_turn_batches.attempt_id AND `+terminalConversationBatch+`)`, batch.attempt, batch.key, formatHubTime(now))
		if err != nil {
			return 0, 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		retired += count
	}
	budget -= receiptWork
	events := plan.obsolete
	if int64(len(events)) > budget {
		events = events[:budget]
	}
	summary, err := readEventCompaction(ctx, tx, plan.organization, plan.project, plan.kind, plan.id)
	if err != nil {
		return 0, 0, err
	}
	if summary.RemovedByType == nil {
		summary.RemovedByType = map[string]int64{}
	}
	if plan.kind == "work_item" && (len(events) > 0 || len(plan.spans) > 0) {
		raw, err := marshalNative(summary)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_compactions(organization_id,project_id,aggregate_kind,aggregate_id,summary_json) VALUES(?,?,'work_item',?,?) ON CONFLICT DO NOTHING`, plan.organization, plan.project, plan.id, raw); err != nil {
			return 0, 0, err
		}
	}
	var deleted int64
	for _, event := range events {
		statement := `DELETE FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence=?`
		args := []any{plan.organization, plan.project, plan.id, event.sequence}
		if plan.kind == "conversation" {
			statement, args = `DELETE FROM conversation_events WHERE conversation_id=? AND seq=? AND body_json=?`, []any{plan.id, event.sequence, event.original}
		}
		if plan.kind == "work_item" {
			if _, err := tx.ExecContext(ctx, `UPDATE event_compactions SET mutation_sequence=?,mutation_delete=1 WHERE organization_id=? AND project_id=? AND aggregate_kind='work_item' AND aggregate_id=?`, event.sequence, plan.organization, plan.project, plan.id); err != nil {
				return 0, 0, err
			}
		}
		result, err := tx.ExecContext(ctx, statement, args...)
		if err != nil {
			return 0, 0, fmt.Errorf("delete obsolete event: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		if affected == 0 {
			continue
		}
		deleted++
		summary.RemovedEvents++
		summary.RemovedBytes += event.bytes
		summary.RemovedByType[event.kind]++
		summary.ThroughSequence = max(summary.ThroughSequence, event.sequence)
	}
	processed := int64(len(events))
	for _, span := range plan.spans[:min(int64(len(plan.spans)), max(budget-processed, 0))] {
		if _, err := tx.ExecContext(ctx, `UPDATE event_compactions SET mutation_sequence=?,mutation_delete=0,mutation_before_sha256=?,mutation_after_sha256=? WHERE organization_id=? AND project_id=? AND aggregate_kind='work_item' AND aggregate_id=?`, span.sequence, storageConversionDigest(span.original), storageConversionDigest(span.replacement), plan.organization, plan.project, plan.id); err != nil {
			return 0, 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE collaboration_events SET data_json=?,activity_spans_present=0,query_metadata_version=1 WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence=? AND data_json=?`, span.replacement, plan.organization, plan.project, plan.id, span.sequence, span.original)
		if err != nil {
			return 0, 0, fmt.Errorf("compact activity spans: %w", err)
		}
		stripped, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		summary.CompactedActivityEvents += stripped
		processed++
		if stripped > 0 {
			summary.CompactedAt = now.UTC()
		}
	}

	scanned := int64(0)
	if plan.scanComplete && processed < budget && int64(len(plan.obsolete)+len(plan.spans)) <= budget {
		if plan.kind == "work_item" || plan.full {
			scanned = plan.head
		}
	}
	if deleted > 0 {
		summary.CompactedAt = now.UTC()
	}
	countStatement := `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=?`
	countArgs := []any{plan.organization, plan.project, plan.id}
	if plan.kind == "conversation" {
		countStatement = `SELECT count(*) FROM conversation_events WHERE conversation_id=?`
		countArgs = []any{plan.id}
	}
	if err := tx.QueryRowContext(ctx, countStatement, countArgs...).Scan(&summary.RetainedEvents); err != nil {
		return 0, 0, err
	}
	raw, err := marshalNative(summary)
	if err != nil {
		return 0, 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO event_compactions(organization_id,project_id,aggregate_kind,aggregate_id,scanned_sequence,summary_json,through_sequence,query_metadata_version) VALUES(?,?,?,?,?,?,?,1)
ON CONFLICT(organization_id,project_id,aggregate_kind,aggregate_id) DO UPDATE SET mutation_sequence=0,mutation_delete=0,mutation_before_sha256='',mutation_after_sha256='',scanned_sequence=excluded.scanned_sequence,summary_json=excluded.summary_json,through_sequence=excluded.through_sequence,query_metadata_version=1`, plan.organization, plan.project, plan.kind, plan.id, scanned, raw, summary.ThroughSequence)
	if err != nil {
		return 0, 0, fmt.Errorf("record event compaction: %w", err)
	}
	return deleted + retired, processed + receiptWork, nil
}

func obsoleteConversationEvents(ctx context.Context, q nativeQueryer, conversation string, limit int64, full bool) ([]obsoleteEvent, error) {
	query, args, err := resolveConversationEventMetadata(ctx, q, conversation, `SELECT e.seq,e.type,length(CAST(e.body_json AS BLOB)),e.body_json
FROM conversation_events e WHERE e.conversation_id=? AND e.query_metadata_version=1 AND ((e.type='message.delta' AND EXISTS(SELECT 1 FROM conversation_messages m WHERE m.conversation_id=e.conversation_id AND m.id=e.message_id AND m.delivery IN ('delivered','completed','interrupted','failed','rejected','unknown'))) OR (? AND (
e.type IN ('message.delta','heartbeat')
OR (e.type='message.updated' AND e.object_id IS NOT NULL AND e.seq < (
SELECT max(latest.seq) FROM conversation_events latest WHERE latest.conversation_id=e.conversation_id AND latest.type='message.updated' AND latest.query_metadata_version=1
AND latest.object_id IS e.object_id))
OR (e.type='execution.updated' AND e.seq < (
SELECT max(latest.seq) FROM conversation_events latest WHERE latest.conversation_id=e.conversation_id AND latest.type='execution.updated' AND latest.query_metadata_version=1
AND latest.attempt_id IS e.attempt_id
AND latest.execution_status IS e.execution_status
AND latest.thread_id IS e.thread_id
AND latest.turn_id IS e.turn_id))
OR (e.type='conversation.updated' AND EXISTS (SELECT 1 FROM conversation_events neighbor WHERE neighbor.conversation_id=e.conversation_id AND neighbor.type='conversation.updated' AND neighbor.seq=(SELECT seq FROM conversation_events WHERE conversation_id=e.conversation_id AND type='conversation.updated' AND seq>e.seq ORDER BY seq LIMIT 1) AND neighbor.query_metadata_version=1 AND neighbor.snapshot_title IS e.snapshot_title AND neighbor.snapshot_visibility IS e.snapshot_visibility AND neighbor.snapshot_status IS e.snapshot_status AND neighbor.snapshot_preferences_key IS e.snapshot_preferences_key AND neighbor.snapshot_subject_work_item_id IS e.snapshot_subject_work_item_id AND neighbor.snapshot_work_item_id IS e.snapshot_work_item_id AND neighbor.snapshot_linked_at IS e.snapshot_linked_at) AND EXISTS (SELECT 1 FROM conversation_events neighbor WHERE neighbor.conversation_id=e.conversation_id AND neighbor.type='conversation.updated' AND neighbor.seq=(SELECT seq FROM conversation_events WHERE conversation_id=e.conversation_id AND type='conversation.updated' AND seq<e.seq ORDER BY seq DESC LIMIT 1) AND neighbor.query_metadata_version=1 AND neighbor.snapshot_title IS e.snapshot_title AND neighbor.snapshot_visibility IS e.snapshot_visibility AND neighbor.snapshot_status IS e.snapshot_status AND neighbor.snapshot_preferences_key IS e.snapshot_preferences_key AND neighbor.snapshot_subject_work_item_id IS e.snapshot_subject_work_item_id AND neighbor.snapshot_work_item_id IS e.snapshot_work_item_id AND neighbor.snapshot_linked_at IS e.snapshot_linked_at))
))) ORDER BY e.seq LIMIT ?`, conversation, full, limit)
	if err != nil {
		return nil, fmt.Errorf("select obsolete conversation events: %w", err)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select obsolete conversation events: %w", err)
	}
	defer rows.Close()
	var events []obsoleteEvent
	for rows.Next() {
		var event obsoleteEvent
		if err := rows.Scan(&event.sequence, &event.kind, &event.bytes, &event.original); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, errors.Join(rows.Err(), rows.Close())
}

func obsoleteWorkItemEvents(ctx context.Context, q nativeQueryer, candidate eventCompactionCandidate, now time.Time) ([]obsoleteEvent, []activitySpanRewrite, bool, error) {
	settled := formatHubTime(now.Add(-terminalAttemptCompactionAge))
	rows, err := q.QueryContext(ctx, `SELECT sequence,run_attempt_id,query_metadata_version,data_json FROM collaboration_events e WHERE organization_id=? AND project_id=? AND work_item_id=? AND type IN ('run.started','run.checkpointed','run.observed','run.finished') AND (query_metadata_version=0 OR (activity_spans_present=1 AND (? OR EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=e.organization_id AND a.project_id=e.project_id AND a.work_item_id=e.work_item_id AND a.id=e.run_attempt_id AND `+terminalAttempt+`)))) ORDER BY sequence LIMIT ?`, candidate.organization, candidate.project, candidate.id, candidate.closed, settled, settled, eventCompactionBatch+1)
	if err != nil {
		return nil, nil, false, fmt.Errorf("select legacy activity spans: %w", err)
	}
	defer rows.Close()
	type candidateEvent struct {
		sequence int64
		attempt  sql.NullString
		version  int
		raw      string
	}
	var events []candidateEvent
	for rows.Next() {
		var e candidateEvent
		if err := rows.Scan(&e.sequence, &e.attempt, &e.version, &e.raw); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, false, err
	}
	complete := len(events) <= eventCompactionBatch
	var spans []activitySpanRewrite
	for _, event := range events[:min(len(events), eventCompactionBatch)] {
		rewrite, attempt, present, err := prepareActivitySpanRewrite(event.sequence, event.raw)
		if err != nil {
			return nil, nil, false, err
		}
		if !present {
			continue
		}
		if event.version == 0 && !candidate.closed {
			var eligible bool
			if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND a.id=? AND `+terminalAttempt+`)`, candidate.organization, candidate.project, candidate.id, attempt, settled, settled).Scan(&eligible); err != nil {
				return nil, nil, false, err
			}
			if !eligible {
				continue
			}
		}
		spans = append(spans, rewrite)
	}
	return nil, spans, complete, nil
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

type conversationBatchReceipt struct{ attempt, key string }

func obsoleteConversationBatches(ctx context.Context, q nativeQueryer, id string, now time.Time, limit int64) ([]conversationBatchReceipt, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.attempt_id,b.batch_key FROM conversation_turn_batches b JOIN conversation_starts cs ON cs.attempt_id=b.attempt_id JOIN native_attempts a ON a.id=b.attempt_id JOIN leases l ON l.lease_id=a.lease_id WHERE cs.conversation_id=? AND `+terminalConversationBatch+` ORDER BY b.attempt_id,b.batch_key LIMIT ?`, id, formatHubTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batches []conversationBatchReceipt
	for rows.Next() {
		var batch conversationBatchReceipt
		if err := rows.Scan(&batch.attempt, &batch.key); err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, errors.Join(rows.Err(), rows.Close())
}

type activitySpanRewrite struct {
	sequence              int64
	original, replacement string
}

func prepareActivitySpanRewrite(sequence int64, raw string) (activitySpanRewrite, string, bool, error) {
	rewrite := activitySpanRewrite{sequence: sequence, original: raw}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return rewrite, "", false, err
	}
	if body == nil {
		return rewrite, "", false, nil
	}
	var run map[string]json.RawMessage
	if value := body["run"]; len(value) == 0 || string(value) == "null" {
		return rewrite, "", false, nil
	}
	if err := json.Unmarshal(body["run"], &run); err != nil {
		return rewrite, "", false, err
	}
	if run == nil {
		return rewrite, "", false, nil
	}
	var attempt string
	if value := run["attempt_id"]; len(value) > 0 {
		if err := json.Unmarshal(value, &attempt); err != nil {
			return rewrite, "", false, err
		}
	}
	runtime := map[string]json.RawMessage{}
	if value := run["runtime"]; len(value) == 0 || string(value) == "null" {
		return rewrite, attempt, false, nil
	} else if err := json.Unmarshal(value, &runtime); err != nil {
		return rewrite, attempt, false, err
	}
	activity := map[string]json.RawMessage{}
	if value := runtime["activity"]; len(value) == 0 || string(value) == "null" {
		return rewrite, attempt, false, nil
	} else if err := json.Unmarshal(value, &activity); err != nil {
		return rewrite, attempt, false, err
	}
	spans := bytes.TrimSpace(activity["spans"])
	if len(spans) == 0 || spans[0] != '[' {
		return rewrite, attempt, false, nil
	}
	delete(activity, "spans")
	var notes []string
	if value := activity["coverage_notes"]; len(value) > 0 {
		if err := json.Unmarshal(value, &notes); err != nil {
			return rewrite, attempt, false, err
		}
	}
	notes = append(notes, "activity_spans_compacted")
	var err error
	activity["coverage_notes"], err = json.Marshal(notes)
	if err != nil {
		return rewrite, attempt, false, err
	}
	runtime["activity"], err = json.Marshal(activity)
	if err != nil {
		return rewrite, attempt, false, err
	}
	run["runtime"], err = json.Marshal(runtime)
	if err != nil {
		return rewrite, attempt, false, err
	}
	body["run"], err = json.Marshal(run)
	if err != nil {
		return rewrite, attempt, false, err
	}
	encoded, err := json.Marshal(body)
	rewrite.replacement = string(encoded)
	return rewrite, attempt, true, err
}
