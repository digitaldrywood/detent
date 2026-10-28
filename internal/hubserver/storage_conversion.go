package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const backgroundStorageBudget = 2 * time.Second

const storageConversionVersion = 1
const storageConversionRows = 100
const storageConversionBytes = 1 << 20

type storageConversionFamily struct {
	name, column, from, where, aggregate, update string
}

var storageConversionFamilies = []storageConversionFamily{
	{name: "runs", update: "UPDATE collaboration_events SET data_json=? WHERE rowid=?", column: "data_json", from: "collaboration_events r", where: "r.payload_version=0 AND r.type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')", aggregate: "r.work_item_id"},
	{name: "attempts", update: "UPDATE native_attempts SET data_json=?,analytics_revision=analytics_revision+1 WHERE rowid=?", column: "data_json", from: "native_attempts r", where: "1", aggregate: "r.work_item_id"},
	{name: "analytics", update: "UPDATE analytics_hourly_rollups SET data_json=? WHERE rowid=?", column: "data_json", from: "analytics_hourly_rollups r", where: "r.metric='attempt'", aggregate: "''"},
	{name: "conversation_events", update: "UPDATE conversation_events SET body_json=? WHERE rowid=?", column: "body_json", from: "conversation_events r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id=''", aggregate: "r.conversation_id"},
	{name: "messages", column: "data_json", from: "conversation_messages r JOIN conversations c ON c.id=r.conversation_id", where: "r.actor_principal_id IS NULL", aggregate: "r.conversation_id"},
	{name: "versions", update: "UPDATE change_versions SET record_json=? WHERE rowid=?", column: "record_json", from: "change_versions r JOIN change_requests c ON c.id=r.change_id", where: "1", aggregate: "c.work_item_id"},
	{name: "evidence", update: "UPDATE change_evidence SET record_json=? WHERE rowid=?", column: "record_json", from: "change_evidence r JOIN change_requests c ON c.id=r.change_id", where: "1", aggregate: "c.work_item_id"},
	{name: "sources", column: "bundle", from: "change_sources r JOIN change_versions v ON v.id=r.version_id JOIN change_requests c ON c.id=v.change_id", where: "r.object_key=''", aggregate: "c.work_item_id"},
	{name: "diff_files", from: "attempt_diff_files r JOIN attempt_diffs c ON c.id=r.diff_id", where: "c.object_key=''", aggregate: "c.work_item_id"},
	{name: "checkpoints", column: "checkpoint_json", from: "native_attempts r", where: "r.checkpoint_object_key='' AND r.checkpoint_json IS NOT NULL AND r.checkpoint_json<>'null'", aggregate: "r.work_item_id"},
	{name: "landings", column: "record_json", from: "change_landing_receipts r JOIN native_attempts c ON c.id=r.attempt_id", where: "r.payload_object_key='' AND r.record_json<>'{}'", aggregate: "r.attempt_id"},
	{name: "tool_revisions", column: "text", from: "conversation_tool_revisions r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id=''", aggregate: "r.conversation_id"},
	{name: "conversation_records", column: "execution_json", from: "conversations r", where: "r.content_attachment_id=''", aggregate: "r.id"},
	{name: "conversation_questions", column: "prompts_json", from: "conversation_questions r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id=''", aggregate: "r.conversation_id"},
	{name: "conversation_commands", column: "receipt_json", from: "conversation_commands r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id=''", aggregate: "r.conversation_id"},
	{name: "conversation_attachments", column: "artifact_ref", from: "conversation_attachments r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id='' AND r.deleted_at IS NULL", aggregate: "r.conversation_id"},
}

func (f storageConversionFamily) scope() string {
	if f.name == "landings" || f.name == "messages" || f.name == "conversation_events" || f.name == "tool_revisions" || f.name == "conversation_questions" || f.name == "conversation_commands" || f.name == "conversation_attachments" || f.name == "versions" || f.name == "evidence" || f.name == "sources" || f.name == "diff_files" {
		return "c"
	}
	return "r"
}

func (f storageConversionFamily) selection() string {
	column := "r." + f.column
	if f.name == "diff_files" {
		column = "r.patch"
	}

	return "SELECT r.rowid AS conversion_rowid," + f.aggregate + " AS aggregate_id," + column + " AS conversion_payload" + f.filter()
}

func (f storageConversionFamily) filter() string {
	return " FROM " + f.from + " WHERE " + f.scope() + ".organization_id=? AND " + f.scope() + ".project_id=? AND (" + f.where + ")"
}

func (f storageConversionFamily) positions() string {
	return "SELECT r.rowid AS conversion_rowid," + f.aggregate + " AS aggregate_id" + f.filter() + " AND r.conversion_state=1"
}

const storageConversionDiscovery = `SELECT aggregate_id,through_rowid FROM storage_conversion_progress
WHERE organization_id=? AND project_id=? AND family=? AND (complete=0 OR format_version<>1)
ORDER BY updated_at,aggregate_id LIMIT 4`

const storageConversionSchedule = `SELECT organization_id,project_id,family,aggregate_id FROM storage_conversion_progress WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files','tool_revisions','conversation_records','conversation_questions','conversation_commands','conversation_attachments') AND family NOT IN ('attempts','analytics','versions','evidence','sources','diff_files') ORDER BY updated_at,organization_id,project_id,family,aggregate_id LIMIT 4`

type storageConversionProgress struct {
	Family    string `json:"family"`
	Aggregate string `json:"aggregate_id"`
	Version   int    `json:"format_version"`
	Through   int64  `json:"through_rowid"`
	Cursor    int64  `json:"cursor_rowid"`
	Rows      int64  `json:"converted_rows"`
	Bytes     int64  `json:"reclaimable_bytes"`
	Complete  bool   `json:"complete"`
}

type storageProjectionRow struct {
	id     string
	cursor []any
}

func (s *Service) storageProjectionPage(ctx context.Context, family, selection, key string, limit int, args ...any) ([]storageProjectionRow, error) {
	cursor, _ := s.projectionCursors.Load(family)
	query := strings.Replace(selection, "SELECT ", "SELECT "+key+",", 1)
	pageArgs := append([]any{}, args...)
	keys := strings.Split(key, ",")
	if cursor != nil {
		position, ok := cursor.([]any)
		if !ok || len(position) != len(keys) {
			return nil, errors.New("invalid storage projection cursor")
		}
		query += " AND (" + key + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
		pageArgs = append(pageArgs, position...)
	}
	pageArgs = append(pageArgs, limit)
	rows, err := s.database.reader.QueryContext(ctx, query+" ORDER BY "+key+" LIMIT ?", pageArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []storageProjectionRow
	for rows.Next() {
		row := storageProjectionRow{cursor: make([]any, len(keys))}
		columns := make([]any, 0, len(keys)+1)
		for index := range row.cursor {
			columns = append(columns, &row.cursor[index])
		}
		if err := rows.Scan(append(columns, &row.id)...); err != nil {
			return nil, err
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(candidates) == 0 && cursor != nil {
		s.projectionCursors.Delete(family)
		return s.storageProjectionPage(ctx, family, selection, key, limit, args...)
	}
	return candidates, nil
}

func (s *Service) convertLegacyStorage(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
	defer cancel()
	var failures error
	for _, convert := range []func(context.Context) error{
		s.convertIssuePageFacts,
		s.convertActivityProjections,
		s.convertAttemptHotProjections,
		s.convertNativeBoardRanks,
		s.convertHealthProjections,
		s.convertRunnerBoardProjections,
	} {
		if err := convert(ctx); err != nil {
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return failures
			}
		}
	}
	if s.storageArchiveContent() == nil && s.runDetailStore() == nil {
		return failures
	}
	ctx = s.freshRunDetailContext(ctx)
	for _, family := range storageConversionFamilies {
		if objectPayloadFamily(family.name) {
			if err := s.convertObjectPayloadFamily(ctx, family, now); err != nil {
				failures = errors.Join(failures, fmt.Errorf("convert legacy %s: %w", family.name, err))
				if ctx.Err() != nil {
					return failures
				}
			}
			continue
		}
		if s.storageArchiveContent() == nil {
			continue
		}
		if err := s.discoverStorageFamily(ctx, family, now); err != nil {
			return err
		}
	}
	if s.storageArchiveContent() == nil {
		return failures
	}
	rows, err := s.database.reader.QueryContext(ctx, storageConversionSchedule)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		scope             nativeScope
		family, aggregate string
	}
	var batch []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.scope.organization, &item.scope.project, &item.family, &item.aggregate); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, item := range batch {
		for _, family := range storageConversionFamilies {
			if family.name != item.family {
				continue
			}
			if _, err := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_progress SET cursor_rowid=CASE WHEN format_version<>? THEN 0 ELSE cursor_rowid END,format_version=?,complete=0,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, storageConversionVersion, storageConversionVersion, formatHubTime(now), item.scope.organization, item.scope.project, family.name, item.aggregate); err != nil {
				return err
			}
			if err := s.convertStorageAggregate(ctx, item.scope, family, item.aggregate, now); err != nil {
				failures = errors.Join(failures, fmt.Errorf("convert legacy %s: %w", family.name, err))
				if ctx.Err() != nil {
					return failures
				}
			}
		}
	}
	return failures
}

func (f storageConversionFamily) table() string {
	table, _, _ := strings.Cut(f.from, " ")
	return table
}

func (f storageConversionFamily) pending() string {
	return strings.ReplaceAll(f.where, "r.", "")
}

func (f storageConversionFamily) discoverySelection() string {
	if f.name == "runs" {
		return "SELECT rowid FROM collaboration_events WHERE payload_version=0 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision') AND conversion_state<>1 ORDER BY id LIMIT ?"
	}
	if f.name == "landings" || f.name == "messages" || f.name == "conversation_events" {
		return "SELECT rowid FROM " + f.table() + " WHERE " + f.pending() + " AND conversion_state<>1 ORDER BY rowid LIMIT ?"
	}
	return "SELECT rowid FROM " + f.table() + " WHERE conversion_state=0 ORDER BY rowid LIMIT ?"
}

func (f storageConversionFamily) discoveryUpsert() string {
	state := "r.conversion_state=0"
	if f.name == "landings" || f.name == "messages" || f.name == "conversation_events" {
		state = f.where
	}
	return `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT ` + f.scope() + ".organization_id," + f.scope() + ".project_id,?," + f.aggregate + ",?,r.rowid,? FROM " + f.from + " WHERE r.rowid=? AND " + state + " AND (" + f.where + `)
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
format_version=excluded.format_version,through_rowid=max(through_rowid,excluded.through_rowid),complete=0,updated_at=excluded.updated_at`
}

func (f storageConversionFamily) stateUpdate() string {
	if f.name == "landings" || f.name == "messages" || f.name == "conversation_events" {
		return "UPDATE " + f.table() + " SET conversion_state=? WHERE rowid=? AND " + f.pending() + " AND ?=0"
	}
	return "UPDATE " + f.table() + " SET conversion_state=? WHERE rowid=? AND conversion_state=?"
}

func (f storageConversionFamily) completionUpdate() string {
	if f.name == "runs" {
		return `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=? AND NOT EXISTS (SELECT 1 FROM collaboration_events WHERE conversion_state<>1 AND payload_version=0 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision') LIMIT 1) AND NOT EXISTS (` + f.positions() + " AND " + f.aggregate + "=? LIMIT 1)"
	}
	return `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=? AND NOT EXISTS (SELECT 1 FROM ` + f.table() + ` WHERE conversion_state=0 LIMIT 1) AND NOT EXISTS (` + f.positions() + " AND " + f.aggregate + "=? LIMIT 1)"
}

func (s *Service) discoverStorageFamily(ctx context.Context, family storageConversionFamily, now time.Time) error {
	limit := storageConversionRows
	if family.name == "messages" || family.name == "conversation_events" {
		limit = 1
	}
	rows, err := s.database.reader.QueryContext(ctx, family.discoverySelection(), limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var candidates []int64
	for rows.Next() {
		var row int64
		if err := rows.Scan(&row); err != nil {
			return err
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range candidates {
		if err := s.discoverStorageRow(ctx, family, row, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) discoverStorageRow(ctx context.Context, family storageConversionFamily, row int64, now time.Time) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if family.name == "runs" {
		if _, err := tx.ExecContext(ctx, "UPDATE collaboration_events SET conversion_state=0 WHERE rowid=? AND payload_version=0 AND conversion_state<>1", row); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, family.discoveryUpsert(), family.name, storageConversionVersion, formatHubTime(now), row)
	if err != nil {
		return err
	}
	queued, err := result.RowsAffected()
	if err != nil {
		return err
	}
	state := 2
	if queued > 0 {
		state = 1
	}
	if _, err := tx.ExecContext(ctx, family.stateUpdate(), state, row, 0); err != nil {
		return err
	}
	return s.database.commit(ctx, tx)
}

func (s *Service) convertStorageFamily(ctx context.Context, scope nativeScope, family storageConversionFamily, now time.Time) error {
	if objectPayloadFamily(family.name) {
		return s.convertObjectPayloadFamily(ctx, family, now)
	}
	rows, err := s.database.reader.QueryContext(ctx, storageConversionDiscovery, scope.organization, scope.project, family.name)
	if err != nil {
		return err
	}
	defer rows.Close()
	type aggregateProgress struct {
		id      string
		through int64
	}
	var aggregates []aggregateProgress
	for rows.Next() {
		var a aggregateProgress
		if err := rows.Scan(&a.id, &a.through); err != nil {
			return err
		}
		aggregates = append(aggregates, a)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, a := range aggregates {
		if _, err := s.database.db.ExecContext(ctx, `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
cursor_rowid=CASE WHEN format_version<>excluded.format_version THEN 0 ELSE cursor_rowid END,
format_version=excluded.format_version,through_rowid=max(through_rowid,excluded.through_rowid),complete=0,updated_at=excluded.updated_at`, scope.organization, scope.project, family.name, a.id, storageConversionVersion, a.through, formatHubTime(now)); err != nil {
			return err
		}
		_, safe, err := storageConversionStamp(ctx, s.database.reader, scope, family, a.id, now)
		if err != nil {
			return err
		}
		if !safe {
			continue
		}
		if err := s.convertStorageAggregate(ctx, scope, family, a.id, now); err != nil {
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return failures
			}
		}
	}
	return failures
}

func (s *Service) convertStorageAggregate(ctx context.Context, scope nativeScope, family storageConversionFamily, aggregate string, now time.Time) error {
	if family.recordPayload() {
		return s.convertConversationRecordPayload(ctx, scope, family, aggregate, now)
	}
	if family.name == "messages" || family.name == "conversation_events" || family.name == "tool_revisions" {
		return s.convertConversationPayload(ctx, scope, family, aggregate, now)
	}
	var p storageConversionProgress
	err := s.database.reader.QueryRowContext(ctx, `SELECT format_version,through_rowid,cursor_rowid,converted_rows,reclaimed_bytes,complete FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, scope.organization, scope.project, family.name, aggregate).Scan(&p.Version, &p.Through, &p.Cursor, &p.Rows, &p.Bytes, &p.Complete)
	if err != nil {
		return err
	}
	cursorKey := "archive-" + string(scope.organization) + "/" + string(scope.project) + "/" + family.name + "/" + aggregate
	selection := "SELECT r.rowid" + family.filter() + " AND r.conversion_state=1 AND " + family.aggregate + "=?"
	candidates, err := s.storageProjectionPage(ctx, cursorKey, selection, "r.rowid", storageConversionRows, scope.organization, scope.project, aggregate)
	if err != nil {
		return err
	}
	convertRow := func(cursor, row int64, written int) (int, bool, error) {
		var raw []byte
		if err := s.database.reader.QueryRowContext(ctx, "SELECT conversion_payload FROM ("+family.selection()+") WHERE conversion_rowid=?", scope.organization, scope.project, row).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nil
			}
			return 0, false, err
		}
		stamp, safe, err := storageConversionStamp(ctx, s.database.reader, scope, family, aggregate, now)
		if err != nil || !safe {
			return 0, false, err
		}
		converted, ref, err := s.prepareStorageConversion(ctx, scope, family, row, raw)
		if err != nil {
			return 0, false, err
		}
		writeBytes := len(converted) + 4096
		if written > 0 && written+writeBytes > storageConversionBytes {
			return 0, false, nil
		}
		metadata, err := s.prepareStorageConversionQueryMetadata(ctx, family, row, raw, converted)
		if err != nil {
			return 0, false, err
		}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, false, err
		}
		applied, err := s.applyStorageConversion(ctx, tx, scope, family, cursor, row, aggregate, stamp, raw, converted, ref, metadata, now)
		if err == nil && applied {
			err = s.database.commit(ctx, tx)
		}
		rollback := tx.Rollback()
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
		return writeBytes, applied, err
	}
	written := 0
	var failures error
	for _, candidate := range candidates {
		row, err := strconv.ParseInt(candidate.id, 10, 64)
		if err != nil {
			return errors.Join(failures, err)
		}
		s.projectionCursors.Store(cursorKey, candidate.cursor)
		writeBytes, applied, err := convertRow(p.Cursor, row, written)
		if err != nil {
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return failures
			}
			continue
		}
		if !applied {
			return failures
		}
		written += writeBytes
		p.Cursor = row
	}
	result, err := s.database.db.ExecContext(ctx, family.completionUpdate(), formatHubTime(now), scope.organization, scope.project, family.name, aggregate, p.Version, scope.organization, scope.project, aggregate)
	if err != nil {
		return errors.Join(failures, err)
	}
	completed, err := result.RowsAffected()
	if completed != 0 {
		s.projectionCursors.Delete(cursorKey)
	}
	return errors.Join(failures, err)
}

func storageConversionStamp(ctx context.Context, q nativeQueryer, scope nativeScope, family storageConversionFamily, aggregate string, now time.Time) (string, bool, error) {
	if aggregate == "" || family.name == "runs" {
		return "", true, nil
	}
	var stamp string
	var revision, sequence int64
	var state string
	var executionStatus, executionLease sql.NullString
	var safe bool
	var err error
	if family.name == "messages" || family.name == "conversation_events" || family.name == "tool_revisions" || family.recordPayload() {
		err = q.QueryRowContext(ctx, `SELECT c.revision,c.event_seq,c.status,c.execution_status,c.execution_lease_id,c.status='settled' AND c.execution_status IS NOT NULL AND c.execution_lease_id IS NOT NULL AND c.execution_status IN ('idle','completed','interrupted','failed') AND NOT EXISTS(SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','sent','responding')) AND NOT EXISTS(SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent')) AND NOT EXISTS(SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND (i.native_id=c.work_item_id OR i.native_id=c.subject_work_item_id OR l.lease_id=c.execution_lease_id) AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) AND NOT EXISTS(SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id WHERE a.organization_id=c.organization_id AND a.project_id=c.project_id AND (a.work_item_id=c.work_item_id OR a.work_item_id=c.subject_work_item_id OR l.lease_id=c.execution_lease_id) AND (a.status='running' OR (l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)))) FROM conversations c WHERE c.organization_id=? AND c.project_id=? AND c.id=?`, formatHubTime(now), formatHubTime(now), scope.organization, scope.project, aggregate).Scan(&revision, &sequence, &state, &executionStatus, &executionLease, &safe)
	} else {
		err = q.QueryRowContext(ctx, `SELECT i.revision,i.event_sequence,i.workflow_state_id,NOT EXISTS(SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) AND NOT EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND a.status='running') FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, formatHubTime(now), scope.organization, scope.project, aggregate).Scan(&revision, &sequence, &state, &safe)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	values := []any{revision, sequence, state}
	if family.name == "messages" || family.name == "conversation_events" || family.name == "tool_revisions" || family.recordPayload() {
		values = append(values, archiveNullValue(executionStatus), archiveNullValue(executionLease))
	}
	raw, err := json.Marshal(values)
	stamp = string(raw)
	return stamp, safe, err
}

func (s *Service) prepareStorageConversion(ctx context.Context, scope nativeScope, family storageConversionFamily, row int64, raw []byte) ([]byte, attachment.ArchiveReference, error) {
	var zero attachment.ArchiveReference
	if family.name == "runs" {
		return s.prepareCollaborationConversion(ctx, scope, row, raw)
	}
	if family.name == "analytics" {
		var projection analyticsAttemptProjection
		if err := json.Unmarshal(raw, &projection); err != nil {
			return nil, zero, err
		}
		data, err := json.Marshal(projection)
		if bytes.Equal(data, raw) {
			return nil, zero, nil
		}
		return data, zero, err
	}
	if family.name != "sources" && family.name != "diff_files" && (len(raw) < 2048 || bytes.Contains(raw, []byte(`"_storage_archive"`))) {
		return nil, zero, nil
	}
	if family.name == "attempts" && bytes.Contains(raw, []byte(`"detail"`)) {
		return nil, zero, nil
	}
	data := raw
	if family.name == "sources" {
		var versionRaw string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT v.record_json FROM change_sources r JOIN change_versions v ON v.id=r.version_id WHERE r.rowid=?", row).Scan(&versionRaw); err != nil {
			return nil, zero, err
		}
		var version struct {
			Source *struct {
				Bytes int64 `json:"bytes"`
			} `json:"source"`
		}
		if err := decodeStorageJSON(ctx, []byte(versionRaw), &version); err != nil {
			return nil, zero, err
		}
		if version.Source == nil {
			return nil, zero, artifact.ErrIntegrity
		}
		decoded, err := decodeChangeSource(raw, version.Source.Bytes)
		if err != nil {
			return nil, zero, err
		}
		data = decoded
	} else if family.name != "diff_files" {
		payload := storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: family.name, Row: row, Data: raw}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, zero, err
		}
		data = encoded
	}
	ref, err := prepareChangeContent(ctx, string(scope.organization), data)
	if err != nil {
		return nil, zero, err
	}
	verified, err := readChangeContent(ctx, string(scope.organization), ref)
	if err != nil {
		return nil, zero, err
	}
	if !bytes.Equal(data, verified) {
		return nil, zero, artifact.ErrIntegrity
	}
	if family.name == "sources" || family.name == "diff_files" {
		return []byte{}, ref, nil
	}
	compact, err := compactStorageJSON(raw, storageJSONReference{Organization: string(scope.organization), Project: string(scope.project), Family: family.name, Row: row, Reference: ref})
	if err != nil {
		return nil, zero, err
	}

	if len(compact) >= len(raw) {
		return nil, zero, nil
	}
	return compact, ref, nil
}

func (s *Service) applyStorageConversion(ctx context.Context, tx *sql.Tx, scope nativeScope, family storageConversionFamily, cursor, row int64, aggregate, stamp string, raw, converted []byte, ref attachment.ArchiveReference, metadata conversationQueryMetadata, now time.Time) (bool, error) {
	var currentCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT cursor_rowid FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=?`, scope.organization, scope.project, family.name, aggregate, storageConversionVersion).Scan(&currentCursor); err != nil {
		return false, err
	}
	if currentCursor != cursor {
		return false, nil
	}
	current, safe, err := storageConversionStamp(ctx, tx, scope, family, aggregate, now)
	if err != nil || !safe || current != stamp {
		return false, err
	}
	var unchanged bool
	if err := tx.QueryRowContext(ctx, "SELECT aggregate_id=? AND CAST(conversion_payload AS BLOB)=? FROM ("+family.selection()+" AND r.conversion_state=1) WHERE conversion_rowid=?", aggregate, raw, scope.organization, scope.project, row).Scan(&unchanged); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !unchanged {
		return false, nil
	}
	var count, reclaimed int64
	if converted != nil {
		if family.name == "runs" && ref.Validate(string(scope.organization)) != nil {
			return false, artifact.ErrIntegrity
		}
		if err := authorizeStorageConversion(ctx, tx, scope, family, aggregate, row, raw, converted, ref); err != nil {
			return false, err
		}
		switch family.name {
		case "runs":
			if metadata.project != nil {
				if err := metadata.project(ctx, tx); err != nil {
					return false, err
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE collaboration_events SET data_json='{}',payload_version=1,payload_object_key=?,payload_sha256=?,payload_bytes=?,payload_content_bytes=? WHERE rowid=?", ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row); err != nil {
				return false, err
			}
		case "diff_files":
			if _, err := tx.ExecContext(ctx, `INSERT INTO attempt_diff_file_archives(diff_id,position,organization_id,project_id,object_key,object_sha256,object_bytes,content_bytes) SELECT diff_id,position,?,?,?,?,?,? FROM attempt_diff_files WHERE rowid=?`, scope.organization, scope.project, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM attempt_diff_files WHERE rowid=?", row); err != nil {
				return false, err
			}
		case "sources":
			if _, err := tx.ExecContext(ctx, "UPDATE change_sources SET bundle=x'',object_key=?,object_sha256=?,object_bytes=?,content_bytes=? WHERE rowid=?", ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row); err != nil {
				return false, err
			}
		default:
			if _, err := tx.ExecContext(ctx, family.update, string(converted), row); err != nil {
				return false, err
			}
		}
		if len(metadata.columns) > 0 {
			table := "collaboration_events"
			where := "rowid=?"
			if err := metadata.write(ctx, tx, table, where, row); err != nil {
				return false, err
			}
		}
		count, reclaimed = 1, max(0, int64(len(raw)-len(converted)))
		if _, err := tx.ExecContext(ctx, "UPDATE attempt_diff_body_migration SET vacuum_pending=1 WHERE singleton=1"); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, family.stateUpdate(), 2, row, 1); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET conversion_policy_revision_id='',conversion_rowid=0,conversion_before_sha256='',conversion_after_sha256='',cursor_rowid=?,converted_rows=converted_rows+?,reclaimed_bytes=reclaimed_bytes+?,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=?`, row, count, reclaimed, formatHubTime(now), scope.organization, scope.project, family.name, aggregate, storageConversionVersion)
	return err == nil, err
}

type storageDiffFile struct {
	DiffID   string                  `json:"diff_id"`
	Position int                     `json:"position"`
	File     tracker.AttemptDiffFile `json:"file"`
}

func readStorageConversionProgress(ctx context.Context, q nativeQueryer, scope nativeScope) ([]storageConversionProgress, error) {
	rows, err := q.QueryContext(ctx, `SELECT family,aggregate_id,format_version,through_rowid,cursor_rowid,converted_rows,reclaimed_bytes,complete FROM storage_conversion_progress WHERE organization_id=? AND project_id=? ORDER BY family,aggregate_id`, scope.organization, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []storageConversionProgress{}
	for rows.Next() {
		var p storageConversionProgress
		if err := rows.Scan(&p.Family, &p.Aggregate, &p.Version, &p.Through, &p.Cursor, &p.Rows, &p.Bytes, &p.Complete); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
