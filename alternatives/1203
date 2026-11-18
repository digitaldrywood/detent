package changecreation

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const maxNativeChangeTitle = 512

type Authority interface {
	Changes(context.Context, tracker.NativeWorkItemID) ([]tracker.ChangeRequest, error)
	CreateChange(context.Context, tracker.NativeWorkItemID, tracker.CreateChange) (tracker.ChangeRequest, error)
}
type Input struct {
	Item      tracker.NativeWorkItemID
	AttemptID string
	Title     string
	Lease     tracker.NativeLease
	Diff      tracker.AttemptDiffRequest
}

func Open(ctx context.Context, client Authority, input Input) (string, error) {
	item, diff := input.Item, input.Diff
	existing, err := client.Changes(ctx, item)
	if err != nil {
		return "", fmt.Errorf("list changes: %w", err)
	}
	if len(existing) > 0 {
		return existing[len(existing)-1].ID, nil
	}
	if err := api.MutationEffectError(ctx); err != nil {
		return "", err
	}
	created, err := client.CreateChange(ctx, item, tracker.CreateChange{
		Mutation: tracker.Mutation{IdempotencyKey: input.AttemptID + ":change", LeaseID: input.Lease.ID, FencingToken: input.Lease.FencingToken},
		Title:    nativeChangeTitle(input.Title, item),
		Body:     nativeChangeBody(input.AttemptID, diff),
	})
	if err != nil {
		return "", fmt.Errorf("create change: %w", err)
	}
	return created.ID, nil
}

func nativeChangeTitle(title string, item tracker.NativeWorkItemID) string {
	if title == "" {
		title = string(item)
	}
	if len(title) <= maxNativeChangeTitle {
		return title
	}
	cut := maxNativeChangeTitle
	for cut > 0 && title[cut]&0xC0 == 0x80 {
		cut--
	}
	return title[:cut]
}

func nativeChangeBody(attempt string, diff tracker.AttemptDiffRequest) string {
	return fmt.Sprintf("Opened by the runner when attempt %s succeeded.\n\nBase: %s\nHead: %s\nFiles changed: %d\n\nThe change is the attempt's stored diff.",
		attempt, diff.BaseSHA, diff.HeadSHA, len(diff.Files))
}
