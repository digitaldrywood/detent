package platformstore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

var ErrImport = errors.New("invalid platform import")

const ImportLimit = 100

type ImportObjects interface {
	Put(context.Context, string, io.ReadSeeker, int64, string, string) error
	Open(context.Context, string) (io.ReadCloser, error)
}

type ImportSource struct {
	DB             *sql.DB
	Catalog        storagecatalog.Catalog
	Registry       storagecatalog.Source
	Auth           storagecatalog.Source
	RegistrySchema storagecatalog.Schema
	AuthSchema     storagecatalog.Schema
	SnapshotID     string
	SigningKey     ed25519.PublicKey
	KeyVersions    []int
	Objects        ImportObjects
}

type ImportProgress struct {
	Processed int64
	Done      bool
}

type Importer struct {
	repository    *Repository
	source        ImportSource
	digest        string
	organizations map[string]bool
}

type importCheckpoint struct {
	stage, processed, revision int64
	after                      [3]string
	started                    bool
}

var importTables = [...]string{
	"registry/organizations", "registry/organization_slugs", "registry/organization_events", "registry/organization_intents",
	"registry/billing_customers", "registry/billing_events", "registry/entitlement_changes", "registry/platform_members",
	"registry/platform_member_changes", "registry/platform_notifications", "registry/archive_storage_bindings",
	"registry/archive_deletion_receipts", "registry/archive_deletion_failures",
	"auth/sessions", "auth/authorizations", "auth/transactions", "auth/access_keys", "auth/access_key_organizations",
	"auth/organization_key_policy", "auth/access_key_notifications", "auth/github_user_connections",
	"auth/oauth_clients", "auth/oauth_client_redirect_uris", "auth/oauth_grants", "auth/oauth_refresh_tokens",
	"auth/oauth_requests", "auth/oauth_spent_refresh_tokens", "auth/platform_notifications", "auth/audit",
}

func (r *Repository) NewImporter(ctx context.Context, source ImportSource) (*Importer, error) {
	if source.DB == nil || source.DB.Stats().MaxOpenConnections < 1 || source.Objects == nil || source.SnapshotID == "" || len(source.SnapshotID) > 200 || source.Registry.Store != "registry" || source.Auth.Store != "auth" || source.Registry.Schema == source.Auth.Schema || !bytes.Equal(source.SigningKey, r.secrets.signingKey) {
		return nil, ErrImport
	}
	encoded, err := json.Marshal(struct {
		Catalog                    storagecatalog.Catalog
		Registry, Auth             storagecatalog.Source
		RegistrySchema, AuthSchema storagecatalog.Schema
		SigningKey                 ed25519.PublicKey
		KeyVersions                []int
	}{source.Catalog, source.Registry, source.Auth, source.RegistrySchema, source.AuthSchema, source.SigningKey, source.KeyVersions})
	if err != nil {
		return nil, ErrImport
	}
	var frozen struct {
		Catalog                    storagecatalog.Catalog
		Registry, Auth             storagecatalog.Source
		RegistrySchema, AuthSchema storagecatalog.Schema
		SigningKey                 ed25519.PublicKey
		KeyVersions                []int
	}
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, ErrImport
	}
	source.Catalog, source.Registry, source.Auth = frozen.Catalog, frozen.Registry, frozen.Auth
	source.RegistrySchema, source.AuthSchema = frozen.RegistrySchema, frozen.AuthSchema
	source.SigningKey, source.KeyVersions = frozen.SigningKey, frozen.KeyVersions
	if _, err := source.Catalog.Bind(source.Registry, source.RegistrySchema); err != nil {
		return nil, err
	}
	if _, err := source.Catalog.Bind(source.Auth, source.AuthSchema); err != nil {
		return nil, err
	}
	organizations := map[string]bool{}
	for _, id := range source.Registry.Organizations {
		organizations[id] = true
	}
	if len(organizations) != len(source.Auth.Organizations) {
		return nil, storagecatalog.ErrOrganization
	}
	for _, id := range source.Auth.Organizations {
		if !organizations[id] {
			return nil, storagecatalog.ErrOrganization
		}
	}
	for n, version := range source.KeyVersions {
		if version < 1 || slices.Contains(source.KeyVersions[:n], version) || !slices.Contains(r.secrets.keys.Versions(), version) {
			return nil, ErrImport
		}
	}
	digest := sha256.Sum256(encoded)
	i := &Importer{repository: r, source: source, digest: hex.EncodeToString(digest[:]), organizations: organizations}
	if err := i.validateSource(ctx); err != nil {
		return nil, mapError(ctx, err)
	}
	return i, nil
}

func (i *Importer) checkpoint(ctx context.Context, tx *sql.Tx, lock bool) (importCheckpoint, error) {
	query := `SELECT snapshot_id,catalog_digest,stage,processed,revision,after_key_1,after_key_2,after_key_3,started FROM platform.import_checkpoint WHERE singleton`
	if lock {
		query += " FOR UPDATE"
	}
	var c importCheckpoint
	var snapshot, digest string
	err := tx.QueryRowContext(ctx, query).Scan(&snapshot, &digest, &c.stage, &c.processed, &c.revision, &c.after[0], &c.after[1], &c.after[2], &c.started)
	if err != nil {
		return c, err
	}
	if snapshot != i.source.SnapshotID || digest != i.digest || c.stage < 0 || c.stage > int64(len(importTables)) {
		return c, ErrImport
	}
	return c, nil
}

func (i *Importer) Step(ctx context.Context, limit int) (ImportProgress, error) {
	if limit < 1 || limit > ImportLimit {
		return ImportProgress{}, ErrImport
	}
	var checkpoint importCheckpoint
	err := i.repository.transaction(ctx, corestore.Background, false, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO platform.import_checkpoint(singleton,snapshot_id,catalog_digest) VALUES(true,$1,$2) ON CONFLICT DO NOTHING`, i.source.SnapshotID, i.digest)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted != 0 {
			if err := i.requireEmptyTarget(ctx, tx); err != nil {
				return err
			}
		}
		checkpoint, err = i.checkpoint(ctx, tx, false)
		return err
	})
	progress := ImportProgress{Processed: checkpoint.processed, Done: checkpoint.stage == int64(len(importTables))}
	if err != nil || progress.Done {
		return progress, err
	}
	page, err := i.readPage(ctx, checkpoint, limit)
	if err != nil {
		return progress, mapError(ctx, err)
	}
	err = i.repository.transaction(ctx, corestore.Background, false, func(tx *sql.Tx) error {
		current, err := i.checkpoint(ctx, tx, true)
		if err != nil {
			return err
		}
		if current.revision != checkpoint.revision {
			return corestore.ErrRevisionConflict
		}
		for _, row := range page {
			if err := i.writeRow(ctx, tx, row); err != nil {
				return err
			}
			checkpoint.after, checkpoint.started = row.keys, true
		}
		if len(page) < limit {
			checkpoint.stage++
			checkpoint.after, checkpoint.started = [3]string{}, false
		}
		_, err = tx.ExecContext(ctx, `UPDATE platform.import_checkpoint SET stage=$1,processed=processed+$2,revision=revision+1,after_key_1=$3,after_key_2=$4,after_key_3=$5,started=$6 WHERE singleton`, checkpoint.stage, len(page), checkpoint.after[0], checkpoint.after[1], checkpoint.after[2], checkpoint.started)
		return err
	})
	if err != nil {
		return progress, err
	}
	return ImportProgress{Processed: progress.Processed + int64(len(page)), Done: checkpoint.stage == int64(len(importTables))}, nil
}

func (i *Importer) requireEmptyTarget(ctx context.Context, tx *sql.Tx) error {
	for _, name := range append(importTargetTables(), "administration_receipts", "access_key_projects", "access_key_blocked_projects", "organization_slug_counters") {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM platform."+name+")").Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("platform import requires an empty target: %w", ErrImport)
		}
	}
	return nil
}
