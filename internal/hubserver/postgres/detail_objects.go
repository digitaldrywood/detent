package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

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
	repository   *Repository
	now          func() time.Time
	mu           sync.Mutex
	reclaimAfter string
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

func (s *session) reclaimDetailObjects(ctx context.Context, cutoff time.Time, after string, limit int) (string, int, error) {
	var keys []string
	err := s.db.QueryRow(ctx, `SELECT ARRAY(SELECT object_key FROM hub_detail_objects WHERE organization_id=$1 AND object_key>$2 AND created_at<$3 ORDER BY object_key LIMIT $4 FOR UPDATE)`, s.organization, after, cutoff, limit).Scan(&keys)
	if err != nil || len(keys) == 0 {
		return "", 0, mapError(ctx, err)
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM hub_detail_objects WHERE organization_id=$1 AND created_at<$3 AND object_key IN (
 SELECT c.key FROM unnest($2::text[]) AS c(key) WHERE NOT (`+attachmentReferenced+`))`, s.organization, keys, cutoff)
	if err != nil {
		return "", 0, mapError(ctx, err)
	}
	reclaimed := int(tag.RowsAffected())
	if len(keys) < limit {
		return "", reclaimed, nil
	}
	return keys[len(keys)-1], reclaimed, nil
}

func (d *DetailStore) Reclaim(ctx context.Context, organization tracker.OrganizationID, cutoff time.Time) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reclaimed := 0
	for range DetailReclaimPages {
		var next string
		var count int
		err := d.repository.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
			var err error
			next, count, err = s.reclaimDetailObjects(ctx, cutoff, d.reclaimAfter, DetailReclaimPage)
			return err
		})
		if err != nil {
			return reclaimed, err
		}
		reclaimed, d.reclaimAfter = reclaimed+count, next
		if next == "" {
			break
		}
	}
	return reclaimed, nil
}

const detailObjectQuery = `SELECT content FROM public.hub_detail_objects WHERE organization_id=$1 AND object_key=$2`

func scanDetailObject(ctx context.Context, row pgx.Row) ([]byte, error) {
	var content []byte
	err := row.Scan(&content)
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
	if _, nested := ctx.Value(readRequestKey{}).(*readRequest); nested {
		err := d.repository.leafTransaction(ctx, tracker.OrganizationID(organization), true, func(s *session) error {
			var err error
			compressed, err = scanDetailObject(ctx, s.db.QueryRow(ctx, detailObjectQuery, s.organization, key))
			return err
		})
		return compressed, err
	}

	batch := &pgx.Batch{}
	batch.Queue(`SELECT pg_catalog.set_config('detent.organization_id', $1, true)`, organization)
	batch.Queue(detailObjectQuery, organization, key).QueryRow(func(row pgx.Row) error {
		var err error
		compressed, err = scanDetailObject(ctx, row)
		return err
	})
	started := time.Now()
	err := d.repository.runtime.reader.SendBatch(ctx, batch).Close()
	observeStatement(ctx, started, detailObjectQuery)
	return compressed, mapError(ctx, err)
}
