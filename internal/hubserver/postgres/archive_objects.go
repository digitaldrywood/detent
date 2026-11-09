package postgres

import (
	"context"
	"fmt"
	"path"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) ArchiveObject(ctx context.Context, id string) (artifactstore.ArchiveObject, error) {
	if id == "" {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	var object artifactstore.ArchiveObject
	var ref attachment.ArchiveReference
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM archive_objects WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(
		&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt)
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
	err = s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, key, digest).Scan(
		&object.OrganizationID, &object.ID, &ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize, &ref.Codec, &ref.SchemaVersion, &ref.Kind, &ref.Retention, &ref.BindingID, &ref.CustomerOwned, &object.CreatedAt)
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
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, ref.Key, ref.SHA256).Scan(
		&object.OrganizationID, &object.ID, &stored.Key, &stored.SHA256, &stored.CompressedSize, &stored.UncompressedSize, &stored.Codec, &stored.SchemaVersion, &stored.Kind, &stored.Retention, &stored.BindingID, &stored.CustomerOwned, &object.CreatedAt)
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
	if object.ID == "" || object.CreatedAt.IsZero() || object.Reference.Validate(string(s.organization)) != nil {
		return artifactstore.ArchiveObject{}, corestore.ErrInvalidPage
	}
	ref := object.Reference
	_, err := s.db.Exec(ctx, `INSERT INTO archive_objects (organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (organization_id,object_key,sha256) DO NOTHING`, object.OrganizationID, object.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention, ref.BindingID, ref.CustomerOwned, object.CreatedAt)
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	var stored artifactstore.ArchiveObject
	var storedRef attachment.ArchiveReference
	err = s.db.QueryRow(ctx, `SELECT organization_id,id,object_key,sha256,compressed_size,uncompressed_size,codec,schema_version,kind,retention,binding_id,customer_owned,created_at FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3`, s.organization, ref.Key, ref.SHA256).Scan(
		&stored.OrganizationID, &stored.ID, &storedRef.Key, &storedRef.SHA256, &storedRef.CompressedSize, &storedRef.UncompressedSize, &storedRef.Codec, &storedRef.SchemaVersion, &storedRef.Kind, &storedRef.Retention, &storedRef.BindingID, &storedRef.CustomerOwned, &stored.CreatedAt)
	if err != nil {
		return artifactstore.ArchiveObject{}, mapError(ctx, err)
	}
	if storedRef != ref {
		return artifactstore.ArchiveObject{}, corestore.ErrUniqueViolation
	}
	stored.Reference = storedRef
	return stored, nil
}

func (s *session) AddArchiveOwner(ctx context.Context, owner artifactstore.ArchiveOwner) error {
	if err := s.validateArchiveOwner(owner); err != nil {
		return err
	}
	if _, err := s.ArchiveBytes(ctx); err != nil {
		return err
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
	if _, err := s.ArchiveBytes(ctx); err != nil {
		return err
	}
	var size int64
	var customerOwned bool
	err := s.db.QueryRow(ctx, `SELECT compressed_size,customer_owned FROM archive_objects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, owner.ArchiveID).Scan(&size, &customerOwned)
	if err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `DELETE FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind=$3 AND owner_id=$4`, owner.OrganizationID, owner.ArchiveID, owner.Kind, owner.ID)
	if err != nil {
		return mapError(ctx, err)
	}
	removed := result.RowsAffected()
	if removed == 0 || customerOwned {
		return mapError(ctx, err)
	}
	var stillOwned bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2)`, s.organization, owner.ArchiveID).Scan(&stillOwned); err != nil {
		return mapError(ctx, err)
	}
	if stillOwned {
		return nil
	}
	result, err = s.db.Exec(ctx, `UPDATE archive_usage_counters SET compressed_bytes=compressed_bytes-$1 WHERE organization_id=$2 AND compressed_bytes >= $1`, size, s.organization)
	if err != nil {
		return mapError(ctx, err)
	}
	changed := result.RowsAffected()
	if changed != 1 {
		return fmt.Errorf("archive byte counter underflow: %w", corestore.ErrInvalidPage)
	}
	return nil
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
