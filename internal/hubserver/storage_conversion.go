package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const backgroundStorageBudget = 2 * time.Second

const storageConversionVersion = 1

const storageConversionRows = 100

const storageConversionBytes = 1 << 20

const storageConversionOwnerPredicate = "family IN ('projection-pages','projection-activity','projection-attempts','projection-leases','projection-ranks','projection-health','projection-runners','runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files','checkpoints','landings','tool_revisions','conversation_records','conversation_questions','conversation_commands','conversation_attachments')"

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
	limit = min(limit, storageConversionLimit(ctx))
	cursor, _ := s.projectionCursors.Load(family)
	if ctx.Value(storageBatchKey{}) != nil {
		position, err := s.readStorageProjectionCursor(ctx, "keyset-"+family)
		if err != nil {
			return nil, err
		}
		if position != nil {
			cursor = position
		} else {
			cursor = nil
		}
	}
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
		if ctx.Value(storageBatchKey{}) != nil {
			if _, err := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET positioned=0 WHERE family=?`, "keyset-"+family); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return s.storageProjectionPage(ctx, family, selection, key, limit, args...)
	}
	if budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget); ok && len(candidates) > 0 {
		budget.advance = func(ctx context.Context) error {
			return s.writeStorageProjectionCursor(ctx, "keyset-"+family, candidates[len(candidates)-1].cursor)
		}
	}
	return candidates, nil
}

func (s *Service) convertLegacyStorage(ctx context.Context, now time.Time) error {
	batchCtx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
	defer cancel()
	err := s.convertStorageBatch(batchCtx, now)
	if ctx.Err() == nil && errors.Is(batchCtx.Err(), context.DeadlineExceeded) {
		return storageConversionBatchError(err)
	}
	return err
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
	var candidates []storageProjectionRow
	if selected, ok := ctx.Value(storageSelectedRowKey{}).(int64); ok {
		candidates = []storageProjectionRow{{id: strconv.FormatInt(selected, 10)}}
	} else {
		candidates, err = s.storageProjectionPage(ctx, cursorKey, selection, "r.rowid", storageConversionRows, scope.organization, scope.project, aggregate)
	}
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
		sourceDigest := storageConversionDigest(raw)
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, false, err
		}
		applied, err := s.applyStorageConversion(ctx, tx, scope, family, cursor, row, aggregate, stamp, sourceDigest, raw, converted, ref, metadata, now)
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
		var projection hubanalytics.AttemptProjection
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

func (s *Service) applyStorageConversion(ctx context.Context, tx *sql.Tx, scope nativeScope, family storageConversionFamily, cursor, row int64, aggregate, stamp, sourceDigest string, raw, converted []byte, ref attachment.ArchiveReference, metadata conversationQueryMetadata, now time.Time) (bool, error) {
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
	if err := tx.QueryRowContext(ctx, "SELECT aggregate_id=? AND storage_conversion_digest(conversion_payload)=? FROM ("+family.selection()+" AND r.conversion_state=1) WHERE conversion_rowid=?", aggregate, sourceDigest, scope.organization, scope.project, row).Scan(&unchanged); err != nil {
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

type storageBatchKey struct{}

type storageSelectedRowKey struct{}

type storageBatchBudget struct {
	bytes     int64
	rows      int
	oversized int
	turn      int64
	reserve   func() error
	advance   func(context.Context) error
	itemBytes int
	visited   map[string]bool
}

var errStorageBatchFull = errors.New("storage conversion batch is full")

func storageConversionLimit(ctx context.Context) int {
	if ctx.Value(storageBatchKey{}) != nil {
		return 1
	}
	return storageConversionRows
}

func chargeStorageConversion(ctx context.Context, size int) error {
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	if !ok {
		return nil
	}
	size = max(1, size)
	if budget.bytes > 0 && budget.bytes+int64(size) > storageConversionBytes {
		return errStorageBatchFull
	}
	if budget.reserve != nil {
		if err := budget.reserve(); err != nil {
			return err
		}
		budget.reserve = nil
	}
	budget.itemBytes = size
	budget.rows++
	budget.bytes += int64(size)
	if size > storageConversionBytes {
		budget.oversized++
	}
	return nil
}

type storageFamilyCursor struct {
	family     string
	turn       int64
	positioned bool
	keys       [4]string
	sequence   int64
}

func (f storageConversionFamily) keyset() ([]string, bool) {
	switch f.name {
	case "analytics":
		return []string{"r.organization_id", "r.project_id", "r.rowid"}, true
	case "attempts", "versions", "evidence", "checkpoints", "landings":
		return []string{"r.rowid"}, true
	case "sources":
		return []string{"r.version_id"}, false
	case "diff_files":
		return []string{"r.diff_id", "r.position"}, true
	case "conversation_events", "messages":
		return []string{"r.conversation_id", "r.rowid"}, true
	case "tool_revisions":
		return []string{"r.conversation_id", "r.message_id", "r.revision"}, true
	case "conversation_commands":
		return []string{"r.conversation_id", "r.key"}, false
	default:
		return []string{"r.id"}, false
	}
}

func (f storageConversionFamily) payloadPredicate() string {
	switch f.name {
	case "attempts", "versions", "evidence":
		return "r.payload_object_key=''"
	case "analytics":
		return "r.payload_object_key='' AND r.metric='attempt' AND r.data_json<>'{}'"
	case "diff_files":
		return "1"
	}
	return f.where
}

func (s *Service) convertStorageBatch(ctx context.Context, now time.Time) error {
	ctx = s.freshRunDetailContext(ctx)
	budget := &storageBatchBudget{}
	ctx = context.WithValue(ctx, storageBatchKey{}, budget)
	projections := []struct {
		name    string
		convert func(context.Context) error
	}{
		{"projection-pages", s.convertIssuePageFacts},
		{"projection-activity", s.convertActivityProjections},
		{"projection-attempts", s.convertAttemptHotProjections},
		{"projection-leases", s.convertAttemptLeases},
		{"projection-ranks", s.convertNativeBoardRanks},
		{"projection-health", s.convertHealthProjections},
		{"projection-runners", s.convertRunnerBoardProjections},
	}
	names := make([]string, 0, len(projections)+len(storageConversionFamilies))
	for _, p := range projections {
		names = append(names, p.name)
	}
	for _, f := range storageConversionFamilies {
		names = append(names, f.name)
	}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT family FROM storage_conversion_cursors WHERE "+storageConversionOwnerPredicate)
	if err != nil {
		return err
	}
	defer rows.Close()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return errors.Join(err, rows.Close())
		}
		existing[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, name := range names {
		if !existing[name] {
			if _, err := s.database.db.ExecContext(ctx, `INSERT INTO storage_conversion_cursors(family) VALUES(?) ON CONFLICT DO NOTHING`, name); err != nil {
				return err
			}
		}
	}
	var failures error
	for turns := 0; turns < storageConversionRows*(len(storageConversionFamilies)+len(projections)) && budget.rows < storageConversionRows && len(names) > 0; turns++ {
		var cursor storageFamilyCursor
		err := s.database.reader.QueryRowContext(ctx, `SELECT family,turn,positioned,key_1,key_2,key_3,key_4,sequence FROM storage_conversion_cursors WHERE family IN (`+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+`) ORDER BY turn,family LIMIT 1`, stringArguments(names)...).Scan(&cursor.family, &cursor.turn, &cursor.positioned, &cursor.keys[0], &cursor.keys[1], &cursor.keys[2], &cursor.keys[3], &cursor.sequence)
		if err != nil {
			return errors.Join(failures, err)
		}
		budget.reserve = func() error {
			_, err := s.database.db.ExecContext(ctx, "UPDATE storage_conversion_cursors SET turn=(SELECT max(turn)+1 FROM storage_conversion_cursors WHERE "+storageConversionOwnerPredicate+") WHERE family=?", cursor.family)
			return err
		}
		budget.advance = nil
		budget.itemBytes = 0
		budget.turn = cursor.turn
		beforeRows, beforeBytes, beforeOversized := budget.rows, budget.bytes, budget.oversized
		index := slices.IndexFunc(projections, func(p struct {
			name    string
			convert func(context.Context) error
		}) bool {
			return p.name == cursor.family
		})
		if index >= 0 {
			err = projections[index].convert(ctx)
		} else {
			family := storageConversionFamilies[slices.IndexFunc(storageConversionFamilies, func(f storageConversionFamily) bool { return f.name == cursor.family })]
			if (objectPayloadFamily(family.name) && (s.runDetailStore() != nil || s.storageArchiveContent() != nil)) || (!objectPayloadFamily(family.name) && s.storageArchiveContent() != nil) {
				err = s.convertStorageFamilyTurn(ctx, family, cursor, now)
			}
		}
		if errors.Is(err, errStorageBatchFull) {
			if budget.reserve != nil {
				return failures
			}
			_, restoreErr := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET turn=? WHERE family=?`, cursor.turn, cursor.family)
			return errors.Join(failures, restoreErr)
		}
		if budget.rows > beforeRows && budget.advance != nil {
			advanceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backgroundStorageBudget)
			advanceErr := budget.advance(advanceCtx)
			cancel()
			if advanceErr != nil {
				return errors.Join(failures, err, advanceErr)
			}
		}
		if budget.rows > beforeRows {
			if _, recordErr := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET examined_rows=examined_rows+?,examined_bytes=examined_bytes+?,oversized_rows=oversized_rows+? WHERE family=?`, budget.rows-beforeRows, budget.bytes-beforeBytes, budget.oversized-beforeOversized, cursor.family); recordErr != nil {
				return errors.Join(failures, err, recordErr)
			}
		} else {
			names = slices.DeleteFunc(names, func(name string) bool { return name == cursor.family })
		}

		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("convert legacy %s: %w", cursor.family, err))
		}
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		if budget.bytes >= storageConversionBytes {
			break
		}
	}
	return failures
}

func stringArguments(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func (s *Service) convertStorageFamilyTurn(ctx context.Context, family storageConversionFamily, cursor storageFamilyCursor, now time.Time) error {
	keys, numeric := family.keyset()
	predicate := family.payloadPredicate()
	aggregate := family.aggregate
	switch family.name {
	case "versions", "evidence":
		aggregate = "c.id"
	case "sources":
		aggregate = "r.version_id"
	case "diff_files":
		aggregate = "r.diff_id"
	}
	var c objectPayloadConversionPosition
	var next storageFamilyCursor
	var err error
	if family.name == "analytics" {
		c, next, err = s.selectStorageAnalyticsRow(ctx, family, cursor)
		if err != nil {
			return err
		}
		if c.row == 0 {
			return nil
		}
	} else {
		base := "SELECT r.rowid," + family.scope() + ".organization_id," + family.scope() + ".project_id," + aggregate + "," + strings.Join(keys, ",") + " FROM " + family.keysetFrom() + " WHERE " + predicate
		scan := []any{&c.row, &c.scope.organization, &c.scope.project, &c.aggregate}
		for i := range keys {
			if numeric && i == len(keys)-1 {
				scan = append(scan, &next.sequence)
			} else {
				scan = append(scan, &next.keys[i])
			}
		}
		wrapped := false
		for positioned := cursor.positioned; ; positioned, wrapped = false, true {
			selection := base
			var args []any
			if positioned {
				selection += " AND (" + strings.Join(keys, ",") + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
				for i := range keys {
					if numeric && i == len(keys)-1 {
						args = append(args, cursor.sequence)
					} else {
						args = append(args, cursor.keys[i])
					}
				}
			}
			err = s.database.reader.QueryRowContext(ctx, selection+" ORDER BY "+strings.Join(keys, ",")+" LIMIT 1", args...).Scan(scan...)
			if !positioned || !errors.Is(err, sql.ErrNoRows) {
				break
			}
		}
		if err == nil && wrapped && storageAggregateVisited(ctx, family, c) {
			err = sql.ErrNoRows
		}
		if errors.Is(err, sql.ErrNoRows) {
			_, err = s.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET positioned=0 WHERE family=? AND positioned<>0`, family.name)
			return err
		}
		if err != nil {
			return err
		}
	}
	sizeQuery := "SELECT " + family.payloadSize() + " FROM " + family.from + " WHERE r.rowid=?"
	var size int
	if err := s.database.reader.QueryRowContext(ctx, sizeQuery, c.row).Scan(&size); err != nil {
		return err
	}
	if err := chargeStorageConversion(ctx, size); err != nil {
		return err
	}
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	if !ok {
		return errors.New("storage conversion budget is unavailable")
	}
	if budget.visited == nil {
		budget.visited = make(map[string]bool)
	}
	budget.visited[storageAggregateKey(family, c)] = true
	budget.advance = func(ctx context.Context) error {
		if family.name == "analytics" {
			if err := s.writeStorageProjectionCursor(ctx, storageAnalyticsCursor(c.scope), []any{next.sequence}); err != nil {
				return err
			}
		}
		_, err := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET positioned=1,key_1=?,key_2=?,key_3=?,key_4=?,sequence=? WHERE family=?`, next.keys[0], next.keys[1], next.keys[2], next.keys[3], next.sequence, family.name)
		return err
	}
	ctx = context.WithValue(ctx, storageSelectedRowKey{}, c.row)
	if objectPayloadFamily(family.name) {
		return s.convertObjectPayloadRow(ctx, family, c, now)
	}
	if err := s.discoverStorageRow(ctx, family, c.row, now); err != nil {
		return err
	}
	if _, err := s.database.db.ExecContext(ctx, `UPDATE storage_conversion_progress SET format_version=?,complete=0 WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, storageConversionVersion, c.scope.organization, c.scope.project, family.name, c.aggregate); err != nil {
		return err
	}
	return s.convertStorageAggregate(ctx, c.scope, family, c.aggregate, now)
}

func storageAggregateKey(family storageConversionFamily, c objectPayloadConversionPosition) string {
	return family.name + "/" + string(c.scope.organization) + "/" + string(c.scope.project) + "/" + c.aggregate
}

func storageAggregateVisited(ctx context.Context, family storageConversionFamily, c objectPayloadConversionPosition) bool {
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	return ok && budget.visited[storageAggregateKey(family, c)]
}

func storageAnalyticsCursor(scope nativeScope) string {
	return "analytics-scope/" + string(scope.organization) + "/" + string(scope.project)
}

func (s *Service) selectStorageAnalyticsRow(ctx context.Context, family storageConversionFamily, cursor storageFamilyCursor) (objectPayloadConversionPosition, storageFamilyCursor, error) {
	var c objectPayloadConversionPosition
	var next storageFamilyCursor
	query := "SELECT r.organization_id,r.project_id FROM " + family.keysetFrom() + " WHERE " + family.payloadPredicate()
	order := " ORDER BY r.organization_id,r.project_id LIMIT 1"
	err := s.database.reader.QueryRowContext(ctx, query+" AND (r.organization_id,r.project_id)>(?,?)"+order, cursor.keys[0], cursor.keys[1]).Scan(&c.scope.organization, &c.scope.project)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.database.reader.QueryRowContext(ctx, query+order).Scan(&c.scope.organization, &c.scope.project)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return c, next, nil
	}
	if err != nil {
		return c, next, err
	}
	position, err := s.readStorageProjectionCursor(ctx, storageAnalyticsCursor(c.scope))
	if err != nil {
		return c, next, err
	}
	var after any = int64(0)
	if len(position) > 0 {
		after = position[0]
	}
	query = "SELECT r.rowid FROM " + family.keysetFrom() + " WHERE " + family.payloadPredicate() + " AND r.organization_id=? AND r.project_id=?"
	err = s.database.reader.QueryRowContext(ctx, query+" AND r.rowid>? ORDER BY r.rowid LIMIT 1", c.scope.organization, c.scope.project, after).Scan(&c.row)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.database.reader.QueryRowContext(ctx, query+" ORDER BY r.rowid LIMIT 1", c.scope.organization, c.scope.project).Scan(&c.row)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return c, next, nil
	}
	next.keys[0], next.keys[1], next.sequence = string(c.scope.organization), string(c.scope.project), c.row
	return c, next, err
}

func (f storageConversionFamily) payloadSize() string {
	columns := []string{f.column}
	switch f.name {
	case "messages":
		columns = []string{"text", "data_json", "actor_json"}
	case "tool_revisions":
		columns = []string{"text", "tool", "item_id", "status"}
	case "diff_files":
		columns = []string{"path", "old_path", "patch"}
	case "checkpoints":
		columns = []string{"checkpoint_json", "artifact_ids_json"}
	case "conversation_records":
		columns = []string{"execution_json", "preferences_json"}
	case "conversation_questions":
		columns = []string{"prompts_json", "answers_json"}
	case "conversation_attachments":
		return "coalesce((SELECT length(b.content) FROM conversation_attachment_blobs b WHERE b.artifact_ref=r.artifact_ref),0)"
	}
	for i, column := range columns {
		columns[i] = "coalesce(length(CAST(r." + column + " AS BLOB)),0)"
	}
	return strings.Join(columns, "+")
}

func (s *Service) readStorageProjectionCursor(ctx context.Context, family string) ([]any, error) {
	var keys [5]string
	var numeric int
	var positioned bool
	err := s.database.reader.QueryRowContext(ctx, `SELECT positioned,key_1,key_2,key_3,key_4,key_5,numeric_keys FROM storage_conversion_cursors WHERE family=?`, family).Scan(&positioned, &keys[0], &keys[1], &keys[2], &keys[3], &keys[4], &numeric)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !positioned {
		return nil, nil
	}
	count := numeric >> 8
	if count < 1 || count > len(keys) {
		return nil, errors.New("invalid storage projection cursor width")
	}
	result := make([]any, count)
	for i := range result {
		if numeric&(1<<i) != 0 {
			value, err := strconv.ParseInt(keys[i], 10, 64)
			if err != nil {
				return nil, err
			}
			result[i] = value
		} else {
			result[i] = keys[i]
		}
	}
	return result, nil
}

func (s *Service) writeStorageProjectionCursor(ctx context.Context, family string, cursor []any) error {
	var keys [5]string
	if len(cursor) == 0 || len(cursor) > len(keys) {
		return errors.New("invalid storage projection cursor width")
	}
	numeric := len(cursor) << 8
	for i, value := range cursor {
		switch value := value.(type) {
		case string:
			keys[i] = value
		case int64:
			keys[i] = strconv.FormatInt(value, 10)
			numeric |= 1 << i
		default:
			return errors.New("invalid storage projection key type")
		}
	}
	_, err := s.database.db.ExecContext(ctx, `INSERT INTO storage_conversion_cursors(family,positioned,key_1,key_2,key_3,key_4,key_5,numeric_keys) VALUES(?,1,?,?,?,?,?,?) ON CONFLICT(family) DO UPDATE SET positioned=1,key_1=excluded.key_1,key_2=excluded.key_2,key_3=excluded.key_3,key_4=excluded.key_4,key_5=excluded.key_5,numeric_keys=excluded.numeric_keys`, family, keys[0], keys[1], keys[2], keys[3], keys[4], numeric)
	return err
}

func storageProjectionStart(ctx context.Context, fallback uint64, count int) int {
	if budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget); ok {
		return int(budget.turn/int64(len(storageConversionFamilies)+7)) % count
	}
	return int(fallback % uint64(count))
}

func storageConversionBatchError(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures error
		for _, item := range joined.Unwrap() {
			failures = errors.Join(failures, storageConversionBatchError(item))
		}
		return failures
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func expandStorageConversion(ctx context.Context, size int) error {
	budget, ok := ctx.Value(storageBatchKey{}).(*storageBatchBudget)
	if !ok || size <= budget.itemBytes {
		return nil
	}
	expanded := budget.bytes + int64(size-budget.itemBytes)
	if budget.rows > 1 && expanded > storageConversionBytes {
		return errStorageBatchFull
	}
	if budget.bytes <= storageConversionBytes && expanded > storageConversionBytes {
		budget.oversized++
	}
	budget.bytes = expanded
	budget.itemBytes = size
	return nil
}

func (f storageConversionFamily) keysetFrom() string {
	from := f.from
	if f.name != "diff_files" {
		index := map[string]string{
			"attempts":            "native_attempts_payload_pending",
			"versions":            "change_versions_payload_pending",
			"evidence":            "change_evidence_payload_pending",
			"checkpoints":         "native_attempts_checkpoint_pending",
			"landings":            "change_landing_receipts_payload_pending",
			"analytics":           "analytics_attempt_payload_pending_scope",
			"conversation_events": "conversation_events_payload_pending",
			"messages":            "conversation_messages_payload_pending",
			"tool_revisions":      "conversation_tool_revisions_payload_pending",
		}[f.name]
		if index == "" {
			index = "storage_keyset_" + f.name
		}
		from = strings.Replace(from, " r", " r INDEXED BY "+index, 1)
	}
	return strings.ReplaceAll(from, " JOIN ", " CROSS JOIN ")
}
