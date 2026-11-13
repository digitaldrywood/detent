package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync"
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
	rows, err := s.db.Query(ctx, `SELECT object_key FROM hub_detail_objects WHERE organization_id=$1 AND object_key>$2 ORDER BY object_key LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return "", 0, mapError(ctx, err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return "", 0, mapError(ctx, err)
		}
		keys = append(keys, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", 0, mapError(ctx, err)
	}
	reclaimed := 0
	for _, key := range keys {
		referenced, err := s.AttachmentObjectExists(ctx, "", key)
		if err != nil || referenced {
			if err != nil {
				return "", reclaimed, err
			}
			continue
		}
		tag, err := s.db.Exec(ctx, `DELETE FROM hub_detail_objects WHERE organization_id=$1 AND object_key=$2 AND created_at<$3`, s.organization, key, cutoff)
		if err != nil {
			return "", reclaimed, mapError(ctx, err)
		}
		reclaimed += int(tag.RowsAffected())
	}
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
