package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) PinVerifiedNeutralImport(ctx context.Context, source *VerifiedNeutralImport, objects attachment.DetailStore) error {
	if source == nil || objects == nil {
		return ErrNeutralImport
	}
	identity := source.Identity()
	schema, err := json.Marshal(source.Schema())
	if err != nil {
		return err
	}
	if err := r.PinCoreImportSource(ctx, identity.OrganizationID, identity.SourceSHA256, objects); err != nil {
		return err
	}
	manifest, err := json.Marshal(source.manifest)
	if err != nil {
		return err
	}
	receipt, err := eventstore.NewContent(objects).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: identity.OrganizationID, ActorID: "production-import", Operation: "neutral-source", CommandKey: "source", RequestHash: source.Digest()}, manifest)
	if err != nil {
		return err
	}
	err = r.transaction(ctx, identity.OrganizationID, corestore.Background, false, func(s *session) error {
		if _, err := s.BindNeutralImportSource(ctx, BindNeutralImportSourceInput{OrganizationID: identity.OrganizationID, SourceID: identity.SourceID, SchemaVersion: int64(identity.SchemaVersion), Checksum: source.Digest(), SchemaDigest: artifact.Digest(schema), VerifiedAt: time.Now().UTC()}); err != nil {
			return err
		}
		_, err := s.PutReceipt(ctx, receipt)
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return mutation.ErrConflict
	}
	return err
}
func (r *Repository) importNeutralCoreProjects(ctx context.Context, source CoreProjectImportSource) (int64, error) {
	var count int64
	err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
		if err := validateNeutralCoreSourceSchema(bundle, "projects", "repositories"); err != nil {
			return err
		}
		keys, err := bundle.TableKey("projects")
		if err != nil || !slices.Equal(keys, []NeutralImportKey{{Column: "id", Kind: "text"}}) {
			return ErrNeutralImport
		}
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		count, err = r.importCoreProjectPages(ctx, source, func(after string) ([]coreProjectImportRow, error) {
			var key []string
			if after != "" {
				key = []string{after}
			}
			rows, err := bundle.Page(ctx, "projects", key, 100)
			if err != nil {
				return nil, err
			}
			result := make([]coreProjectImportRow, 0, len(rows))
			for _, raw := range rows {
				row, err := neutralCoreProjectRow(ctx, bundle, source.OrganizationID, raw)
				if err != nil {
					return nil, err
				}
				result = append(result, row)
			}
			return result, nil
		})
		return err
	})
	return count, err
}
func neutralCoreProjectRow(ctx context.Context, bundle *VerifiedNeutralImport, organization tracker.OrganizationID, raw NeutralImportRow) (coreProjectImportRow, error) {
	var row coreProjectImportRow
	c, v := &row.Record.Core, &row.Record.Settings
	c.OrganizationID = organization
	v.OrganizationID = organization
	if err := raw.Decode(map[string]any{
		"id": &c.ID, "name": &c.Name, "slug": &c.Slug, "profile": &c.Profile, "require_dependencies": &c.RequireDependencies, "scheduling_rank": &c.SchedulingRank, "content_settings_revision": &c.ContentSettingsRevision, "workflow_source": &c.WorkflowSource, "workflow_source_revision": &c.WorkflowSourceRevision, "workflow_markdown": &row.Workflow, "states_json": &row.States, "created_at": &row.Created, "deleted_at": &row.Deleted, "integration_revision": &v.Revision, "github_intake": &v.Intake, "github_projection": &v.Projection, "github_repository_enabled": &v.RepositoryEnabled, "checkout_repository": &v.CheckoutRepository, "aging_enabled": &v.AgingEnabled, "duplicate_challenge": &v.DuplicateChallenge, "archive_completed_after_days": &v.ArchiveCompletedAfterDays, "archive_cancelled_after_days": &v.ArchiveCancelledAfterDays, "require_visual_attachments": &v.RequireVisualAttachments, "model_selection_revision": &v.ModelRevision, "repository_id": &row.RepositoryID, "icon_kind": &row.IconKind, "icon_name": &row.IconName, "icon_color": &row.IconColor, "icon_emoji": &row.IconEmoji, "icon_image_key": &row.IconImageKey, "icon_image_sha256": &row.IconImageSHA256,
	}); err != nil {
		return row, err
	}
	if cell, ok := raw.Values["model_selection_json"]; !ok {
		return row, ErrNeutralImport
	} else if !cell.Null {
		if err := raw.Decode(map[string]any{"model_selection_json": &row.Models}); err != nil {
			return row, err
		}
	}
	v.ProjectID = c.ID
	if row.RepositoryID.Valid {
		keys, err := bundle.TableKey("repositories")
		if err != nil || !slices.Equal(keys, []NeutralImportKey{{Column: "id", Kind: "integer"}}) {
			return row, ErrNeutralImport
		}
		repository, err := bundle.Lookup(ctx, "repositories", []string{strconv.FormatInt(row.RepositoryID.Int64, 10)})
		if err != nil {
			return row, err
		}
		var owner, name string
		if err := repository.Decode(map[string]any{"github_owner": &owner, "github_name": &name}); err != nil {
			return row, err
		}
		v.Repository = owner + "/" + name
	}
	return row, nil
}
