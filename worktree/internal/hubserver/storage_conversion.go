package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const backgroundStorageBudget = 2 * time.Second

const storageConversionVersion = 1
const storageConversionRows = 100
const storageConversionBytes = 1 << 20

type storageConversionFamily struct {
	name, column, from, where, aggregate, update string
	triggers                                     []string
}

var storageConversionFamilies = []storageConversionFamily{
	{name: "runs", update: "UPDATE collaboration_events SET data_json=? WHERE rowid=?", column: "data_json", from: "collaboration_events r", where: "r.type IN ('run.started','run.checkpointed','run.observed','run.finished')", aggregate: "r.work_item_id", triggers: []string{"collaboration_events_no_update"}},
	{name: "attempts", update: "UPDATE native_attempts SET data_json=? WHERE rowid=?", column: "data_json", from: "native_attempts r", where: "1", aggregate: "r.work_item_id"},
	{name: "analytics", update: "UPDATE analytics_hourly_rollups SET data_json=? WHERE rowid=?", column: "data_json", from: "analytics_hourly_rollups r", where: "r.metric='attempt'", aggregate: "''"},
	{name: "conversation_events", update: "UPDATE conversation_events SET body_json=? WHERE rowid=?", column: "body_json", from: "conversation_events r JOIN conversations c ON c.id=r.conversation_id", where: "r.type IN ('message.accepted','message.updated')", aggregate: "r.conversation_id"},
	{name: "messages", column: "", from: "conversation_messages r JOIN conversations c ON c.id=r.conversation_id", where: "r.content_attachment_id=''", aggregate: "r.conversation_id"},
	{name: "versions", update: "UPDATE change_versions SET record_json=? WHERE rowid=?", column: "record_json", from: "change_versions r JOIN change_requests c ON c.id=r.change_id", where: "1", aggregate: "c.work_item_id", triggers: []string{"change_versions_immutable"}},
	{name: "evidence", update: "UPDATE change_evidence SET record_json=? WHERE rowid=?", column: "record_json", from: "change_evidence r JOIN change_requests c ON c.id=r.change_id", where: "1", aggregate: "c.work_item_id", triggers: []string{"change_evidence_immutable"}},
	{name: "sources", column: "bundle", from: "change_sources r JOIN change_versions v ON v.id=r.version_id JOIN change_requests c ON c.id=v.change_id", where: "r.object_key=''", aggregate: "c.work_item_id", triggers: []string{"change_sources_no_update", "change_sources_immutable_reference"}},
	{name: "diff_files", from: "attempt_diff_files r JOIN attempt_diffs c ON c.id=r.diff_id", where: "c.object_key=''", aggregate: "c.work_item_id"},
}

func (f storageConversionFamily) scope() string {
	if f.name == "messages" || f.name == "conversation_events" || f.name == "versions" || f.name == "evidence" || f.name == "sources" || f.name == "diff_files" {
		return "c"
	}
	return "r"
}

func (f storageConversionFamily) selection() string {
	column := "r." + f.column
	if f.name == "messages" {
		column = "json_object('text',r.text,'data',json(r.data_json))"
	}
	if f.name == "diff_files" {
		column = `json_object('diff_id',r.diff_id,'position',r.position,'file',json_object('path',r.path,'old_path',r.old_path,'status',r.status,'additions',r.additions,'deletions',r.deletions,'binary',json(CASE WHEN r.binary THEN 'true' ELSE 'false' END),'patch',r.patch,'truncated',json(CASE WHEN r.truncated THEN 'true' ELSE 'false' END),'denied',json(CASE WHEN r.denied THEN 'true' ELSE 'false' END),'patch_expired',json(CASE WHEN r.patch_expired THEN 'true' ELSE 'false' END),'patch_body',CASE WHEN r.patch_object<>'' AND r.patch_expired=0 THEN json_object('key',r.patch_object,'sha256',r.patch_sha256,'bytes',r.patch_size) ELSE NULL END))`
	}
	return "SELECT r.rowid AS conversion_rowid," + f.aggregate + " AS aggregate_id," + column + " AS conversion_payload" + f.filter()
}

func (f storageConversionFamily) filter() string {
	return " FROM " + f.from + " WHERE " + f.scope() + ".organization_id=? AND " + f.scope() + ".project_id=? AND (" + f.where + ")"
}

func (f storageConversionFamily) positions() string {
	return "SELECT r.rowid AS conversion_rowid," + f.aggregate + " AS aggregate_id" + f.filter()
}

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

func (s *Service) convertLegacyStorage(ctx context.Context, now time.Time) error {
	if s.storageArchiveContent() == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, backgroundStorageBudget)
	defer cancel()
	ctx = s.contentContext(ctx)
	rows, err := s.database.reader.QueryContext(ctx, "SELECT organization_id,id FROM projects WHERE profile='native' AND deleted_at IS NULL ORDER BY organization_id,id")
	if err != nil {
		return err
	}
	defer rows.Close()
	var scopes []nativeScope
	for rows.Next() {
		var scope nativeScope
		if err := rows.Scan(&scope.organization, &scope.project); err != nil {
			return err
		}
		scopes = append(scopes, scope)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, scope := range scopes {
		for _, family := range storageConversionFamilies {
			if err := s.convertStorageFamily(ctx, scope, family, now); err != nil {
				failures = errors.Join(failures, fmt.Errorf("convert legacy %s: %w", family.name, err))
				if ctx.Err() != nil {
					return failures
				}
			}
		}
	}
	return failures
}

func (s *Service) convertStorageFamily(ctx context.Context, scope nativeScope, family storageConversionFamily, now time.Time) error {
	rows, err := s.database.reader.QueryContext(ctx, `WITH candidates AS (`+family.positions()+`), aggregates AS (
 SELECT aggregate_id,max(conversion_rowid) AS latest FROM candidates GROUP BY aggregate_id
 UNION ALL SELECT aggregate_id,through_rowid FROM storage_conversion_progress p
 WHERE p.organization_id=? AND p.project_id=? AND p.family=? AND p.complete=0
 AND NOT EXISTS(SELECT 1 FROM candidates c WHERE c.aggregate_id=p.aggregate_id))
SELECT a.aggregate_id,a.latest FROM aggregates a LEFT JOIN storage_conversion_progress p
ON p.organization_id=? AND p.project_id=? AND p.family=? AND p.aggregate_id=a.aggregate_id
WHERE coalesce(p.format_version,0)<>? OR coalesce(p.complete,0)=0 OR a.latest>p.cursor_rowid
ORDER BY coalesce(p.updated_at,''),a.aggregate_id LIMIT 4`, scope.organization, scope.project, scope.organization, scope.project, family.name, scope.organization, scope.project, family.name, storageConversionVersion)
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
	var p storageConversionProgress
	err := s.database.reader.QueryRowContext(ctx, `SELECT format_version,through_rowid,cursor_rowid,converted_rows,reclaimed_bytes,complete FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, scope.organization, scope.project, family.name, aggregate).Scan(&p.Version, &p.Through, &p.Cursor, &p.Rows, &p.Bytes, &p.Complete)
	if err != nil {
		return err
	}
	rows, err := s.database.reader.QueryContext(ctx, family.positions()+" AND "+family.aggregate+"=? AND r.rowid>? AND r.rowid<=? ORDER BY r.rowid LIMIT ?", scope.organization, scope.project, aggregate, p.Cursor, p.Through, storageConversionRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		row       int64
		aggregate string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.row, &c.aggregate); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	written := 0
	for _, c := range candidates {
		var raw []byte
		if err := s.database.reader.QueryRowContext(ctx, "SELECT conversion_payload FROM ("+family.selection()+") WHERE conversion_rowid=?", scope.organization, scope.project, c.row).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		stamp, safe, err := storageConversionStamp(ctx, s.database.reader, scope, family, c.aggregate, now)
		if err != nil || !safe {
			return err
		}
		converted, ref, err := s.prepareStorageConversion(ctx, scope, family, c.row, raw)
		if err != nil {
			return err
		}
		writeBytes := len(converted) + 4096
		if written > 0 && written+writeBytes > storageConversionBytes {
			return nil
		}
		triggerSQL := make([]string, len(family.triggers))
		for i, name := range family.triggers {
			if err := s.database.reader.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?", name).Scan(&triggerSQL[i]); err != nil {
				return err
			}
		}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		applied, err := s.applyStorageConversion(ctx, tx, scope, family, p.Cursor, c.row, c.aggregate, stamp, raw, converted, ref, triggerSQL, now)
		if err == nil && applied {
			err = s.database.commit(ctx, tx)
		}
		rollback := tx.Rollback()
		if err != nil {
			return err
		}
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			return rollback
		}
		if !applied {
			return nil
		}
		written += writeBytes
		p.Cursor = c.row
	}
	if len(candidates) < storageConversionRows || p.Cursor >= p.Through {
		_, err = s.database.db.ExecContext(ctx, `UPDATE storage_conversion_progress SET complete=1,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=? AND cursor_rowid=?`, formatHubTime(now), scope.organization, scope.project, family.name, aggregate, p.Version, p.Cursor)
	}
	return err
}

func storageConversionStamp(ctx context.Context, q nativeQueryer, scope nativeScope, family storageConversionFamily, aggregate string, now time.Time) (string, bool, error) {
	if aggregate == "" {
		return "", true, nil
	}
	var stamp string
	var safe bool
	var err error
	if family.name == "messages" || family.name == "conversation_events" {
		err = q.QueryRowContext(ctx, `SELECT json_array(c.revision,c.event_seq,c.status,c.execution_json),c.status='settled' AND coalesce(json_extract(c.execution_json,'$.status'),'idle') IN ('idle','completed','interrupted','failed') AND NOT EXISTS(SELECT 1 FROM conversation_messages m WHERE m.conversation_id=c.id AND m.delivery IN ('saved','queued','sending','sent','responding')) AND NOT EXISTS(SELECT 1 FROM conversation_questions q WHERE q.conversation_id=c.id AND q.status IN ('pending','sending','sent')) AND NOT EXISTS(SELECT 1 FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND (i.native_id=c.work_item_id OR i.native_id=c.subject_work_item_id OR l.lease_id=json_extract(c.execution_json,'$.lease_id')) AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) AND NOT EXISTS(SELECT 1 FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id WHERE a.organization_id=c.organization_id AND a.project_id=c.project_id AND (a.work_item_id=c.work_item_id OR a.work_item_id=c.subject_work_item_id OR l.lease_id=json_extract(c.execution_json,'$.lease_id')) AND (a.status='running' OR (l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)))) FROM conversations c WHERE c.organization_id=? AND c.project_id=? AND c.id=?`, formatHubTime(now), formatHubTime(now), scope.organization, scope.project, aggregate).Scan(&stamp, &safe)
	} else {
		err = q.QueryRowContext(ctx, `SELECT json_array(i.revision,i.event_sequence,i.workflow_state_id),NOT EXISTS(SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) AND NOT EXISTS(SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.work_item_id=i.native_id AND a.status='running') FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, formatHubTime(now), scope.organization, scope.project, aggregate).Scan(&stamp, &safe)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return stamp, safe, err
}

func (s *Service) prepareStorageConversion(ctx context.Context, scope nativeScope, family storageConversionFamily, row int64, raw []byte) ([]byte, attachment.ArchiveReference, error) {
	var zero attachment.ArchiveReference
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
	if family.name == "runs" && bytes.Contains(raw, []byte(`"detail"`)) || family.name == "attempts" && bytes.Contains(raw, []byte(`"detail"`)) {
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
	} else if family.name != "messages" && family.name != "diff_files" {
		payload := storageJSONPayload{Organization: string(scope.organization), Project: string(scope.project), Family: family.name, Row: row, Data: raw}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, zero, err
		}
		data = encoded
	}
	var ref attachment.ArchiveReference
	var err error
	if family.name == "messages" {
		if s.conversations == nil || s.conversations.store.contents == nil {
			return nil, zero, errors.New("conversation storage unavailable")
		}
		var content conversationContent
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, zero, err
		}
		message := conversationMessageRecord{Role: conversation.RoleSystem, Text: content.Text, Data: content.Data}
		data, err = messageContent(message)
		if err != nil {
			return nil, zero, err
		}
		var uploader string
		if err := s.database.reader.QueryRowContext(ctx, "SELECT token_id FROM token_grants WHERE organization_id=? AND project_id=? ORDER BY token_id LIMIT 1", scope.organization, scope.project).Scan(&uploader); err != nil {
			return nil, zero, err
		}
		prepared, err := s.conversations.prepareContents(ctx, string(scope.organization), string(scope.project), uploader, message)
		if err != nil {
			return nil, zero, err
		}
		contents, ok := prepared.Value(conversationContentsKey{}).(preparedConversationContents)
		if !ok {
			return nil, zero, errors.New("conversation content was not prepared")
		}
		ref = contents[string(scope.organization)+"/"+artifact.Digest(data)]
	} else {
		ref, err = prepareChangeContent(ctx, string(scope.organization), data)
		if err != nil {
			return nil, zero, err
		}
	}
	verified, err := readChangeContent(ctx, string(scope.organization), ref)
	if err != nil {
		return nil, zero, err
	}
	if !bytes.Equal(data, verified) {
		return nil, zero, artifact.ErrIntegrity
	}
	if family.name == "messages" {
		var message conversationContent
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, zero, err
		}
		return []byte(conversationTruncate(message.Text, 256)), ref, nil
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

func (s *Service) applyStorageConversion(ctx context.Context, tx *sql.Tx, scope nativeScope, family storageConversionFamily, cursor, row int64, aggregate, stamp string, raw, converted []byte, ref attachment.ArchiveReference, triggers []string, now time.Time) (bool, error) {
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
	if err := tx.QueryRowContext(ctx, "SELECT aggregate_id=? AND CAST(conversion_payload AS BLOB)=? FROM ("+family.selection()+") WHERE conversion_rowid=?", aggregate, raw, scope.organization, scope.project, row).Scan(&unchanged); err != nil {
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
		for _, name := range family.triggers {
			if _, err := tx.ExecContext(ctx, "DROP TRIGGER "+name); err != nil {
				return false, err
			}
		}
		switch family.name {
		case "messages":
			var uploader string
			if err := tx.QueryRowContext(ctx, "SELECT token_id FROM token_grants WHERE organization_id=? AND project_id=? ORDER BY token_id LIMIT 1", scope.organization, scope.project).Scan(&uploader); err != nil {
				return false, err
			}
			if s.conversations == nil {
				return false, errors.New("conversation storage unavailable")
			}
			if err := s.conversations.store.recordContentArchive(ctx, tx, string(scope.organization), string(scope.project), uploader, ref, now); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE conversation_messages SET text=?,data_json='{}',content_attachment_id=? WHERE rowid=?", string(converted), contentArchiveID(string(scope.organization), ref), row); err != nil {
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
		for _, statement := range triggers {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return false, err
			}
		}
		count, reclaimed = 1, max(0, int64(len(raw)-len(converted)))
		if _, err := tx.ExecContext(ctx, "UPDATE attempt_diff_body_migration SET vacuum_pending=1 WHERE singleton=1"); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET cursor_rowid=?,converted_rows=converted_rows+?,reclaimed_bytes=reclaimed_bytes+?,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=?`, row, count, reclaimed, formatHubTime(now), scope.organization, scope.project, family.name, aggregate, storageConversionVersion)
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
