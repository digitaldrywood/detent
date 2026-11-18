package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func (s *session) AttachmentObjectExists(ctx context.Context, id, key string) (bool, error) {
	var exists bool
	if key == "" {
		err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachments WHERE organization_id=$1 AND id=$2 AND object_deleted_at IS NULL)`, s.organization, id).Scan(&exists)
		return exists, mapError(ctx, err)
	}
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachment_object_references WHERE organization_id=$1 AND object_key=$2)`, s.organization, key).Scan(&exists)
	return exists, mapError(ctx, err)
}

func (s *session) ReferencedAttachmentObjects(ctx context.Context, keys []string) ([]bool, error) {
	if len(keys) == 0 || len(keys) > attachment.ReferenceBatchSize {
		return nil, attachment.ErrInvalid
	}
	var result []bool
	err := s.db.QueryRow(ctx, `WITH live AS MATERIALIZED (
 SELECT DISTINCT object_key FROM attachment_object_references WHERE organization_id=$1 AND object_key=ANY($2::text[])
) SELECT ARRAY(SELECT live.object_key IS NOT NULL FROM unnest($2::text[]) WITH ORDINALITY AS c(key,n)
 LEFT JOIN live ON live.object_key=c.key ORDER BY c.n)`, s.organization, keys).Scan(&result)
	return result, mapError(ctx, err)
}

func (s *session) ArchiveReclaimPage(ctx context.Context, now time.Time) (attachment.ReclaimPage, error) {
	var page attachment.ReclaimPage
	var keys []string
	var revisions []int64
	err := s.db.QueryRow(ctx, `SELECT h.archive_reclaim_after,c.keys,c.revisions FROM hosted_tenant h CROSS JOIN LATERAL (
 SELECT array_agg(object_key ORDER BY due_at,object_key) AS keys,array_agg(revision ORDER BY due_at,object_key) AS revisions FROM
 (SELECT object_key,revision,due_at FROM archive_reclaim_candidates WHERE organization_id=h.organization_id AND due_at<=$2 ORDER BY due_at,object_key LIMIT $3) due
) c WHERE h.organization_id=$1`, s.organization, now, attachment.ReferenceBatchSize).Scan(&page.After, &keys, &revisions)
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Changed = make([]attachment.ReclaimCandidate, 0, len(keys))
	for i, key := range keys {
		page.Changed = append(page.Changed, attachment.ReclaimCandidate{Key: key, Revision: revisions[i]})
	}
	return page, nil
}

func (s *session) CompleteArchiveReclaim(ctx context.Context, receipt attachment.ReclaimReceipt) error {
	if s.readOnly || !receipt.Valid(string(s.organization)) {
		return attachment.ErrInvalid
	}
	keys := make([]string, len(receipt.Checked))
	revisions := make([]int64, len(keys))
	due := make([]*time.Time, len(keys))
	for i, candidate := range receipt.Checked {
		keys[i], revisions[i], due[i] = candidate.Key, candidate.Revision, candidate.Due
	}
	_, err := s.db.Exec(ctx, `WITH checked AS MATERIALIZED (SELECT * FROM unnest($4::text[],$5::bigint[],$6::timestamptz[]) AS c(key,revision,due)),
 advanced AS (UPDATE hosted_tenant SET archive_reclaim_after=$3 WHERE organization_id=$1 AND archive_reclaim_after=$2 AND archive_reclaim_after<>$3),
 settled AS (DELETE FROM archive_reclaim_candidates q USING checked c WHERE q.organization_id=$1 AND q.object_key=c.key AND q.revision=c.revision AND c.due IS NULL)
 UPDATE archive_reclaim_candidates q SET due_at=c.due FROM checked c WHERE q.organization_id=$1 AND q.object_key=c.key AND q.revision=c.revision AND c.due IS NOT NULL`,
		s.organization, receipt.After, receipt.Next, keys, revisions, due)
	return mapError(ctx, err)
}
