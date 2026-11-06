package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) RuntimePublished(ctx context.Context, id string) (bool, error) {
	value, err := s.queries.RuntimePublished(ctx, pgquery.RuntimePublishedParams{OrganizationID: s.organization, RevisionID: id})
	return value.Bool, mapError(ctx, err)
}

func (s *session) CurrentRuntime(ctx context.Context) (billingstore.RuntimeRevision, error) {
	row, err := s.queries.CurrentRuntime(ctx, s.organization)
	return billingstore.RuntimeRevision{ID: row.ID, Published: row.LocalPublished, Reference: billingstore.AuditReference{BindingID: row.BindingID, CustomerOwned: row.CustomerOwned, Key: row.ObjectKey, SHA256: row.Sha256, CompressedBytes: row.CompressedBytes, ContentBytes: row.ContentBytes, Codec: row.Codec, FormatVersion: int(row.FormatVersion), Kind: row.Kind, Retention: row.Retention}}, mapError(ctx, err)
}

func (s *session) StageRuntime(ctx context.Context, revision billingstore.RuntimeRevision) error {
	head, err := s.queries.LockRuntimeHead(ctx, s.organization)
	if err != nil && !errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return mapError(ctx, err)
	}
	active := head.ActiveID
	if err != nil && revision.Published {
		active = new(revision.ID)
	} else if err == nil {
		published, err := s.RuntimePublished(ctx, head.PreparedID)
		if err != nil {
			return err
		}
		if published {
			active = new(head.PreparedID)
		}
	}
	ref := revision.Reference
	if err := s.queries.InsertRuntimeRevision(ctx, pgquery.InsertRuntimeRevisionParams{OrganizationID: s.organization, ID: revision.ID, LocalPublished: revision.Published, BindingID: ref.BindingID, CustomerOwned: ref.CustomerOwned, ObjectKey: ref.Key, Sha256: ref.SHA256, CompressedBytes: ref.CompressedBytes, ContentBytes: ref.ContentBytes, Codec: ref.Codec, FormatVersion: int32(ref.FormatVersion), Kind: ref.Kind, Retention: ref.Retention}); err != nil {
		return mapError(ctx, err)
	}
	return mapError(ctx, s.queries.SetRuntimeHead(ctx, pgquery.SetRuntimeHeadParams{OrganizationID: s.organization, ActiveID: active, PreparedID: revision.ID}))
}
