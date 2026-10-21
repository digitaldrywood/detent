package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const eventRetention = 30 * 24 * time.Hour
const eventCompactionBatch = 500
const closedWorkCompactionAge = 3 * 24 * time.Hour
const terminalAttemptCompactionAge = time.Hour

const closedWork = `(i.archived=1 OR (EXISTS (SELECT 1 FROM workflow_states ws WHERE ws.id=i.workflow_state_id AND ws.terminal=1)
AND julianday(` + nativeTerminalEnteredAt + `)<julianday(?)))`

const terminalAttempt = `a.status<>'running' AND julianday(a.updated_at)<julianday(?)
AND EXISTS (SELECT 1 FROM leases l WHERE l.lease_id=a.lease_id AND (l.released_at IS NOT NULL OR julianday(l.expires_at)<julianday(?)))`

const compactableWork = `NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE(s.scanned_sequence,0)<i.event_sequence
AND (` + closedWork + ` OR EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND ` + terminalAttempt + `))`

const compactableConversation = `c.status='settled'
AND max(julianday(c.settled_at),julianday(c.updated_at),julianday(COALESCE(c.last_message_at,c.updated_at)))<julianday(?)
AND json_extract(c.execution_json,'$.status') IN ('idle','completed','interrupted','failed')
AND NOT EXISTS (SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','responding'))
AND NOT EXISTS (SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent'))
AND NOT EXISTS (SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND (i.native_id=c.work_item_id OR l.lease_id=json_extract(c.execution_json,'$.lease_id')) AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND COALESCE((SELECT scanned_sequence FROM event_compactions s WHERE s.organization_id=c.organization_id AND s.project_id=c.project_id AND s.aggregate_kind='conversation' AND s.aggregate_id=c.id),0)<c.event_seq`

const terminalConversationBatch = `(a.status<>'running' OR l.released_at IS NOT NULL OR julianday(l.expires_at)<=julianday(?))`
const compactableConversationContent = `EXISTS (SELECT 1 FROM conversation_events e JOIN conversation_messages m ON m.conversation_id=e.conversation_id AND m.id=json_extract(e.body_json,'$.message_id') WHERE e.conversation_id=c.id AND e.type='message.delta' AND m.delivery IN ('delivered','completed','interrupted','failed','rejected','unknown'))
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
	obsolete []obsoleteEvent
	spans    []int64
	batches  []conversationBatchReceipt
	full     bool
}

func planEventCompaction(ctx context.Context, q nativeQueryer, now time.Time) ([]eventCompactionPlan, error) {
	closedBefore, settledBefore := formatHubTime(now.Add(-closedWorkCompactionAge)), formatHubTime(now.Add(-terminalAttemptCompactionAge))
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
			if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversations c WHERE c.id=? AND "+compactableConversation+")", plan.id, formatHubTime(now.Add(-eventRetention)), formatHubTime(now)).Scan(&plan.full); err != nil {
				return nil, err
			}
			plan.batches, err = obsoleteConversationBatches(ctx, q, plan.id, now, eventCompactionBatch)
			if err != nil {
				return nil, err
			}
			plan.obsolete, err = obsoleteConversationEvents(ctx, q, plan.id, eventCompactionBatch, plan.full)
		} else {
			plan.obsolete, plan.spans, err = obsoleteWorkItemEvents(ctx, q, plan.eventCompactionCandidate, now)
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
	rewrites := slices.ContainsFunc(plans, func(plan eventCompactionPlan) bool { return len(plan.obsolete) > 0 || len(plan.spans) > 0 })
	if rewrites {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER collaboration_events_no_delete`); err != nil {
			return nil, 0, err
		}
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER collaboration_events_no_update`); err != nil {
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
	if rewrites {
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END`); err != nil {
			return nil, 0, err
		}
		if _, err := tx.ExecContext(ctx, `CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END`); err != nil {
			return nil, 0, err
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
	var deleted int64
	for _, event := range events {
		statement := `DELETE FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence=?`
		args := []any{plan.organization, plan.project, plan.id, event.sequence}
		if plan.kind == "conversation" {
			statement, args = `DELETE FROM conversation_events WHERE conversation_id=? AND seq=?`, []any{plan.id, event.sequence}
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
	if spans := plan.spans[:min(int64(len(plan.spans)), max(budget-processed, 0))]; len(spans) > 0 {
		sequences, err := marshalNative(spans)
		if err != nil {
			return 0, 0, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE collaboration_events SET data_json=json_insert(json_remove(data_json,'$.run.runtime.activity.spans'),'$.run.runtime.activity.coverage_notes',json('[]'),'$.run.runtime.activity.coverage_notes[#]','activity_spans_compacted')
WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence IN (SELECT value FROM json_each(?)) AND json_type(data_json,'$.run.runtime.activity.spans')='array'`, plan.organization, plan.project, plan.id, sequences)
		if err != nil {
			return 0, 0, fmt.Errorf("compact activity spans: %w", err)
		}
		stripped, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		summary.CompactedActivityEvents += stripped
		processed += stripped
		if stripped > 0 {
			summary.CompactedAt = now.UTC()
		}
	}
	scanned := int64(0)
	if processed < budget && int64(len(plan.obsolete)+len(plan.spans)) <= budget {
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
	_, err = tx.ExecContext(ctx, `INSERT INTO event_compactions(organization_id,project_id,aggregate_kind,aggregate_id,scanned_sequence,summary_json) VALUES(?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,aggregate_kind,aggregate_id) DO UPDATE SET scanned_sequence=excluded.scanned_sequence,summary_json=excluded.summary_json`, plan.organization, plan.project, plan.kind, plan.id, scanned, raw)
	if err != nil {
		return 0, 0, fmt.Errorf("record event compaction: %w", err)
	}
	return deleted + retired, processed + receiptWork, nil
}

func obsoleteConversationEvents(ctx context.Context, q nativeQueryer, conversation string, limit int64, full bool) ([]obsoleteEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.seq,e.type,length(CAST(e.body_json AS BLOB))
FROM conversation_events e WHERE e.conversation_id=? AND ((e.type='message.delta' AND EXISTS(SELECT 1 FROM conversation_messages m WHERE m.conversation_id=e.conversation_id AND m.id=json_extract(e.body_json,'$.message_id') AND m.delivery IN ('delivered','completed','interrupted','failed','rejected','unknown'))) OR (? AND (
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
)) ORDER BY e.seq LIMIT ?`, conversation, full, limit)
	if err != nil {
		return nil, fmt.Errorf("select obsolete conversation events: %w", err)
	}
	defer rows.Close()
	var events []obsoleteEvent
	for rows.Next() {
		var event obsoleteEvent
		if err := rows.Scan(&event.sequence, &event.kind, &event.bytes); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, errors.Join(rows.Err(), rows.Close())
}

const (
	runFactAttempt = iota
	runFactTerminalFailure
	runFactFinalization
	runFactDisposition
	runFactCompletionBody
	runFactOutcome
	runFactValidation
	runFactCompletion
	runFactLanding
	runFactEvidence
	runFactUsage
	runFactHandoff
	runFactArtifacts
	runFactProcessUsage
	runFactCount
)

const runFactPaths = `'$.run.attempt_id','$.run.terminal_failure','$.run.finalization','$.run.disposition','$.run.completion_body','$.run.outcome',
'$.run.runtime.validation','$.run.runtime.completion','$.run.runtime.landing','$.run.evidence','$.run.usage','$.run.handoff','$.run.artifact_ids','$.run.process_usage'`

type runHistoryEvent struct {
	obsoleteEvent
	attempt string
	facts   []json.RawMessage
}

func obsoleteWorkItemEvents(ctx context.Context, q nativeQueryer, candidate eventCompactionCandidate, now time.Time) ([]obsoleteEvent, []int64, error) {
	terminal := map[string]bool{}
	if !candidate.closed {
		settled := formatHubTime(now.Add(-terminalAttemptCompactionAge))
		ids, err := nativePageIDs(ctx, q, `SELECT a.id FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND `+terminalAttempt, candidate.organization, candidate.project, candidate.id, settled, settled)
		if err != nil {
			return nil, nil, fmt.Errorf("select terminal attempts: %w", err)
		}
		for _, id := range ids {
			terminal[id] = true
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT sequence,type,length(CAST(data_json AS BLOB))+length(CAST(actor_json AS BLOB)),json_extract(data_json,`+runFactPaths+`),
coalesce(json_type(data_json,'$.run.runtime.activity.spans')='array',0)
FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type IN ('run.started','run.checkpointed','run.observed','run.finished') ORDER BY sequence`, candidate.organization, candidate.project, candidate.id)
	if err != nil {
		return nil, nil, fmt.Errorf("select run history: %w", err)
	}
	defer rows.Close()
	var events []runHistoryEvent
	var spans []int64
	for rows.Next() {
		var event runHistoryEvent
		var facts string
		var activity bool
		if err := rows.Scan(&event.sequence, &event.kind, &event.bytes, &facts, &activity); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal([]byte(facts), &event.facts); err != nil {
			return nil, nil, fmt.Errorf("decode run history facts: %w", err)
		}
		if len(event.facts) != runFactCount {
			return nil, nil, fmt.Errorf("decode run history facts: %d of %d", len(event.facts), runFactCount)
		}
		if json.Unmarshal(event.facts[runFactAttempt], &event.attempt) != nil {
			event.attempt = ""
		}
		if !candidate.closed && !terminal[event.attempt] {
			continue
		}
		events = append(events, event)
		if activity {
			spans = append(spans, event.sequence)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, nil, err
	}
	obsolete := obsoleteRunEvents(events)
	deleted := map[int64]bool{}
	for _, event := range obsolete {
		deleted[event.sequence] = true
	}
	spans = slices.DeleteFunc(spans, func(sequence int64) bool { return deleted[sequence] })
	return obsolete, spans, nil
}

func obsoleteRunEvents(events []runHistoryEvent) []obsoleteEvent {
	type snapshot struct{ attempt, kind string }
	last := map[snapshot]int64{}
	for _, event := range events {
		if runSnapshotEvent(event.kind) {
			key := snapshot{event.attempt, event.kind}
			last[key] = max(last[key], event.sequence)
		}
	}
	kept := map[string][]runHistoryEvent{}
	var intermediate []runHistoryEvent
	for _, event := range events {
		if runSnapshotEvent(event.kind) && last[snapshot{event.attempt, event.kind}] != event.sequence {
			intermediate = append(intermediate, event)
			continue
		}
		kept[event.attempt] = append(kept[event.attempt], event)
	}
	var obsolete []obsoleteEvent
	for _, event := range intermediate {
		var later []runHistoryEvent
		for _, retained := range kept[event.attempt] {
			if retained.sequence > event.sequence {
				later = append(later, retained)
			}
		}
		if runFactsRetained(event, later) {
			obsolete = append(obsolete, event.obsoleteEvent)
		}
	}
	return obsolete
}

func runSnapshotEvent(kind string) bool {
	return kind == "run.checkpointed" || kind == "run.observed"
}

func runFactsRetained(event runHistoryEvent, later []runHistoryEvent) bool {
	for fact := runFactTerminalFailure; fact <= runFactLanding; fact++ {
		if runFactPresent(event.facts[fact]) && !slices.ContainsFunc(later, func(k runHistoryEvent) bool { return bytes.Equal(k.facts[fact], event.facts[fact]) }) {
			return false
		}
	}
	for fact, superseding := range map[int]int{runFactHandoff: runFactHandoff, runFactArtifacts: runFactHandoff, runFactProcessUsage: runFactProcessUsage} {
		if runFactPresent(event.facts[fact]) && !slices.ContainsFunc(later, func(k runHistoryEvent) bool { return runFactPresent(k.facts[superseding]) }) {
			return false
		}
	}
	effect, ok := runHandoffEffect(event.facts[runFactHandoff])
	if !ok {
		return false
	}
	if effect != "" && !slices.ContainsFunc(later, func(k runHistoryEvent) bool {
		retained, ok := runHandoffEffect(k.facts[runFactHandoff])
		return ok && retained == effect
	}) {
		return false
	}
	evidence, ok := runFactItems(event.facts[runFactEvidence])
	if !ok {
		return false
	}
	for _, item := range evidence {
		if !slices.ContainsFunc(later, func(k runHistoryEvent) bool {
			retained, ok := runFactItems(k.facts[runFactEvidence])
			return ok && slices.ContainsFunc(retained, func(r json.RawMessage) bool { return bytes.Equal(r, item) })
		}) {
			return false
		}
	}
	usage, ok := runUsageKeys(event.facts[runFactUsage])
	if !ok {
		return false
	}
	for key := range usage {
		if !slices.ContainsFunc(later, func(k runHistoryEvent) bool {
			retained, ok := runUsageKeys(k.facts[runFactUsage])
			return ok && retained[key]
		}) {
			return false
		}
	}
	return true
}

func runFactPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func runHandoffEffect(raw json.RawMessage) (string, bool) {
	if !runFactPresent(raw) {
		return "", true
	}
	var handoff struct {
		ExternalEffect string          `json:"external_effect"`
		EffectState    string          `json:"effect_state"`
		EffectID       string          `json:"effect_id"`
		HeadSHA        string          `json:"head_sha"`
		Change         json.RawMessage `json:"change"`
	}
	if json.Unmarshal(raw, &handoff) != nil {
		return "", false
	}
	if (handoff.ExternalEffect == "" || handoff.ExternalEffect == "none") && !runFactPresent(handoff.Change) {
		return "", true
	}
	return strings.Join([]string{handoff.ExternalEffect, handoff.EffectState, handoff.EffectID, handoff.HeadSHA, string(handoff.Change)}, "\x00"), true
}

func runFactItems(raw json.RawMessage) ([]json.RawMessage, bool) {
	if !runFactPresent(raw) {
		return nil, true
	}
	var items []json.RawMessage
	return items, json.Unmarshal(raw, &items) == nil
}

func runUsageKeys(raw json.RawMessage) (map[string]bool, bool) {
	keys := map[string]bool{}
	if !runFactPresent(raw) {
		return keys, true
	}
	var usage []struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		UsageKind string `json:"usage_kind"`
		SourceID  string `json:"source_id"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return nil, false
	}
	for _, entry := range usage {
		keys[entry.Provider+"\x00"+entry.Model+"\x00"+entry.UsageKind+"\x00"+entry.SourceID] = true
	}
	return keys, true
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
