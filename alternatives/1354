package nativechange

import (
	"context"
	"time"

	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeClaim struct {
	source          *NativeConnector
	lease           tracker.NativeLease
	recovery        tracker.NativeRecovery
	landingCapacity int
	deadline        time.Time
	execution       executioncontract.Execution
}
type executionAuthority struct {
	host            executioncontract.ExecutionHost
	client          *Client
	now             func() time.Time
	leaseTTL        time.Duration
	providerReports func() ([]providercapacity.Report, error)
}

func authority(host executioncontract.ExecutionHost) *executionAuthority {
	return &executionAuthority{host: host, client: host.Transport(), now: host.Now, leaseTTL: host.LeaseDuration(), providerReports: host.ProviderObservations()}
}
func (s *executionAuthority) checkClaimPolicy(ctx context.Context, item, policyID string) error {
	return s.host.ValidateExecutionPolicy(ctx, item, policyID)
}
func (s *executionAuthority) nativeClaimError(ctx context.Context, item string, token tracker.FencingToken, err error) error {
	return s.host.ExecutionError(ctx, item, token, err)
}
func (s *executionAuthority) cleanupNativeClaim(ctx context.Context, item string, token tracker.FencingToken) {
	s.host.CleanupExecution(ctx, item, token)
}

func (s *executionAuthority) CheckProjectPolicy(ctx context.Context, project, repository string, p policy.Descriptor) error {
	return s.host.CheckProjectPolicy(ctx, project, repository, p)
}
