package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/readableurl"
)

func (s *session) AccessOrganizationSlug(ctx context.Context, slug string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE organization_id=$1 AND slug=$2) OR EXISTS(SELECT 1 FROM organization_slugs WHERE organization_id=$1 AND slug=$2)`, s.organization, slug).Scan(&found)
	return found, mapError(ctx, err)
}

func (s *session) AddAccessOrganizationSlug(ctx context.Context, slug string) error {
	if err := readableurl.Validate(slug); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO organization_slugs (organization_id,slug) VALUES ($1,$2) ON CONFLICT DO NOTHING`, s.organization, slug)
	return mapError(ctx, err)
}

func (s *session) SetAccessOrganizationSlug(ctx context.Context, expected *string, slug string) error {
	if err := readableurl.Validate(slug); err != nil {
		return err
	}
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT slug FROM organizations WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&current); err != nil {
		return mapError(ctx, err)
	}
	if expected != nil && current != *expected {
		return corestore.ErrRevisionConflict
	}
	if current == slug {
		return nil
	}
	if current != "" {
		if err := s.AddAccessOrganizationSlug(ctx, current); err != nil {
			return err
		}
	}
	if err := s.AddAccessOrganizationSlug(ctx, slug); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE organizations SET slug=$2 WHERE organization_id=$1`, s.organization, slug)
	return mapError(ctx, err)
}
