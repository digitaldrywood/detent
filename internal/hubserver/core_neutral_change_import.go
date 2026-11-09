package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateCoreNeutralChangeImport(bundle *postgres.VerifiedNeutralImport) error {
	return postgres.ValidateNeutralCoreImportTables(bundle, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
}

func coreNeutralPayload(row postgres.NeutralImportRow) (string, error) {
	var key sql.NullString
	if err := row.Decode(map[string]any{"payload_object_key": &key}); err != nil {
		return "", err
	}
	if key.Valid && key.String != "" {
		return key.String, nil
	}
	var raw string
	if err := row.Decode(map[string]any{"record_json": &raw}); err != nil {
		return "", err
	}
	return raw, nil
}

func coreNeutralBytes(row postgres.NeutralImportRow, column string) ([]byte, error) {
	cell, ok := row.Values[column]
	if !ok || cell.Null {
		return nil, postgres.ErrNeutralImport
	}
	switch cell.Encoding {
	case "binary", "utf8":
		return bytes.Clone(cell.Data), nil
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(string(cell.Data))
		if err != nil {
			return nil, postgres.ErrNeutralImport
		}
		return raw, nil
	case "hex":
		raw, err := hex.DecodeString(string(cell.Data))
		if err != nil {
			return nil, postgres.ErrNeutralImport
		}
		return raw, nil
	default:
		return nil, postgres.ErrNeutralImport
	}
}

func (s *coreNeutralImport) changeFilterValue(ctx context.Context, table string, row postgres.NeutralImportRow, column string) (storagecatalog.Cell, error) {
	if column != "change_id" || table != "change_landing_receipts" && table != "change_viewed_files" {
		return storagecatalog.Cell{}, postgres.ErrNeutralImport
	}
	if s.versionChanges == nil {
		owners := make(map[string]string)
		var after []string
		for {
			rows, err := s.bundle.Page(ctx, "change_versions", after, 100)
			if err != nil {
				return storagecatalog.Cell{}, err
			}
			if len(rows) == 0 {
				break
			}
			for _, version := range rows {
				var id, change string
				if err := version.Decode(map[string]any{"id": &id, "change_id": &change}); err != nil {
					return storagecatalog.Cell{}, err
				}
				owners[id] = change
			}
			after = rows[len(rows)-1].Key
		}
		s.versionChanges = owners
	}
	var version string
	if err := row.Decode(map[string]any{"version_id": &version}); err != nil {
		return storagecatalog.Cell{}, err
	}
	owner, ok := s.versionChanges[version]
	if !ok || owner == "" {
		return storagecatalog.Cell{}, eventstore.ErrInvalidRecord
	}
	return storagecatalog.Cell{Encoding: "utf8", Data: []byte(owner)}, nil
}

func hydrateCoreNeutralVersion(ctx context.Context, source *coreNeutralImport, input CoreChangeImportSource, value *tracker.ChangeVersion) error {
	change, err := source.one(ctx, "change_requests", map[string]string{"id": value.ChangeID, "organization_id": string(input.OrganizationID)}, []string{"id"})
	if err != nil {
		return err
	}
	var project string
	if err := change.Decode(map[string]any{"project_id": &project}); err != nil {
		return err
	}
	row, err := source.one(ctx, "policy_revisions", map[string]string{"scope": string(input.OrganizationID) + "/" + project, "policy_id": value.PolicyID}, []string{"scope", "policy_id"})
	if err != nil {
		return err
	}
	var metadata string
	if err := row.Decode(map[string]any{"metadata_json": &metadata}); err != nil {
		return err
	}
	raw, err := resolveCoreNeutralJSON(ctx, source, input.OrganizationID, input.SourceObjects, []byte(metadata))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &value.Policy); err != nil {
		return err
	}
	if err := value.Policy.Validate(); err != nil {
		return err
	}
	if value.Policy.ID != value.PolicyID {
		return errors.New("version policy reference mismatch")
	}
	if value.ReviewPolicy.PolicyID == "" {
		value.ReviewPolicy.PolicyID = value.PolicyID
	}
	if value.ReviewPolicy.RequiredChecks == nil && len(value.Checks) > 0 {
		value.ReviewPolicy.RequiredChecks = make([]tracker.ChangeCheckSpec, len(value.Checks))
		for index, check := range value.Checks {
			value.ReviewPolicy.RequiredChecks[index] = check.ChangeCheckSpec
		}
	}
	if value.Validation == nil {
		return nil
	}
	slots := make(map[string][]attributionSlot)
	commandAttributionSlots(slots, "validation", value.Validation)
	if err := source.each(ctx, "change_finding_attributions", map[string]string{"version_id": value.ID, "record_id": value.ID}, []string{"role", "position"}, func(row postgres.NeutralImportRow) error {
		var role, attribution string
		var position int
		var evidence failureattribution.Evidence
		if err := row.Decode(map[string]any{"role": &role, "position": &position, "attribution": &attribution, "base_sha": &evidence.BaseSHA, "head_sha": &evidence.HeadSHA, "command": &evidence.Command, "diagnostic": &evidence.Diagnostic, "head_output_digest": &evidence.HeadOutputDigest, "base_output_digest": &evidence.BaseOutputDigest, "base_exit_code": &evidence.BaseExitCode}); err != nil {
			return err
		}
		list := slots[role]
		if position < -1 || position+1 >= len(list) {
			return errors.New("finding attribution position is invalid")
		}
		slot := list[position+1]
		if position >= 0 {
			var proof *failureattribution.Evidence
			if evidence.BaseSHA != "" {
				proof = &evidence
			}
			if !failureattribution.Valid(attribution, proof, evidence.BaseSHA, evidence.HeadSHA) {
				return errors.New("stored finding attribution evidence is invalid")
			}
		}
		*slot.value = attribution
		if slot.evidence != nil {
			*slot.evidence = nil
			if evidence.BaseSHA != "" {
				*slot.evidence = &evidence
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read finding attributions: %w", err)
	}
	commands := map[string][]gate.CommandResult{"validation": {*value.Validation}}
	if err := source.each(ctx, "change_command_outputs", map[string]string{"version_id": value.ID, "review_id": value.ID}, []string{"role", "position"}, func(row postgres.NeutralImportRow) error {
		var role, organization string
		var position int
		ref := contentReference()
		if err := row.Decode(map[string]any{"role": &role, "position": &position, "organization_id": &organization, "object_key": &ref.Key, "object_sha256": &ref.SHA256, "object_bytes": &ref.CompressedSize, "content_bytes": &ref.UncompressedSize}); err != nil {
			return err
		}
		list := commands[role]
		if position < 0 || position >= len(list) || organization != string(input.OrganizationID) {
			return errors.New("command output reference mismatch")
		}
		raw, err := readCoreNeutralArchive(ctx, source, input.OrganizationID, input.SourceObjects, ref)
		if err != nil {
			return err
		}
		list[position].Output = string(raw)
		return nil
	}); err != nil {
		return err
	}
	*value.Validation = commands["validation"][0]
	return nil
}

func readCoreNeutralArchive(ctx context.Context, source *coreNeutralImport, organization tracker.OrganizationID, objects attachment.DetailStore, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(string(organization)) != nil {
		return nil, artifact.ErrIntegrity
	}
	raw, err := objects.ReadDetail(ctx, string(organization), ref)
	if err == nil && int64(len(raw)) == ref.UncompressedSize {
		return raw, nil
	}
	raw, err = readCoreNeutralObject(ctx, source, organization, objects, ref.Key)
	if err != nil {
		return nil, err
	}
	prepared, _, err := attachment.PrepareArchive(string(organization), ref.SchemaVersion, bytes.NewReader(raw))
	if err != nil || prepared != ref {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	return raw, nil
}
