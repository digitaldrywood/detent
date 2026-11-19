package changeapi

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ColumnCursor struct {
	Review         string
	Checkpoint     string
	CheckpointTurn bool
}

type ColumnDeps struct {
	Repository               tenantstore.Repository
	Organization             func() tracker.OrganizationID
	RunDetailStore           func() attachment.DetailStore
	PopulateCheckpointSource func(context.Context, tracker.OrganizationID, string) (string, error)
}

func PopulateChangeColumns(ctx context.Context, cursor *ColumnCursor, deps ColumnDeps) error {
	checkpointTurn := cursor.CheckpointTurn
	cursor.CheckpointTurn = !cursor.CheckpointTurn
	if checkpointTurn {
		next, err := deps.PopulateCheckpointSource(ctx, deps.Organization(), cursor.Checkpoint)
		if next != "" || err == nil {
			cursor.Checkpoint = next
		}
		return err
	}
	owner, ok := deps.Repository.(interface {
		BackfillCurrentChangeReviewsAfter(context.Context, tracker.OrganizationID, *eventstore.Content, int, string) (int, string, error)
	})
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	_, next, err := owner.BackfillCurrentChangeReviewsAfter(ctx, deps.Organization(), eventstore.NewContent(deps.RunDetailStore()), corestore.MaxPageSize, cursor.Review)
	if next != "" || err == nil {
		cursor.Review = next
	}
	return err
}
