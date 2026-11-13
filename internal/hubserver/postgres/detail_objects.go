package postgres

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type DetailStore struct {
	repository *Repository
	now        func() time.Time
}

func (r *Repository) DetailStore(now func() time.Time) *DetailStore {
	return &DetailStore{repository: r, now: now}
}

func (s *session) putDetailObject(ctx context.Context, ref attachment.ArchiveReference, compressed []byte, now time.Time) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO hub_detail_objects(organization_id,object_key,sha256,compressed_bytes,content_bytes,content,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,object_key) DO NOTHING`, s.organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, compressed, now)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := s.detailObject(ctx, ref.Key)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, compressed) {
		return artifact.ErrIntegrity
	}
	return nil
}

func (s *session) detailObject(ctx context.Context, key string) ([]byte, error) {
	var content []byte
	err := s.db.QueryRow(ctx, `SELECT content FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2`, s.organization, key).Scan(&content)
	if err != nil {
		err = mapError(ctx, err)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, artifact.ErrMissing
		}
		return nil, err
	}
	return content, nil
}

func (d *DetailStore) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	err = d.repository.leafTransaction(ctx, tracker.OrganizationID(organization), false, func(s *session) error {
		return s.putDetailObject(ctx, ref, compressed, d.now().UTC())
	})
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	return ref, nil
}

func (d *DetailStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	compressed, err := d.read(ctx, organization, ref.Key)
	if err != nil {
		return nil, err
	}
	return attachment.DecodeArchive(organization, ref, compressed)
}

func (d *DetailStore) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	compressed, err := d.read(ctx, organization, key)
	if err != nil {
		return nil, errors.Join(artifact.ErrIntegrity, err)
	}
	return attachment.DecodeCanonicalArchive(organization, project, key, compressed)
}

func (d *DetailStore) read(ctx context.Context, organization, key string) ([]byte, error) {
	var compressed []byte
	err := d.repository.leafTransaction(ctx, tracker.OrganizationID(organization), true, func(s *session) error {
		var err error
		compressed, err = s.detailObject(ctx, key)
		return err
	})
	return compressed, err
}
