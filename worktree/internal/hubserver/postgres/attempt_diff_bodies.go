package postgres

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AttemptDiffBodyReferenced(ctx context.Context, project tracker.ProjectID, key string) (bool, bool, error) {
	if project == "" || key == "" {
		return false, false, corestore.ErrInvalidPage
	}
	row, err := s.queries.AttemptDiffBodyReferenceCheck(ctx, pgquery.AttemptDiffBodyReferenceCheckParams{OrganizationID: s.organization, ProjectID: project, ObjectKey: key})
	return row.Referenced, row.Complete, mapError(ctx, err)
}

func (s *session) IndexAttemptDiffBodies(ctx context.Context, project tracker.ProjectID, diffID string, keys []string) error {
	if s.readOnly || project == "" || diffID == "" || len(keys) > tracker.MaxDiffFiles {
		return corestore.ErrInvalidPage
	}
	for _, key := range keys {
		parts := strings.Split(key, "/")
		if len(parts) != 7 {
			return corestore.ErrInvalidPage
		}
		ref := diffbody.Reference{Key: key, SHA256: parts[6], Bytes: 1}
		if ref.Validate(string(s.organization), string(project), 1) != nil {
			return corestore.ErrInvalidPage
		}
	}
	count, err := s.queries.AttemptDiffBodyReferencesComplete(ctx, pgquery.AttemptDiffBodyReferencesCompleteParams{OrganizationID: s.organization, ProjectID: project, DiffID: diffID})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	err = s.queries.AttemptDiffBodyReferencesInsert(ctx, pgquery.AttemptDiffBodyReferencesInsertParams{OrganizationID: s.organization, DiffID: diffID, ObjectKeys: keys})
	return mapError(ctx, err)
}
