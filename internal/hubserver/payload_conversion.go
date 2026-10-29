package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const analyticsPayloadPendingSQL = "SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups INDEXED BY analytics_attempt_payload_pending_scope WHERE organization_id=? AND project_id=? AND metric='attempt' AND payload_object_key='' AND data_json<>'{}')"

type objectPayloadConversionPosition struct {
	row       int64
	scope     nativeScope
	aggregate string
}

func objectPayloadFamily(name string) bool {
	switch name {
	case "attempts", "analytics", "checkpoints", "landings", "versions", "evidence", "sources", "diff_files":
		return true
	}
	return false
}

func (s *Service) convertObjectPayloadFamily(ctx context.Context, family storageConversionFamily, now time.Time) error {
	predicate := "r.payload_object_key=''"
	switch family.name {
	case "landings":
		predicate = family.where
	case "analytics":
		predicate += " AND r.metric='attempt' AND r.data_json<>'{}'"
	case "checkpoints":
		predicate = family.where
	case "sources":
		predicate = "r.object_key=''"
	case "diff_files":
		predicate = "1"
	}
	aggregateColumn := family.aggregate
	switch family.name {
	case "versions", "evidence":
		aggregateColumn = "c.id"
	case "sources":
		aggregateColumn = "r.version_id"
	case "diff_files":
		aggregateColumn = "r.diff_id"
	}
	stale, err := s.database.reader.QueryContext(ctx, `SELECT organization_id,project_id,aggregate_id FROM storage_conversion_progress WHERE family=? AND format_version<>2 LIMIT 4`, family.name)
	if err != nil {
		return err
	}
	defer stale.Close()
	var retired [][3]string
	for stale.Next() {
		var id [3]string
		if err := stale.Scan(&id[0], &id[1], &id[2]); err != nil {
			return errors.Join(err, stale.Close())
		}
		retired = append(retired, id)
	}
	if err := errors.Join(stale.Err(), stale.Close()); err != nil {
		return err
	}
	for _, id := range retired {
		if _, err := s.database.db.ExecContext(ctx, `DELETE FROM storage_conversion_progress WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version<>2`, id[0], id[1], family.name, id[2]); err != nil {
			return err
		}
	}
	selection := strings.Join([]string{"SELECT r.rowid,", family.scope(), ".organization_id,", family.scope(), ".project_id,", aggregateColumn, " FROM ", family.from, " WHERE ", predicate}, "")
	var args []any
	selection += " LIMIT ?"
	args = append(args, storageConversionRows)
	rows, err := s.database.reader.QueryContext(ctx, selection, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var candidates []objectPayloadConversionPosition
	for rows.Next() {
		var c objectPayloadConversionPosition
		if err := rows.Scan(&c.row, &c.scope.organization, &c.scope.project, &c.aggregate); err != nil {
			return errors.Join(err, rows.Close())
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	processed := 0
	for _, c := range candidates {
		raw, err := readObjectConversionPayload(ctx, s.database.reader, family, c.row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if processed > 0 && processed+len(raw) > storageConversionBytes {
			return nil
		}
		data := raw
		var run tracker.NativeRunData
		var checkpoint *tracker.NativeCheckpoint
		switch family.name {
		case "attempts":
			if err := decodeRunDetail(ctx, s.database.reader, string(raw), &run); err != nil {
				return err
			}
			data, err = json.Marshal(run)
		case "checkpoints":
			checkpoint, err = decodeRunCheckpoint(ctx, s.database.reader, string(raw))
			if err == nil {
				var artifactsRaw string
				if err := s.database.reader.QueryRowContext(ctx, "SELECT artifact_ids_json FROM native_attempts WHERE rowid=?", c.row).Scan(&artifactsRaw); err != nil {
					return err
				}
				var artifacts []string
				if err := json.Unmarshal([]byte(artifactsRaw), &artifacts); err != nil {
					return err
				}
				data, err = json.Marshal(checkpointPayload{Checkpoint: checkpoint, ArtifactIDs: artifacts})
			}
		case "landings":
			var receipt tracker.NativeLandingReceipt
			if err := decodeStorageJSON(ctx, raw, &receipt); err != nil {
				return err
			}
			var document string
			if err := s.database.reader.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(a.payload_object_key,''),a.data_json) FROM native_attempts a JOIN change_landing_receipts r ON r.attempt_id=a.id WHERE r.rowid=?", c.row).Scan(&document); err != nil {
				return err
			}
			data, err = resolveStorageJSON(ctx, []byte(document))
			if err == nil {
				var run tracker.NativeRunData
				if err := json.Unmarshal(data, &run); err != nil {
					return err
				}
				if run.Runtime == nil || !reflect.DeepEqual(run.Runtime.Landing, &receipt) {
					data, err = json.Marshal(tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &receipt}})
				}
			}
		case "analytics", "versions", "evidence":
			data, err = resolveStorageJSON(ctx, raw)
		case "sources":
			var change, version string
			if err := s.database.reader.QueryRowContext(ctx, "SELECT v.change_id,v.id FROM change_sources r JOIN change_versions v ON v.id=r.version_id WHERE r.rowid=?", c.row).Scan(&change, &version); err != nil {
				return err
			}
			var versionRaw string
			if err := s.database.reader.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?", change, version).Scan(&versionRaw); err != nil {
				return err
			}
			var value tracker.ChangeVersion
			if err := decodeStorageJSON(ctx, []byte(versionRaw), &value); err != nil {
				return err
			}
			if value.Source == nil {
				return artifact.ErrIntegrity
			}
			data, err = decodeChangeSource(raw, value.Source.Bytes)
			if err == nil {
				err = value.Source.Validate(value.BaseSHA, value.HeadSHA, data)
			}
		}
		if err != nil {
			return err
		}
		var analyticsValues []any
		if family.name == "analytics" {
			analyticsValues, err = analyticsProjection(string(data))
			if err != nil {
				return err
			}
			analyticsValues = append(analyticsValues, c.row, string(raw))
		}
		ref, err := prepareChangeContent(ctx, string(c.scope.organization), data)
		if err != nil {
			return err
		}
		restored, err := readChangeContent(ctx, string(c.scope.organization), ref)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, restored) {
			return artifact.ErrIntegrity
		}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = s.applyObjectPayloadConversion(ctx, tx, c.scope, family, c.row, c.aggregate, raw, data, ref, run, checkpoint, analyticsValues, now)
		if err == nil {
			err = s.database.commit(ctx, tx)
		}
		rollback := tx.Rollback()
		if err != nil {
			return err
		}
		if rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			return rollback
		}
		processed += len(raw)
	}
	return nil
}

func readObjectConversionPayload(ctx context.Context, q nativeQueryer, family storageConversionFamily, row int64) ([]byte, error) {
	if family.name != "diff_files" {
		var raw []byte
		err := q.QueryRowContext(ctx, "SELECT "+family.column+" FROM "+family.table()+" WHERE rowid=?", row).Scan(&raw)
		return raw, err
	}
	var stored storageDiffFile
	var body diffbody.Reference
	err := q.QueryRowContext(ctx, `SELECT diff_id,position,path,old_path,status,additions,deletions,binary,patch,truncated,denied,patch_expired,patch_object,patch_sha256,patch_size FROM attempt_diff_files WHERE rowid=?`, row).Scan(&stored.DiffID, &stored.Position, &stored.File.Path, &stored.File.OldPath, &stored.File.Status, &stored.File.Additions, &stored.File.Deletions, &stored.File.Binary, &stored.File.Patch, &stored.File.Truncated, &stored.File.Denied, &stored.File.PatchExpired, &body.Key, &body.SHA256, &body.Bytes)
	if err != nil {
		return nil, err
	}
	if body.Key != "" && !stored.File.PatchExpired {
		stored.File.PatchBody = &body
	}
	return json.Marshal(stored)
}

func (s *Service) applyObjectPayloadConversion(ctx context.Context, tx *sql.Tx, scope nativeScope, family storageConversionFamily, row int64, aggregate string, raw, data []byte, ref attachment.ArchiveReference, run tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint, analyticsValues []any, now time.Time) error {
	current, err := readObjectConversionPayload(ctx, tx, family, row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, raw) {
		return nil
	}
	column := "payload_object_key"
	switch family.name {
	case "checkpoints":
		column = "checkpoint_object_key"
	case "sources":
		column = "object_key"
	case "diff_files":
		column = ""
	}
	if column != "" {
		var key string
		if err := tx.QueryRowContext(ctx, "SELECT "+column+" FROM "+family.table()+" WHERE rowid=?", row).Scan(&key); err != nil {
			return err
		}
		if key != "" {
			return nil
		}
	}
	before, after := storageConversionDigest(raw, ""), storageConversionDigest("{}", ref.Key)
	if family.name == "sources" {
		before = storageConversionDigest(raw, "", "", int64(0), int64(0))
		after = storageConversionDigest([]byte{}, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize)
	}
	var policyID string
	if family.name == "versions" {
		var version tracker.ChangeVersion
		if err := json.Unmarshal(data, &version); err != nil {
			return err
		}
		policyID = version.PolicyID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at,conversion_rowid,conversion_before_sha256,conversion_after_sha256,conversion_policy_revision_id)
VALUES(?,?,?,?,2,?,?,?,?,?,?) ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET format_version=2,complete=0,conversion_policy_revision_id=excluded.conversion_policy_revision_id,conversion_rowid=excluded.conversion_rowid,conversion_before_sha256=excluded.conversion_before_sha256,conversion_after_sha256=excluded.conversion_after_sha256,through_rowid=max(through_rowid,excluded.through_rowid),updated_at=excluded.updated_at`, scope.organization, scope.project, family.name, aggregate, row, formatHubTime(now), row, before, after, policyID)
	if err != nil {
		return err
	}
	switch family.name {
	case "analytics":
		if _, err := tx.ExecContext(ctx, analyticsProjectionBackfill, analyticsValues...); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE analytics_hourly_rollups SET data_json='{}',payload_object_key=?,conversion_state=2 WHERE rowid=?", ref.Key, row)
	case "attempts":
		var id, status string
		if err := tx.QueryRowContext(ctx, "SELECT id,status FROM native_attempts WHERE rowid=?", row).Scan(&id, &status); err != nil {
			return err
		}
		if err := updateActivityAttemptProjection(ctx, tx, id, run, run.Handoff); err != nil {
			return err
		}
		if err := updateAttemptClaimFacts(ctx, tx, id, run); err != nil {
			return err
		}
		if err := updateAttemptHotProjection(ctx, tx, id, status, run); err != nil {
			return err
		}
		if err := writeAttemptUsageFacts(ctx, tx, id, run); err != nil {
			return err
		}
		if err := writeAttemptSummary(ctx, tx, id, run); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE native_attempts SET data_json='{}',payload_object_key=?,conversion_state=2 WHERE rowid=?", ref.Key, row)
	case "checkpoints":
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM native_attempts WHERE rowid=?", row).Scan(&id); err != nil {
			return err
		}
		if err := writeCheckpointSummary(ctx, tx, id, checkpoint); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE native_attempts SET checkpoint_json='null',artifact_ids_json='[]',checkpoint_object_key=?,checkpoint_preserves_source=? WHERE rowid=?", ref.Key, checkpoint.PreservesSource(), row)
	case "landings":
		receipt, decodeErr := decodeLandingReceipt(ctx, string(data), true)
		if decodeErr != nil {
			return decodeErr
		}
		_, err = tx.ExecContext(ctx, "UPDATE change_landing_receipts SET record_json='{}',payload_object_key=?,merge_sha=?,conversion_state=2 WHERE rowid=?", ref.Key, receipt.MergeSHA, row)
	case "versions", "evidence":
		columns, projectionErr := changeRecordColumns(ctx, tx, family.table(), string(data))
		if projectionErr != nil {
			return projectionErr
		}
		assignments := []string{"record_json='{}'", "payload_object_key=?", "conversion_state=2"}
		values := []any{ref.Key}
		for i, column := range columns.columns {
			if column == "policy_revision_id" {
				assignments = append(assignments, "policy_revision_id=CASE WHEN policy_revision_id='' THEN ? ELSE policy_revision_id END")
			} else {
				assignments = append(assignments, column+"=?")
			}
			values = append(values, columns.values[i])
		}
		values = append(values, row)
		_, err = tx.ExecContext(ctx, strings.Join([]string{"UPDATE ", family.table(), " SET ", strings.Join(assignments, ","), " WHERE rowid=?"}, ""), values...)

	case "sources":
		_, err = tx.ExecContext(ctx, "UPDATE change_sources SET bundle=x'',object_key=?,object_sha256=?,object_bytes=?,content_bytes=?,conversion_state=2 WHERE rowid=?", ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row)
	case "diff_files":
		_, err = tx.ExecContext(ctx, `INSERT INTO attempt_diff_file_archives(diff_id,position,organization_id,project_id,object_key,object_sha256,object_bytes,content_bytes) SELECT diff_id,position,?,?,?,?,?,? FROM attempt_diff_files WHERE rowid=?`, scope.organization, scope.project, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, row)
		if err == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM attempt_diff_files WHERE rowid=?", row)
		}
	}
	if err != nil {
		return fmt.Errorf("convert object payload %s: %w", family.name, err)
	}
	pendingQuery := ""
	var pendingArgs []any
	switch family.name {
	case "analytics":
		pendingQuery = analyticsPayloadPendingSQL
		pendingArgs = []any{scope.organization, scope.project}
	case "attempts":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND payload_object_key='')"
		pendingArgs = []any{scope.organization, scope.project, aggregate}
	case "checkpoints":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND checkpoint_object_key='' AND checkpoint_json IS NOT NULL AND checkpoint_json<>'null')"
		pendingArgs = []any{scope.organization, scope.project, aggregate}
	case "landings":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM change_landing_receipts WHERE attempt_id=? AND payload_object_key='' AND record_json<>'{}')"
		pendingArgs = []any{aggregate}
	case "versions", "evidence":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM " + family.table() + " WHERE change_id=? AND payload_object_key='')"
		pendingArgs = []any{aggregate}
	case "sources":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM change_sources WHERE version_id=? AND object_key='')"
		pendingArgs = []any{aggregate}
	case "diff_files":
		pendingQuery = "SELECT EXISTS(SELECT 1 FROM attempt_diff_files WHERE diff_id=?)"
		pendingArgs = []any{aggregate}
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, pendingQuery, pendingArgs...).Scan(&pending); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET complete=?,conversion_policy_revision_id='',conversion_rowid=0,conversion_before_sha256='',conversion_after_sha256='',cursor_rowid=?,converted_rows=converted_rows+1,reclaimed_bytes=reclaimed_bytes+?,updated_at=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=?`, !pending, row, len(raw), formatHubTime(now), scope.organization, scope.project, family.name, aggregate)
	return err
}
