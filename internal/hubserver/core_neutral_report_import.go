package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func prepareCoreNeutralRunDetail(ctx context.Context, source *coreNeutralImport, organization tracker.OrganizationID, objects attachment.DetailStore, raw []byte, content *changeContent, owner *runDetailContext) error {
	var envelope struct {
		Detail *runDetailReference `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	detail := envelope.Detail
	if detail == nil {
		return nil
	}
	if detail.Organization != string(organization) {
		return artifact.ErrIntegrity
	}
	if detail.ObjectKey != "" {
		value, err := readCoreNeutralObject(ctx, source, organization, objects, detail.ObjectKey)
		if err != nil {
			return err
		}
		content.payloads[detail.ObjectKey] = value
		return nil
	}
	row, err := source.one(ctx, "attachments", map[string]string{"id": detail.AttachmentID, "organization_id": string(organization), "kind": "archive"}, []string{"id"})
	if err != nil {
		return err
	}
	var ref attachment.ArchiveReference
	if err := row.Decode(map[string]any{"object_key": &ref.Key, "sha256": &ref.SHA256, "size": &ref.CompressedSize, "uncompressed_size": &ref.UncompressedSize, "codec": &ref.Codec, "schema_version": &ref.SchemaVersion, "kind": &ref.Kind, "retention_class": &ref.Retention, "storage_binding_id": &ref.BindingID, "customer_owned": &ref.CustomerOwned}); err != nil {
		return err
	}
	if ref.Validate(string(organization)) != nil || ref.CustomerOwned || ref.BindingID != "" {
		return artifact.ErrIntegrity
	}
	value, err := readCoreNeutralArchive(ctx, source, organization, objects, ref)
	if err != nil {
		return err
	}
	if int64(len(value)) != ref.UncompressedSize {
		return artifact.ErrIntegrity
	}
	var runs map[string]tracker.NativeRunData
	if err := json.Unmarshal(value, &runs); err != nil {
		return err
	}
	owner.cache[detail.AttachmentID] = runs
	return nil
}

func validateCoreNeutralUsageScope(ctx context.Context, source *coreNeutralImport, organization tracker.OrganizationID) error {
	var after []string
	for {
		rows, err := source.bundle.Page(ctx, "attempt_usage", after, 100)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			var tenant tracker.OrganizationID
			var project tracker.ProjectID
			var attemptID, currency string
			if err := row.Decode(map[string]any{"organization_id": &tenant, "project_id": &project, "attempt_id": &attemptID, "currency": &currency}); err != nil {
				return err
			}
			if currency != "USD" {
				return errors.New("report usage source has unsupported currency")
			}
			if tenant != organization {
				return errors.New("report usage source has invalid attempt scope")
			}
			if _, err := source.one(ctx, "native_attempts", map[string]string{"id": attemptID, "organization_id": string(tenant), "project_id": string(project)}, []string{"id"}); err != nil {
				return errors.Join(errors.New("report usage source has invalid attempt scope"), err)
			}
		}
		after = rows[len(rows)-1].Key
	}
}
