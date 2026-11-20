package nativechange

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/delivery"
	landingrecord "github.com/digitaldrywood/detent/internal/hubclient/landing"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func (e *nativeExecution) ReconcileReleaseDelivery(ctx context.Context, localAttempt int64, generation uint64) (runner.ReleaseDeliveryResult, error) {
	if e.claim.recovery.Issue.ReleaseDelivery == nil || e.conversationContinuation {
		return runner.ReleaseDeliveryResult{}, nil
	}
	return delivery.Reconcile(ctx, deliveryAuthority{e}, delivery.State{Client: e.claim.source.NativeClient(), Delivery: e.claim.recovery.Issue.ReleaseDelivery, Lease: e.claim.lease, ConversationContinuation: e.conversationContinuation, Now: e.scheduler.now}, localAttempt, generation)
}

type deliveryAuthority struct{ *nativeExecution }

func (a deliveryAuthority) DeliveryIdentity(fallback tracker.NativeExecutionIdentity) tracker.NativeExecutionIdentity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.data.Identity != nil {
		return *a.data.Identity
	}
	return fallback
}
func (a deliveryAuthority) AcceptDelivery(status tracker.ReleaseDeliveryStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.role = runner.RoleMerge
	a.releaseDelivery = &status
}

// LandingTarget names the reviewed version a landing run puts on the base
// branch: the item's Change Request, its current version, and the merge
// method of the policy that version was published under.
func (e *nativeExecution) LandingTarget(ctx context.Context) (runner.NativeLandingTarget, error) {
	if err := api.MutationEffectError(ctx); err != nil {
		return runner.NativeLandingTarget{}, err
	}
	if e.claim.source == nil || e.claim.source.NativeClient() == nil {
		return runner.NativeLandingTarget{}, runner.ErrExecutionAuthorityUnavailable
	}
	client := e.claim.source.NativeClient()
	target, err := landingrecord.Target(ctx, client, e.claim.lease.WorkItemID, e.claim.recovery.Issue.ReleaseDelivery)
	if err != nil {
		return target, err
	}
	project, err := client.Project(ctx)
	if err != nil {
		return target, err
	}
	if strings.TrimSpace(project.CloneURL) == "" {
		return target, errors.New("landing requires a configured project repository")
	}
	if target.GitHubPullRequest && !gitexec.SamePublicationRepository("https://github.com/"+project.Repository, target.Repository) {
		return target, errors.New("reviewed GitHub repository differs from the configured project repository")
	}
	target.PublicationRepository = project.CloneURL
	return target, nil
}

// RecordLanding tells the hub the commit the base branch advanced to. The
// idempotency key is the attempt's, so a retried report records one landing.
func (e *nativeExecution) RecordLanding(ctx context.Context, landing runner.NativeLanding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recordLanding(ctx, landing)
}

func (e *nativeExecution) recordLanding(ctx context.Context, landing runner.NativeLanding) error {
	if e.claim.source == nil || e.claim.source.NativeClient() == nil {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if err := landingrecord.Record(ctx, e.claim.source.NativeClient(), e.claim.lease, e.data.AttemptID, landing); err != nil {
		return err
	}
	e.landed = true
	return nil
}
