package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Authority interface {
	Validate(context.Context) error
	Start(context.Context, tracker.NativeExecutionIdentity) error
	DeliveryIdentity(tracker.NativeExecutionIdentity) tracker.NativeExecutionIdentity
	AcceptDelivery(tracker.ReleaseDeliveryStatus)
	ObserveRuntime(context.Context, tracker.NativeRuntimeObservation) error
}
type State struct {
	Client                   *api.NativeClient
	Delivery                 *tracker.ReleaseDelivery
	Lease                    tracker.NativeLease
	ConversationContinuation bool
	Now                      func() time.Time
}

func Reconcile(ctx context.Context, host Authority, state State, localAttempt int64, generation uint64) (runner.ReleaseDeliveryResult, error) {
	delivery := state.Delivery
	if delivery == nil || state.ConversationContinuation {
		return runner.ReleaseDeliveryResult{}, nil
	}
	if err := host.Validate(ctx); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	path, err := api.NativeItemPath(state.Lease.WorkItemID)
	if err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	var status tracker.ReleaseDeliveryStatus
	if err := state.Client.Transport().Request(ctx, http.MethodGet, state.Client.Scope()+path+"/release-pick", nil, &status); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	if status.ReleasePickInput != delivery.ReleasePickInput || status.ReleaseHeadSHA == "" {
		return runner.ReleaseDeliveryResult{}, errors.New("release delivery verification identity mismatch")
	}
	if status.DeliveredSHA == "" && !status.Closed {
		return runner.ReleaseDeliveryResult{}, nil
	}
	identity := tracker.NativeExecutionIdentity{Role: runner.RoleMerge, Backend: "git", Model: "none"}
	if reservation := state.Lease.ProviderReservation; reservation != nil {
		identity.Role, identity.Backend, identity.Model = reservation.Role, reservation.Backend, reservation.Model
	}
	identity = host.DeliveryIdentity(identity)
	if err := host.Start(ctx, identity); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	host.AcceptDelivery(status)
	at := state.Now().UTC()
	runtimeIdentity := runner.NativeLandingIdentity(at)
	if err := host.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{LocalAttemptID: localAttempt, Generation: generation, Phase: "merging", HeartbeatAt: at, Identity: runtimeIdentity}); err != nil {
		return runner.ReleaseDeliveryResult{}, err
	}
	result := runner.RunResult{FinalState: runner.FinalStateCompleted, RuntimeIdentity: runtimeIdentity}
	if status.DeliveredSHA != "" {
		result.NativeLanding = &runner.NativeLanding{HeadSHA: delivery.SourceMergeSHA, BaseSHA: delivery.FrozenSHA, BaseRef: delivery.BaseRef, MergeSHA: status.DeliveredSHA, Landed: true}
		result.FinalMessage = fmt.Sprintf("Verified source commit %s on %s as %s at release head %s.", delivery.SourceMergeSHA, delivery.BaseRef, status.DeliveredSHA, status.ReleaseHeadSHA)
	} else {
		action := "The delivery PR is closed without merge and its source is absent from the release branch. Reopen the delivery PR or confirm that this release delivery should be cancelled."
		result.FinalMessage = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: " + strconv.Quote(action) + "\n```"
	}
	return runner.ReleaseDeliveryResult{Settled: true, Result: result}, nil
}
