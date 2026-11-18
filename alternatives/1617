package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeColumnCursor struct {
	Review         string
	Checkpoint     string
	CheckpointTurn bool
}

func (s *Service) populateChangeColumns(ctx context.Context) error {
	var cursor changeColumnCursor
	switch prior := s.changeColumnAfter.(type) {
	case changeColumnCursor:
		cursor = prior
	case string:
		cursor.Review = prior
	}
	checkpointTurn := cursor.CheckpointTurn
	cursor.CheckpointTurn = !cursor.CheckpointTurn
	defer func() { s.changeColumnAfter = cursor }()
	if checkpointTurn {
		next, err := s.populateCheckpointSource(ctx, tracker.OrganizationID(s.appOrganizationID()), cursor.Checkpoint)
		if next != "" || err == nil {
			cursor.Checkpoint = next
		}
		return err
	}
	owner, ok := s.config.TenantRepository.(interface {
		BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
	})
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	_, next, err := owner.BackfillCurrentChangeReviewsAfter(ctx, tracker.OrganizationID(s.appOrganizationID()), eventstore.NewContent(s.runDetailStore()), corestore.MaxPageSize, cursor.Review)
	if next != "" || err == nil {
		cursor.Review = next
	}
	return err
}
