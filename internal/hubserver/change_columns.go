package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeColumnFamily struct {
	table, key, payload, pending string
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

type changeColumnCursor struct {
	Review         string
	Checkpoint     string
	CheckpointTurn bool
}

func (s *Service) populateChangeColumns(ctx context.Context) error {
	cursor, _ := s.changeColumnAfter.(changeColumnCursor)
	if prior, ok := s.changeColumnAfter.(string); ok {
		cursor.Review = prior
	}
	checkpointTurn := cursor.CheckpointTurn
	cursor.CheckpointTurn = !cursor.CheckpointTurn
	defer func() { s.changeColumnAfter = cursor }()
	if checkpointTurn {
		next, err := s.populateCheckpointSource(ctx, tracker.OrganizationID(s.appOrganizationID()), cursor.Checkpoint)
		if next != "" || err == nil {
			cursor.Checkpoint = next
		}
		return err
	}
	owner, ok := s.config.TenantRepository.(interface {
		BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
	})
	if !ok {
		return workItemStorageUnavailable()
	}
	_, next, err := owner.BackfillCurrentChangeReviewsAfter(ctx, tracker.OrganizationID(s.appOrganizationID()), eventstore.NewContent(s.runDetailStore()), corestore.MaxPageSize, cursor.Review)
	if next != "" || err == nil {
		cursor.Review = next
	}
	return err
}

func changeColumnTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
