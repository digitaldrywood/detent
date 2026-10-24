package cli

import (
	"context"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func executeNativeChangeCommand(ctx context.Context, client *hubclient.NativeClient, action string, args []string, input io.Reader) (any, error) {
	var item tracker.NativeWorkItemID
	if len(args) > 0 {
		item = tracker.NativeWorkItemID(args[0])
	}
	switch action {
	case "changes":
		return client.Changes(ctx, item)
	case "change":
		return client.Change(ctx, item, args[1])
	case "create-change":
		return nativeIssueInput(input, func(request tracker.CreateChange) (any, error) { return client.CreateChange(ctx, item, request) })
	case "publish-change":
		return nativeIssueInput(input, func(request tracker.PublishChangeVersion) (any, error) {
			return client.PublishChangeVersion(ctx, item, args[1], request)
		})
	case "review-change":
		return nativeIssueInput(input, func(request tracker.ReviewChange) (any, error) {
			return client.ReviewChange(ctx, item, args[1], args[2], request)
		})
	case "check-change":
		return nativeIssueInput(input, func(request tracker.SubmitChangeCheck) (any, error) {
			return client.SubmitChangeCheck(ctx, item, args[1], args[2], request)
		})
	case "discuss-change":
		return nativeIssueInput(input, func(request tracker.DiscussChange) (any, error) {
			return client.DiscussChange(ctx, item, args[1], request)
		})
	case "approve-review-policy":
		return nativeIssueInput(input, func(request tracker.ApproveChangeReviewPolicy) (any, error) {
			return client.ApproveChangeReviewPolicy(ctx, request)
		})
	default:
		return nil, errors.New("unsupported native issue action")
	}
}
