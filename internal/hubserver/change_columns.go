package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/changeapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeColumnCursor = changeapi.ColumnCursor

func (s *Service) populateChangeColumns(ctx context.Context) error {
	var cursor changeColumnCursor
	switch prior := s.changeColumnAfter.(type) {
	case changeColumnCursor:
		cursor = prior
	case string:
		cursor.Review = prior
	}
	defer func() { s.changeColumnAfter = cursor }()
	return changeapi.PopulateChangeColumns(ctx, &cursor, changeapi.ColumnDeps{
		Repository:               s.config.TenantRepository,
		Organization:             func() tracker.OrganizationID { return tracker.OrganizationID(s.appOrganizationID()) },
		RunDetailStore:           s.runDetailStore,
		PopulateCheckpointSource: s.populateCheckpointSource,
	})
}
