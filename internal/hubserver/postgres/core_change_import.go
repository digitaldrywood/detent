package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Repository) ImportCoreNativePRPublication(ctx context.Context, record eventstore.NativePRPublication, receipt eventstore.Receipt) error {
	if err := validateCoreImportReceipt(record.OrganizationID, receipt, "core-change-publication", record.VersionID); err != nil {
		return err
	}
	if record.ChangeID == "" || record.Repository == "" || record.BaseRef == "" || record.Branch == "" || record.HeadSHA == "" || record.PolicyID == "" || record.SourceAttemptID == "" || record.RecorderAttemptID == "" || record.External.Provider == "" || record.External.ID == "" || record.External.URL == "" {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := coreImportReceiptReplay(ctx, s, receipt)
		if err != nil || replayed {
			return err
		}
		_, err = s.db.Exec(ctx, `INSERT INTO native_pr_publications (organization_id,version_id,change_id,repository,base_ref,branch,head_sha,policy_id,source_attempt_id,recorder_attempt_id,provider,external_id,external_url)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, record.OrganizationID, record.VersionID, record.ChangeID, record.Repository, record.BaseRef, record.Branch, record.HeadSHA, record.PolicyID, record.SourceAttemptID, record.RecorderAttemptID, record.External.Provider, record.External.ID, record.External.URL)
		if err != nil {
			return mapError(ctx, err)
		}
		_, err = s.PutReceipt(ctx, receipt)
		return err
	})
}

func (r *Repository) ImportCoreReleaseDelivery(ctx context.Context, record eventstore.ReleaseDelivery, receipt eventstore.Receipt) error {
	if err := validateCoreImportReceipt(record.OrganizationID, receipt, "core-release-delivery", string(record.WorkItemID)); err != nil {
		return err
	}
	delivery := record.ReleaseDelivery
	if record.ProjectID == "" || record.WorkItemID == "" || delivery.SourceIssueID == "" || delivery.SourceChangeID == "" || delivery.SourceMergeSHA == "" || delivery.Repository == "" || delivery.PRNumber < 1 || delivery.HeadRef == "" || delivery.HeadSHA == "" || delivery.BaseRef == "" || delivery.BaseSHA == "" || delivery.FrozenSHA == "" || delivery.ChangeID == "" || delivery.Actor.PrincipalID == "" || delivery.Actor.Kind == "" || delivery.AssociatedAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		replayed, err := coreImportReceiptReplay(ctx, s, receipt)
		if err != nil || replayed {
			return err
		}
		var version any
		if delivery.VersionID != "" {
			version = delivery.VersionID
		}
		_, err = s.db.Exec(ctx, `INSERT INTO release_deliveries (organization_id,project_id,work_item_id,source_issue_id,source_change_id,source_merge_sha,repository_id,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,version_id,principal_id,actor_kind,via_key,associated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`, record.OrganizationID, record.ProjectID, record.WorkItemID, delivery.SourceIssueID, delivery.SourceChangeID, delivery.SourceMergeSHA, record.RepositoryID, delivery.Repository, delivery.PRNumber, delivery.HeadRef, delivery.HeadSHA, delivery.BaseRef, delivery.BaseSHA, delivery.FrozenSHA, delivery.ChangeID, version, delivery.Actor.PrincipalID, delivery.Actor.Kind, delivery.Actor.ViaKey, delivery.AssociatedAt)
		if err != nil {
			return mapError(ctx, err)
		}
		_, err = s.PutReceipt(ctx, receipt)
		return err
	})
}

func validateCoreImportReceipt(organization tracker.OrganizationID, receipt eventstore.Receipt, operation, key string) error {
	if organization == "" || receipt.OrganizationID != organization || receipt.ActorID != "production-import" || receipt.Operation != operation || receipt.CommandKey != key || key == "" {
		return eventstore.ErrInvalidRecord
	}
	return nil
}

func coreImportReceiptReplay(ctx context.Context, s *session, receipt eventstore.Receipt) (bool, error) {
	previous, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if previous.RequestHash != receipt.RequestHash {
		return false, corestore.ErrRevisionConflict
	}
	return true, nil
}

func (r *Repository) CoreNativePRPublication(ctx context.Context, organization tracker.OrganizationID, versionID string) (eventstore.NativePRPublication, error) {
	var record eventstore.NativePRPublication
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		if err := s.checkOrganization(organization); err != nil {
			return err
		}
		err := s.db.QueryRow(ctx, `SELECT organization_id,version_id,change_id,repository,base_ref,branch,head_sha,policy_id,source_attempt_id,recorder_attempt_id,provider,external_id,external_url FROM native_pr_publications WHERE organization_id=$1 AND version_id=$2`, organization, versionID).Scan(&record.OrganizationID, &record.VersionID, &record.ChangeID, &record.Repository, &record.BaseRef, &record.Branch, &record.HeadSHA, &record.PolicyID, &record.SourceAttemptID, &record.RecorderAttemptID, &record.External.Provider, &record.External.ID, &record.External.URL)
		return mapError(ctx, err)
	})
	return record, err
}

func (r *Repository) CoreReleaseDelivery(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.ReleaseDelivery, error) {
	var record eventstore.ReleaseDelivery
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		if err := s.checkOrganization(organization); err != nil {
			return err
		}
		var repositoryID pgtype.Int8
		var versionID pgtype.Text
		delivery := &record.ReleaseDelivery
		err := s.db.QueryRow(ctx, `SELECT organization_id,project_id,work_item_id,repository_id,source_issue_id,source_change_id,source_merge_sha,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,version_id,principal_id,actor_kind,via_key,associated_at FROM release_deliveries WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, organization, project, item).Scan(&record.OrganizationID, &record.ProjectID, &record.WorkItemID, &repositoryID, &delivery.SourceIssueID, &delivery.SourceChangeID, &delivery.SourceMergeSHA, &delivery.Repository, &delivery.PRNumber, &delivery.HeadRef, &delivery.HeadSHA, &delivery.BaseRef, &delivery.BaseSHA, &delivery.FrozenSHA, &delivery.ChangeID, &versionID, &delivery.Actor.PrincipalID, &delivery.Actor.Kind, &delivery.Actor.ViaKey, &delivery.AssociatedAt)
		if err != nil {
			return mapError(ctx, err)
		}
		if repositoryID.Valid {
			record.RepositoryID = &repositoryID.Int64
		}
		delivery.VersionID = versionID.String
		return nil
	})
	return record, err
}

func (r *Repository) FinalizeCoreChangeImport(ctx context.Context, record eventstore.Change, receipt eventstore.Receipt) error {
	if record.OrganizationID == "" || record.ID == "" || record.ProjectID == "" || record.WorkItemID == "" || record.Revision < 1 || receipt.OrganizationID != record.OrganizationID || receipt.ActorID != "production-import" || receipt.Operation != "core-change" || receipt.CommandKey != record.ID {
		return eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return err
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		storedReceipt, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err != nil {
			return err
		}
		if storedReceipt.RequestHash != receipt.RequestHash {
			return mutation.ErrConflict
		}
		current, err := s.LockChange(ctx, record.ID)
		if err != nil {
			return err
		}
		if current.OrganizationID != record.OrganizationID || current.ProjectID != record.ProjectID || current.WorkItemID != record.WorkItemID || current.Revision != record.Revision || current.Title != record.Title {
			return mutation.ErrConflict
		}
		if sameCoreChangeCurrent(current, record) {
			return nil
		}
		if current.CurrentVersionID != nil || record.CurrentVersionID == nil {
			return mutation.ErrConflict
		}
		if _, err := s.Version(ctx, record.ID, *record.CurrentVersionID); err != nil {
			return err
		}
		result, err := s.db.Exec(ctx, `UPDATE change_requests SET current_version_id=$1,payload_object_key=$2,payload_sha256=$3,payload_bytes=$4,payload_content_bytes=$5,updated_at=$6,landed_version_id=$7,landed_at=$8,landed_head_sha=$9,landed_merge_sha=$10,landed_base_ref=$11
WHERE organization_id=$12 AND project_id=$13 AND id=$14 AND work_item_id=$15 AND revision=$16 AND current_version_id IS NULL`, record.CurrentVersionID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes, recordTime(record.UpdatedAt), record.LandedVersionID, nullableCoreImportTime(record.LandedAt), record.LandedHeadSHA, record.LandedMergeSHA, record.LandedBaseRef, record.OrganizationID, record.ProjectID, record.ID, record.WorkItemID, record.Revision)
		if err != nil {
			return mapError(ctx, err)
		}
		changed := result.RowsAffected()
		if changed != 1 {
			return mutation.ErrConflict
		}
		current, err = s.Change(ctx, record.ID)
		if err != nil {
			return err
		}
		if !sameCoreChangeCurrent(current, record) {
			return mutation.ErrConflict
		}
		return nil
	})
}

func sameCoreChangeCurrent(current, expected eventstore.Change) bool {
	if current.CurrentVersionID == nil || expected.CurrentVersionID == nil {
		return current.CurrentVersionID == nil && expected.CurrentVersionID == nil && current.PayloadObjectKey == expected.PayloadObjectKey && current.PayloadSHA256 == expected.PayloadSHA256 && current.PayloadBytes == expected.PayloadBytes && current.PayloadContentBytes == expected.PayloadContentBytes && reflect.DeepEqual(current.LandedVersionID, expected.LandedVersionID) && sameCoreImportOptionalTime(current.LandedAt, expected.LandedAt) && reflect.DeepEqual(current.LandedHeadSHA, expected.LandedHeadSHA) && reflect.DeepEqual(current.LandedMergeSHA, expected.LandedMergeSHA) && reflect.DeepEqual(current.LandedBaseRef, expected.LandedBaseRef)
	}
	return *current.CurrentVersionID == *expected.CurrentVersionID && current.PayloadObjectKey == expected.PayloadObjectKey && current.PayloadSHA256 == expected.PayloadSHA256 && current.PayloadBytes == expected.PayloadBytes && current.PayloadContentBytes == expected.PayloadContentBytes && reflect.DeepEqual(current.LandedVersionID, expected.LandedVersionID) && sameCoreImportOptionalTime(current.LandedAt, expected.LandedAt) && reflect.DeepEqual(current.LandedHeadSHA, expected.LandedHeadSHA) && reflect.DeepEqual(current.LandedMergeSHA, expected.LandedMergeSHA) && reflect.DeepEqual(current.LandedBaseRef, expected.LandedBaseRef)
}
