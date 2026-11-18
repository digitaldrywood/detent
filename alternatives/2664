package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) ArchiveObject(ctx context.Context, id string) (artifactstore.ArchiveObject, error) {
	if id == "" {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	var object artifactstore.ArchiveObject
	var ref attachment.ArchiveReference
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes FROM archive_objects WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(
		&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt, &object.CollaborationBytes)
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	if err := ref.Validate(string(s.organization)); err != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	object.Reference = ref
	return object, nil
}

func (s *session) ArchiveObjectByKey(ctx context.Context, key string) (artifactstore.ArchiveObject, error) {
	digest := path.Base(key)
	expected, err := attachment.ArchiveKey(string(s.organization), digest)
	if err != nil || expected != key {
		return artifactstore.ArchiveObject{}, artifact.ErrIntegrity
	}
	var object artifactstore.ArchiveObject
	var ref attachment.ArchiveReference
	err = s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, key, digest).Scan(
		&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt, &object.CollaborationBytes)
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	if ref.Validate(string(s.organization)) != nil {
		return artifactstore.ArchiveObject{}, artifact.ErrIntegrity
	}
	object.Reference = ref
	return object, nil
}

func (s *session) ArchiveObjectByReference(ctx context.Context, ref attachment.ArchiveReference) (artifactstore.ArchiveObject, error) {
	if ref.Validate(string(s.organization)) != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	var object artifactstore.ArchiveObject
	var stored attachment.ArchiveReference
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, ref.Key, ref.SHA256).Scan(
		&object.OrganizationID, &object.ID, &stored.Key, &stored.SHA256, &stored.CompressedSize, &stored.UncompressedSize, &stored.Codec, &stored.SchemaVersion, &stored.Kind, &stored.Retention, &stored.BindingID, &stored.CustomerOwned, &object.CreatedAt, &object.CollaborationBytes)
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	if stored != ref {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	object.Reference = stored
	return object, nil
}

func (s *session) ArchiveBytes(ctx context.Context) (int64, error) {
	if !s.readOnly {
		if _, err := s.db.Exec(ctx, `INSERT INTO archive_usage_counters(organization_id,compressed_bytes) VALUES($1,0) ON CONFLICT (organization_id) DO NOTHING`, s.organization); err != nil {
			return 0, mapError(ctx, err)
		}
		var bytes int64
		err := s.db.QueryRow(ctx, `SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&bytes)
		if err == nil {
			s.archiveCounterLocked = true
		}
		return bytes, mapError(ctx, err)
	}
	var bytes int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE((SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1),0)`, s.organization).Scan(&bytes)
	return bytes, mapError(ctx, err)
}

func (s *session) RegisterArchiveObject(ctx context.Context, object artifactstore.ArchiveObject) (artifactstore.ArchiveObject, error) {
	if err := s.checkOrganization(object.OrganizationID); err != nil {
		return artifactstore.ArchiveObject{}, err
	}
	if object.ID == "" || object.CollaborationBytes != 0 || object.CreatedAt.IsZero() || object.Reference.Validate(string(s.organization)) != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	if !s.archiveCounterLocked {
		if _, err := s.ArchiveBytes(ctx); err != nil {
			return artifactstore.ArchiveObject{}, err
		}
	}
	ref := object.Reference
	var stored artifactstore.ArchiveObject
	var storedRef attachment.ArchiveReference
	scan := func(row pgx.Row) error {
		return row.Scan(&stored.OrganizationID, &stored.ID, &storedRef.Key, &storedRef.SHA256, &storedRef.CompressedSize, &storedRef.UncompressedSize, &storedRef.Codec, &storedRef.SchemaVersion, &storedRef.Kind, &storedRef.Retention, &storedRef.BindingID, &storedRef.CustomerOwned, &stored.CreatedAt, &stored.CollaborationBytes)
	}
	err := scan(s.db.QueryRow(ctx, `INSERT INTO archive_objects (organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (organization_id,object_key,sha256) DO NOTHING RETURNING organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes`, object.OrganizationID, object.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, object.CreatedAt))
	inserted := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = scan(s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, ref.Key, ref.SHA256))
	}
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	if storedRef != ref {
		return artifactstore.ArchiveObject{}, corestore.ErrUniqueViolation
	}
	stored.Reference = storedRef
	s.insertedArchive = nil
	if inserted {
		s.insertedArchive = new(stored)
	}
	return stored, nil
}

func (s *session) AddArchiveOwner(ctx context.Context, owner artifactstore.ArchiveOwner) error {
	if err := s.validateArchiveOwner(owner); err != nil {
		return err
	}
	if !s.archiveCounterLocked {
		if _, err := s.ArchiveBytes(ctx); err != nil {
			return err
		}
	}
	var size int64
	var customerOwned bool
	err := s.db.QueryRow(ctx, `SELECT compressed_size,customer_owned FROM archive_objects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, owner.ArchiveID).Scan(&size, &customerOwned)
	if err != nil {
		return mapError(ctx, err)
	}
	var alreadyOwned bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2)`, s.organization, owner.ArchiveID).Scan(&alreadyOwned); err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `INSERT INTO archive_object_owners(organization_id,archive_id,owner_kind,owner_id,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, owner.OrganizationID, owner.ArchiveID, owner.Kind, owner.ID, owner.CreatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	inserted := result.RowsAffected()
	if inserted == 1 && !alreadyOwned && !customerOwned {
		_, err = s.db.Exec(ctx, `UPDATE archive_usage_counters SET compressed_bytes=compressed_bytes+$1 WHERE organization_id=$2`, size, s.organization)
	}
	return mapError(ctx, err)
}

func (s *session) RemoveArchiveOwner(ctx context.Context, owner artifactstore.ArchiveOwner) error {
	if err := s.checkOrganization(owner.OrganizationID); err != nil {
		return err
	}
	if owner.ArchiveID == "" || owner.Kind == "" || owner.ID == "" {
		return corestore.ErrInvalidPage
	}
	if err := s.LockArchiveAccounting(ctx); err != nil {
		return err
	}
	object, err := s.lockArchiveObject(ctx, owner.ArchiveID)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `DELETE FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind=$3 AND owner_id=$4`, owner.OrganizationID, owner.ArchiveID, owner.Kind, owner.ID)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	if err := s.recognizeArchiveDebit(ctx, &object, owner.Kind == "attempt-diff"); err != nil {
		return err
	}
	var stillOwned bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2)`, s.organization, owner.ArchiveID).Scan(&stillOwned); err != nil {
		return mapError(ctx, err)
	}
	if stillOwned {
		return nil
	}
	if !object.Reference.CustomerOwned {
		result, err = s.db.Exec(ctx, `UPDATE archive_usage_counters SET compressed_bytes=compressed_bytes-$1 WHERE organization_id=$2 AND compressed_bytes >= $1`, object.Reference.CompressedSize, s.organization)
		if err != nil {
			return mapError(ctx, err)
		}
		if result.RowsAffected() != 1 {
			return fmt.Errorf("archive byte counter underflow: %w", corestore.ErrInvalidPage)
		}
	}
	if object.CollaborationBytes != 0 {
		if !s.archiveBillingPresent {
			return corestore.ErrNotFound
		}
		if _, err := s.AdjustCollaborationBytes(ctx, -object.CollaborationBytes, math.MaxInt64); err != nil {
			return err
		}
	}
	contentID := pgConversationContentID(string(s.organization), object.Reference)
	if _, err := s.db.Exec(ctx, `DELETE FROM conversation_content_objects WHERE organization_id=$1 AND id=$2 AND archive_id=$3`, s.organization, contentID, owner.ArchiveID); err != nil {
		return mapError(ctx, err)
	}
	result, err = s.db.Exec(ctx, `DELETE FROM archive_objects WHERE organization_id=$1 AND id=$2 AND NOT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2)`, s.organization, owner.ArchiveID)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) lockOrganizationCounter(ctx context.Context) error {
	if s.archiveAccountingLocked || s.archiveCounterLocked {
		return nil
	}
	return s.LockArchiveAccounting(ctx)
}

func (s *session) LockArchiveAccounting(ctx context.Context) error {
	if s.readOnly {
		return corestore.ErrInvalidPage
	}
	if s.archiveAccountingLocked {
		return nil
	}
	if s.archiveCounterLocked {
		return fmt.Errorf("archive accounting lock order: %w", corestore.ErrInvalidPage)
	}
	_, err := s.AdjustCollaborationBytes(ctx, 0, math.MaxInt64)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	s.archiveBillingPresent = err == nil
	if _, err := s.ArchiveBytes(ctx); err != nil {
		return err
	}
	s.archiveAccountingLocked = true
	return nil
}

func (s *session) lockArchiveObject(ctx context.Context, id string) (artifactstore.ArchiveObject, error) {
	if inserted := s.insertedArchive; inserted != nil {
		s.insertedArchive = nil
		if inserted.ID == id {
			return *inserted, nil
		}
	}
	var object artifactstore.ArchiveObject
	var ref attachment.ArchiveReference
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at,collaboration_bytes FROM archive_objects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt, &object.CollaborationBytes)
	if err != nil {
		return object, mapError(ctx, err)
	}
	if ref.Validate(string(s.organization)) != nil || object.CollaborationBytes < 0 || object.CollaborationBytes != 0 && object.CollaborationBytes != ref.CompressedSize || ref.CustomerOwned && object.CollaborationBytes != 0 {
		return object, corestore.ErrInvalidPage
	}
	object.Reference = ref
	return object, nil
}

func (s *session) recognizeArchiveDebit(ctx context.Context, object *artifactstore.ArchiveObject, removedDiff bool) error {
	if object.CollaborationBytes != 0 || object.Reference.CustomerOwned {
		return nil
	}
	hasDiff := removedDiff
	if !hasDiff {
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind='attempt-diff')`, s.organization, object.ID).Scan(&hasDiff); err != nil {
			return mapError(ctx, err)
		}
	}
	if !hasDiff {
		return nil
	}
	identity := strings.TrimPrefix(object.ID, "archive_")
	_, err := hex.DecodeString(identity)
	if identity == object.ID || len(identity) != 32 || err != nil || identity[12] != '4' || !strings.ContainsRune("89ab", rune(identity[16])) {
		return nil
	}
	if !s.archiveBillingPresent {
		return corestore.ErrNotFound
	}
	used, err := s.CollaborationBytes(ctx)
	if err != nil {
		return err
	}
	if used < object.Reference.CompressedSize {
		return billingstore.ErrInvalidCollaborationBytes
	}
	result, err := s.db.Exec(ctx, `UPDATE archive_objects SET collaboration_bytes=compressed_size WHERE organization_id=$1 AND id=$2 AND collaboration_bytes=0`, s.organization, object.ID)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	object.CollaborationBytes = object.Reference.CompressedSize
	return nil
}

func (s *session) ChargeArchiveObject(ctx context.Context, id string, limit int64) (billingstore.UsageResult, error) {
	if id == "" || limit < 0 {
		return billingstore.UsageResult{}, corestore.ErrInvalidPage
	}
	if err := s.LockArchiveAccounting(ctx); err != nil {
		return billingstore.UsageResult{}, err
	}
	object, err := s.lockArchiveObject(ctx, id)
	if err != nil {
		return billingstore.UsageResult{}, err
	}
	if err := s.recognizeArchiveDebit(ctx, &object, false); err != nil {
		return billingstore.UsageResult{}, err
	}
	if object.Reference.CustomerOwned {
		return billingstore.UsageResult{Allowed: true}, nil
	}
	if !s.archiveBillingPresent {
		return billingstore.UsageResult{}, corestore.ErrNotFound
	}
	if object.CollaborationBytes != 0 {
		return s.AdjustCollaborationBytes(ctx, 0, limit)
	}
	usage, err := s.AdjustCollaborationBytes(ctx, object.Reference.CompressedSize, limit)
	if err != nil || !usage.Allowed {
		return usage, err
	}
	result, err := s.db.Exec(ctx, `UPDATE archive_objects SET collaboration_bytes=$3 WHERE organization_id=$1 AND id=$2 AND collaboration_bytes=0`, s.organization, id, object.Reference.CompressedSize)
	if err != nil {
		return billingstore.UsageResult{}, mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return billingstore.UsageResult{}, corestore.ErrRevisionConflict
	}
	return usage, nil
}

func (s *session) validateArchiveOwner(owner artifactstore.ArchiveOwner) error {
	if err := s.checkOrganization(owner.OrganizationID); err != nil {
		return err
	}
	if owner.ArchiveID == "" || owner.Kind == "" || owner.ID == "" || owner.CreatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	return nil
}

var _ artifactstore.Reader = (*session)(nil)
var _ artifactstore.Writer = (*session)(nil)
