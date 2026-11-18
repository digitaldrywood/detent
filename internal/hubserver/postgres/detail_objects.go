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

const (
	DetailReclaimPage  = 100
	DetailReclaimPages = 4
)

type DetailStore struct {
	repository *Repository
	now        func() time.Time
}

func (r *Repository) DetailStore(now func() time.Time) *DetailStore {
	return &DetailStore{repository: r, now: now}
}

func (s *session) putDetailObject(ctx context.Context, ref attachment.ArchiveReference, compressed []byte, now time.Time) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO hub_detail_objects AS stored(organization_id,object_key,sha256,compressed_bytes,content_bytes,content,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,object_key) DO UPDATE SET created_at=GREATEST(stored.created_at,excluded.created_at)
WHERE stored.sha256=excluded.sha256 AND stored.content=excluded.content`, s.organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, compressed, now)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return artifact.ErrIntegrity
	}
	return nil
}

func (s *session) reclaimDetailObjects(ctx context.Context, cutoff time.Time) (int, int, error) {
	var keys []string
	err := s.db.QueryRow(ctx, `SELECT ARRAY(SELECT object_key FROM hub_detail_objects
WHERE organization_id=$1 AND reclaim_pending AND created_at<$2 ORDER BY created_at,object_key
LIMIT $3 FOR UPDATE SKIP LOCKED)`, s.organization, cutoff, DetailReclaimPage).Scan(&keys)
	if err != nil || len(keys) == 0 {
		return 0, 0, mapError(ctx, err)
	}
	var reclaimed int
	err = s.db.QueryRow(ctx, `WITH live AS MATERIALIZED (
 SELECT DISTINCT object_key FROM attachment_object_references WHERE organization_id=$1 AND object_key=ANY($2::text[])
), kept AS (
 UPDATE hub_detail_objects SET reclaim_pending=false WHERE organization_id=$1 AND object_key IN (SELECT object_key FROM live)
), removed AS (
 DELETE FROM hub_detail_objects WHERE organization_id=$1 AND object_key=ANY($2) AND created_at<$3
 AND NOT EXISTS (SELECT 1 FROM live WHERE live.object_key=hub_detail_objects.object_key) RETURNING 1
) SELECT count(*) FROM removed`, s.organization, keys, cutoff).Scan(&reclaimed)
	return len(keys), reclaimed, mapError(ctx, err)
}

func (d *DetailStore) Reclaim(ctx context.Context, organization tracker.OrganizationID, cutoff time.Time) (int, error) {
	reclaimed := 0
	for range DetailReclaimPages {
		var scanned, count int
		err := d.repository.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			var err error
			scanned, count, err = s.reclaimDetailObjects(ctx, cutoff)
			return err
		})
		if err != nil {
			return reclaimed, err
		}
		reclaimed += count
		if scanned < DetailReclaimPage {
			break
		}
	}
	return reclaimed, nil
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
