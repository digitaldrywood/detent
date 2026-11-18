package cli

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type clihangNativeClient struct {
	*hubclient.NativeClient
	admin        *hubclient.NativeClient
	projectPages func()
}

func clihangRuntimeEvidence(ctx context.Context, client *clihangNativeClient, item tracker.NativeWorkItemID) (tracker.NativeRuntimeEvidence, error) {
	attempts, err := client.Attempts(ctx, item, "")
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	if len(attempts.Items) == 0 {
		return tracker.NativeRuntimeEvidence{}, errors.New("fixture work item has no recorded attempt")
	}
	return client.RuntimeEvidence(ctx, item, attempts.Items[0].AttemptID)
}

func (c *clihangNativeClient) ApproveProjectPolicy(ctx context.Context, change policy.Change) (policy.Approval, error) {
	return c.admin.ApproveProjectPolicy(ctx, change)
}

func (c *clihangNativeClient) Transition(ctx context.Context, item tracker.NativeWorkItemID, change tracker.Transition) (tracker.NativeIssue, error) {
	return c.admin.Transition(ctx, item, change)
}
