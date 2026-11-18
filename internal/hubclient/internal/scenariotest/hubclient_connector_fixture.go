package scenariotest

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hubclientFixtureConnector struct {
	*NativeConnector
	admin              *NativeClient
	worker             *NativeConnector
	afterCurrentReview func(context.Context, string)
}

func (c *hubclientFixtureConnector) UpdateIssueState(ctx context.Context, id, state string) error {
	reason := strings.ReplaceAll(connector.LaneTransitionReasonDetail(ctx), " ", "_")
	if reason == "recorded_blocker_recovery" || reason == "dependency_auto_unblock" {
		return c.worker.SetIntakeIssueState(ctx, id, state)
	}
	mutation := tracker.MutationForContext(ctx, api.NativeMutationKey().IdempotencyKey)
	mutation = c.worker.NativeClient().FencedMutation(ctx, tracker.NativeWorkItemID(id), mutation)
	if mutation.LeaseID != "" {
		return c.worker.SetIntakeIssueState(ctx, id, state)
	}
	item, err := c.admin.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil || item.State == state {
		return err
	}
	_, err = c.admin.Transition(ctx, item.WorkItemID, tracker.Transition{Mutation: mutation, ExpectedRevision: item.Revision, State: state, Reason: "user_requested", ReasonDetail: connector.LaneTransitionReasonDetail(ctx)})
	return err
}

func (c *hubclientFixtureConnector) ReadChangeReview(ctx context.Context, id, change, version, head string) (connector.ChangeReview, error) {
	review, err := c.NativeConnector.ReadChangeReview(ctx, id, change, version, head)
	if err == nil && review.Current && c.afterCurrentReview != nil {
		c.afterCurrentReview(ctx, id)
	}
	return review, err
}
