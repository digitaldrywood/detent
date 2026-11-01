package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeColumnFamily struct {
	table, key, payload, pending string
}

func (f changeColumnFamily) payloadColumn() string {
	switch f.table {
	case "change_versions", "change_evidence", "change_landing_receipts", "native_attempts":
		return "COALESCE(NULLIF(payload_object_key,'')," + f.payload + ")"
	}
	return f.payload
}

var changeColumnFamilies = []changeColumnFamily{
	{"change_requests", "id", "record_json", "created_at"},
	{"change_versions", "id", "record_json", "head_sha"},
	{"change_evidence", "sequence", "record_json", "has_validator"},
	{"quality_landings", "version_id", "record_json", "landed_at"},
	{"change_landing_receipts", "attempt_id", "record_json", "merge_sha"},
	{"native_attempts", "id", "data_json", "activity_identity_known"},
	{"collaboration_events", "id", "data_json", "from_state"},
	{"issues", "id", "actor_json", "actor_principal_id"},
	{"native_comments", "id", "actor_json", "change_actor_kind"},
}

type changeColumnValues struct {
	checkpointSource *sql.NullString
	columns          []string
	values           []any
}

func (v *changeColumnValues) add(column string, value any) {
	v.columns = append(v.columns, column)
	v.values = append(v.values, value)
}

func decodeChangeColumnsJSON(ctx context.Context, raw []byte, result any) error {
	if bytes.HasPrefix(raw, []byte("orgs/")) {
		return decodeStorageJSON(ctx, raw, result)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for key := range object {
		if key != "_storage_archive" {
			return json.Unmarshal(raw, result)
		}
	}
	reference, found := object["_storage_archive"]
	if !found {
		return json.Unmarshal(raw, result)
	}
	var ref storageJSONReference
	if err := json.Unmarshal(reference, &ref); err != nil {
		return err
	}
	data, err := readChangeContent(ctx, ref.Organization, ref.Reference)
	if err != nil {
		return err
	}
	var payload storageJSONPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.Organization != ref.Organization || payload.Project != ref.Project || payload.Family != ref.Family || payload.Row != ref.Row || !json.Valid(payload.Data) {
		return artifact.ErrIntegrity
	}
	return json.Unmarshal(payload.Data, result)
}

func changeRecordColumns(ctx context.Context, query nativeQueryer, family, raw string) (changeColumnValues, error) {
	var columns changeColumnValues
	switch family {
	case "change_requests":
		var c tracker.ChangeRequest
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &c); err != nil {
			return columns, err
		}
		columns.add("created_at", changeColumnTime(c.CreatedAt))
		columns.add("updated_at", changeColumnTime(c.UpdatedAt))
		landing := tracker.ChangeLanding{}
		if c.Landed != nil {
			landing = *c.Landed
		}
		columns.add("landed_version_id", landing.VersionID)
		columns.add("landed_head_sha", landing.HeadSHA)
		columns.add("landed_merge_sha", landing.MergeSHA)
		columns.add("landed_base_ref", landing.BaseRef)
		columns.add("landed_at", changeColumnTime(landing.LandedAt))
	case "change_versions":
		var v tracker.ChangeVersion
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &v); err != nil {
			return columns, err
		}
		columns.add("created_at", changeColumnTime(v.CreatedAt))
		columns.add("attempt_id", v.AttemptID)
		columns.add("head_sha", v.HeadSHA)
		columns.add("repository", v.Repository)
		columns.add("policy_revision_id", v.PolicyID)
		external := tracker.ChangeExternalReference{}
		if v.External != nil {
			external = *v.External
		}
		columns.add("external_provider", external.Provider)
		columns.add("external_id", external.ID)
		columns.add("external_url", external.URL)
	case "change_evidence":
		var e struct {
			Decision             string                       `json:"decision"`
			Actor                tracker.Actor                `json:"actor"`
			ImplementationReview *gate.ValidatorResult        `json:"implementation_review"`
			Validator            *gate.ValidatorResult        `json:"validator"`
			SecurityAudit        *tracker.SecurityAuditReview `json:"security_audit"`
			CheckRunID           string                       `json:"check_run_id"`
			CreatedAt            time.Time                    `json:"created_at"`
			ReceivedAt           time.Time                    `json:"received_at"`
			Escape               *tracker.QualityEscape       `json:"escape"`
		}
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &e); err != nil {
			return columns, err
		}
		columns.add("claim_review_decision", e.Decision)
		columns.add("claim_review_actor", e.Actor.PrincipalID)
		columns.add("has_implementation_review", e.ImplementationReview != nil)
		columns.add("has_validator", e.Validator != nil)
		columns.add("has_security_audit", e.SecurityAudit != nil)
		columns.add("security_audit_landing", e.SecurityAudit != nil && e.SecurityAudit.Landing)
		columns.add("check_run_id", e.CheckRunID)
		columns.add("created_at", changeColumnTime(e.CreatedAt))
		var received any
		if !e.ReceivedAt.IsZero() {
			received = changeColumnTime(e.ReceivedAt)
		}
		columns.add("received_at", received)
		escape := tracker.QualityEscape{}
		if e.Escape != nil {
			escape = *e.Escape
		}
		columns.add("escape_occurrence_id", escape.OccurrenceID)
		columns.add("escape_observed_at", changeColumnTime(escape.ObservedAt))
	case "quality_landings":
		var l tracker.ChangeLanding
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &l); err != nil {
			return columns, err
		}
		columns.add("landed_at", changeColumnTime(l.LandedAt))
	case "change_landing_receipts":
		var l tracker.NativeLandingReceipt
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &l); err != nil {
			return columns, err
		}
		columns.add("merge_sha", l.MergeSHA)
	case "native_attempts":
		var run tracker.NativeRunData
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &run); err != nil {
			return columns, err
		}
		columns.add("policy_id", run.PolicyID)
		columns.add("has_landing_receipt", run.Runtime != nil && run.Runtime.Landing != nil)
		columns.add("runner_id", run.RunnerID)
		columns.add("activity_identity_known", true)
		finalID, finalURL := "", ""
		if run.Finalization != nil {
			finalID = run.Finalization.ChangeID
			if run.Finalization.Publication != nil {
				finalURL = run.Finalization.Publication.External.URL
			}
		}
		columns.add("finalization_change_id", finalID)
		columns.add("finalization_external_url", finalURL)
		var checkpointID any
		if run.Handoff != nil {
			checkpointID = ""
			if run.Handoff.Change != nil {
				checkpointID = run.Handoff.Change.ChangeID
			}
		}
		columns.add("checkpoint_change_id", checkpointID)
	case "collaboration_events":
		var e tracker.CollaborationData
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &e); err != nil {
			return columns, err
		}
		return changeEventColumns(e), nil
	case "issues", "native_comments":
		var actor tracker.Actor
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &actor); err != nil {
			return columns, err
		}
		if family == "issues" {
			columns.add("actor_principal_id", actor.PrincipalID)
		} else {
			columns.add("change_actor_kind", actor.Kind)
		}
	default:
		return columns, errors.New("unknown Change column family")
	}
	return columns, nil
}

var changeColumnUpdates = map[string]string{
	"change_requests":         `UPDATE change_requests SET created_at=?,updated_at=?,landed_version_id=?,landed_head_sha=?,landed_merge_sha=?,landed_base_ref=?,landed_at=? WHERE id=? AND record_json=? AND (?=0 OR created_at IS NULL)`,
	"change_versions":         `UPDATE change_versions SET created_at=?,attempt_id=?,head_sha=?,repository=?,policy_revision_id=?,external_provider=?,external_id=?,external_url=? WHERE id=? AND record_json=? AND (?=0 OR head_sha IS NULL)`,
	"change_evidence":         `UPDATE change_evidence SET claim_review_decision=?,claim_review_actor=?,has_implementation_review=?,has_validator=?,has_security_audit=?,security_audit_landing=?,check_run_id=?,created_at=?,received_at=?,escape_occurrence_id=?,escape_observed_at=? WHERE sequence=? AND record_json=? AND (?=0 OR has_validator IS NULL)`,
	"quality_landings":        `UPDATE quality_landings SET landed_at=? WHERE version_id=? AND record_json=? AND (?=0 OR landed_at IS NULL)`,
	"change_landing_receipts": `UPDATE change_landing_receipts SET merge_sha=? WHERE attempt_id=? AND record_json=? AND (?=0 OR merge_sha IS NULL)`,
	"native_attempts":         `UPDATE native_attempts SET policy_id=?,has_landing_receipt=?,runner_id=?,activity_identity_known=?,finalization_change_id=?,finalization_external_url=?,checkpoint_change_id=COALESCE(?,checkpoint_change_id) WHERE id=? AND data_json=? AND (?=0 OR activity_identity_known IS NULL)`,
	"collaboration_events":    `UPDATE collaboration_events SET from_state=?,to_state=?,push_external_effect=?,push_effect_state=?,push_change_id=?,push_version_id=?,push_head_sha=?,claim_attempt_id=?,run_id=?,run_lease_id=?,run_fencing_token=?,run_policy_id=?,run_sequence=? WHERE id=? AND data_json=? AND (?=0 OR from_state IS NULL)`,
	"issues":                  `UPDATE issues SET actor_principal_id=? WHERE id=? AND actor_json=? AND (?=0 OR actor_principal_id IS NULL)`,
	"native_comments":         `UPDATE native_comments SET change_actor_kind=?,change_edited_by_kind=COALESCE(change_edited_by_kind,?) WHERE id=? AND actor_json=? AND (?=0 OR change_actor_kind IS NULL)`,
	"change_requests_legacy":  `UPDATE change_requests SET created_at=?,updated_at=?,landed_version_id=?,landed_head_sha=?,landed_merge_sha=?,landed_base_ref=?,landed_at=?,claim_version_id=?,claim_head_sha=? WHERE id=? AND record_json=? AND (?=0 OR created_at IS NULL)`,
}

func writeChangeColumns(ctx context.Context, tx *sql.Tx, family changeColumnFamily, id any, raw string, columns changeColumnValues, pendingOnly ...bool) error {
	owner := family.table
	if owner == "change_requests" && len(columns.values) == 9 {
		owner = "change_requests_legacy"
	}
	statement, found := changeColumnUpdates[owner]
	if !found {
		return errors.New("unknown Change column family")
	}
	pending := len(pendingOnly) > 0 && pendingOnly[0]
	values := append([]any(nil), columns.values...)
	if owner == "change_versions" && pending {
		statement = strings.Replace(statement, "policy_revision_id=?,", "", 1)
		for index, column := range columns.columns {
			if column == "policy_revision_id" {
				values = append(values[:index], values[index+1:]...)
				break
			}
		}
	}
	args := append(values, id)
	if !pending && (owner == "change_versions" || owner == "change_evidence" || owner == "native_attempts") {
		statement = strings.Replace(statement, " AND "+family.payload+"=?", "", 1)
	} else {
		statement = strings.Replace(statement, " AND "+family.payload+"=?", " AND "+family.payloadColumn()+"=?", 1)
		args = append(args, raw)
	}
	args = append(args, pending)
	if columns.checkpointSource != nil {
		statement += " AND COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) IS ?"
		args = append(args, *columns.checkpointSource)
	}
	_, err := tx.ExecContext(ctx, statement, args...)
	return err
}

func projectChangeColumns(ctx context.Context, tx *sql.Tx, table string, id any, raw string) error {
	for _, family := range changeColumnFamilies {
		if family.table != table {
			continue
		}
		columns, err := changeRecordColumns(ctx, tx, table, raw)
		if err != nil {
			return err
		}
		return writeChangeColumns(ctx, tx, family, id, raw, columns)
	}
	return errors.New("unknown Change column family")
}

func readChangeColumnIdentity(ctx context.Context, query nativeQueryer, id, raw string) (string, string, error) {
	var change struct {
		CurrentVersion string `json:"current_version_id"`
	}
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &change); err != nil {
		return "", "", err
	}
	if change.CurrentVersion == "" {
		return "", "", nil
	}
	if err := query.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?`, id, change.CurrentVersion).Scan(&raw); err != nil {
		return "", "", err
	}
	var version struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &version); err != nil {
		return "", "", err
	}
	return change.CurrentVersion, version.HeadSHA, nil
}

func decodeChangeColumnCheckpoint(ctx context.Context, raw string) (*tracker.NativeCheckpoint, error) {
	var checkpoint tracker.NativeCheckpoint
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

const changeColumnsPerTick = 100

func (s *Service) populateChangeColumns(ctx context.Context) error {
	ctx = s.runDetailContext(ctx)
	var failures error
	for range changeColumnsPerTick {
		changed, err := s.populateChangeColumn(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failures = errors.Join(failures, err)
		if !changed {
			return failures
		}
	}
	return failures
}

func (s *Service) populateChangeColumn(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for s.changeColumnFamily < len(changeColumnFamilies) {
		family := changeColumnFamilies[s.changeColumnFamily]
		var id any
		var raw string
		statement := "SELECT " + family.key + "," + family.payloadColumn() + " FROM " + family.table + " WHERE " + family.pending + " IS NULL"
		var args []any
		if s.changeColumnAfter != nil {
			statement += " AND " + family.key + ">?"
			args = append(args, s.changeColumnAfter)
		}
		err := s.database.reader.QueryRowContext(ctx, statement+" ORDER BY "+family.key+" LIMIT 1", args...).Scan(&id, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			s.changeColumnFamily++
			s.changeColumnAfter = nil
			continue
		}
		if err != nil {
			return true, err
		}
		s.changeColumnAfter = id
		columns, err := changeRecordColumns(ctx, s.database.reader, family.table, raw)
		if err != nil {
			return true, fmt.Errorf("decode %s columns: %w", family.table, err)
		}
		if family.table == "change_requests" {
			version, head, err := readChangeColumnIdentity(ctx, s.database.reader, fmt.Sprint(id), raw)
			if err != nil {
				return true, err
			}
			columns.add("claim_version_id", version)
			columns.add("claim_head_sha", head)
		}
		if family.table == "native_attempts" {
			var checkpoint sql.NullString
			if err := s.database.reader.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json) FROM native_attempts WHERE id=?`, id).Scan(&checkpoint); err != nil {
				return true, err
			}
			checkpointID := ""
			if checkpoint.Valid {
				value, err := decodeChangeColumnCheckpoint(ctx, checkpoint.String)
				if err != nil {
					return true, err
				}
				if value != nil && value.Change != nil {
					checkpointID = value.Change.ChangeID
				}
			}
			columns.values[len(columns.values)-1] = checkpointID
			columns.checkpointSource = &checkpoint
		}
		if family.table == "native_comments" {
			var edited sql.NullString
			if err := s.database.reader.QueryRowContext(ctx, `SELECT edited_by_json FROM native_comments WHERE id=?`, id).Scan(&edited); err != nil {
				return true, err
			}
			actor := tracker.Actor{}
			if edited.Valid {
				if err := decodeChangeColumnsJSON(ctx, []byte(edited.String), &actor); err != nil {
					return true, err
				}
			}
			columns.add("change_edited_by_kind", actor.Kind)
		}
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return true, err
		}
		err = writeChangeColumns(ctx, tx, family, id, raw, columns, true)
		if err == nil {
			err = s.database.commit(ctx, tx)
		}
		_ = tx.Rollback()
		return true, err
	}
	s.changeColumnFamily = 0
	s.changeColumnAfter = nil
	return s.populateChangeSession(ctx)
}

func readPriorChangeApproval(ctx context.Context, query nativeQueryer, change, version string) (bool, error) {
	var found bool
	err := query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence WHERE change_id=? AND kind='review' AND version_id<>? AND claim_review_decision='approved' AND has_validator=0 AND has_implementation_review=0)`, change, version).Scan(&found)
	if err != nil || found {
		return found, err
	}
	reviews, err := changeColumnRecords[tracker.ChangeReview](ctx, query, `SELECT COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND kind='review' AND version_id<>? AND has_validator IS NULL ORDER BY sequence`, change, version)
	if err != nil {
		return false, err
	}
	for _, review := range reviews {
		if review.Decision == "approved" && review.Validator == nil && review.ImplementationReview == nil {
			return true, nil
		}
	}
	return false, nil
}

func decodeQualityLanding(ctx context.Context, raw string, landing *tracker.ChangeLanding) error {
	var record struct {
		tracker.ChangeLanding
		Landed *tracker.ChangeLanding `json:"landed"`
	}
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &record); err != nil {
		return err
	}
	*landing = record.ChangeLanding
	if record.Landed != nil {
		*landing = *record.Landed
	}
	return nil
}

func changeColumnTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func changeEventColumns(e tracker.CollaborationData) changeColumnValues {
	var columns changeColumnValues
	columns.add("from_state", e.FromState)
	columns.add("to_state", e.ToState)
	run := tracker.NativeRunData{}
	if e.Run != nil {
		run = *e.Run
	}
	checkpoint := tracker.NativeCheckpoint{}
	if run.Handoff != nil {
		checkpoint = *run.Handoff
	}
	change := tracker.NativeChangeReference{}
	if checkpoint.Change != nil {
		change = *checkpoint.Change
	}
	columns.add("push_external_effect", checkpoint.ExternalEffect)
	columns.add("push_effect_state", checkpoint.EffectState)
	columns.add("push_change_id", change.ChangeID)
	columns.add("push_version_id", change.VersionID)
	columns.add("push_head_sha", change.HeadSHA)
	columns.add("claim_attempt_id", run.AttemptID)
	columns.add("run_id", run.RunID)
	columns.add("run_lease_id", run.LeaseID)
	columns.add("run_fencing_token", run.FencingToken)
	columns.add("run_policy_id", run.PolicyID)
	columns.add("run_sequence", run.Sequence)
	return columns
}

func changeColumnRecords[T any](ctx context.Context, query nativeQueryer, statement string, args ...any) (_ []T, resultErr error) {
	rows, err := queryChangeLegacyRows(ctx, query, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func(rows *changeLegacyRows) { resultErr = errors.Join(resultErr, rows.Close()) }(rows)
	values := []T{}
	for rows.Next() {
		var raw string
		var value T
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
