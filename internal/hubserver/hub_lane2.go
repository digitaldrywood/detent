package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) runnerRoutingDeps() runnerfleet.RoutingDeps {
	return runnerfleet.RoutingDeps{
		LandingCapacityKey:  landingCapacityKey,
		Repository:          s.config.TenantRepository,
		Now:                 s.config.Clock,
		HTTPError:           s.nativeAPIError,
		InvalidRequest:      s.invalidAPIRequest,
		Details:             s.runDetailStore,
		NotifyDispatch:      s.runnerNotifyDispatch(),
		AuthorizeAccess:     s.authorizeAccessScope,
		ManagementProjects:  runnerManagementProjects,
		FleetRecord:         runnerFromFleetRecord,
		AllRunnerGrants:     s.appAllRunnerGrants,
		AllRunnerGrantsWith: s.appAllRunnerGrantsWith,
		AuthorizeProject:    s.authorizeProjectRecord,
	}
}

func (s *Service) getRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().GetRunnerRouting(c)
}

type runnerProjectRankReader = runnerfleet.RunnerProjectRankReader

func (s *Service) readRunnerRouting(ctx context.Context, scope hubscope.NativeScope, runnerID string) (runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ReadRunnerRouting(ctx, scope, runnerID)
}

type runnerRoutingRequest runnerfleet.RunnerRoutingRequest

func (request runnerRoutingRequest) effective(current runnerauth.Routing) runnerauth.RoutingChange {
	return runnerfleet.RunnerRoutingRequest(request).Effective(current)
}

func (s *Service) updateRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().UpdateRunnerRouting(c)
}

func (s *Service) updateRunnerRoutingCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerRoutingRequest) (any, error) {
	return s.runnerRoutingDeps().UpdateRunnerRoutingCommand(ctx, scope, resource, runnerfleet.RunnerRoutingRequest(request))
}

func (s *Service) updateRunnerHost(c echo.Context) error {
	return s.runnerRoutingDeps().UpdateRunnerHost(c)
}

func (s *Service) updateRunnerHostCommand(ctx context.Context, scope hubscope.NativeScope, resource string, change runnerauth.HostChange) (any, error) {
	return s.runnerRoutingDeps().UpdateRunnerHostCommand(ctx, scope, resource, change)
}

func (s *Service) requireRunnerAdministrationTenant(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope) error {
	return s.runnerRoutingDeps().RequireRunnerAdministrationTenant(ctx, reader, scope)
}

func runnerFromRoutingRecord(record runnerstore.RoutingRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	return (runnerfleet.RoutingDeps{FleetRecord: runnerFromFleetRecord, LandingCapacityKey: landingCapacityKey}).FromRoutingRecord(record, now, urgent)
}

func (s *Service) listRunnerRouting(c echo.Context) error {
	return s.runnerRoutingDeps().ListRunnerRouting(c)
}

func (s *Service) listRunnerRoutingData(ctx context.Context, organization tracker.OrganizationID, limit, offset int) ([]runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ListRunnerRoutingData(ctx, organization, limit, offset)
}

type routingPolicyKey = runnerfleet.RoutingPolicyKey

func readRoutingLeasePolicies(ctx context.Context, reader policystore.Reader, runners ...runnerauth.Runner) (map[routingPolicyKey]policystore.Revision, error) {
	return runnerfleet.ReadRoutingLeasePolicies(ctx, reader, runners...)
}

func (s *Service) pinRoutingLeasePolicies(ctx context.Context, rows map[routingPolicyKey]policystore.Revision, runners ...runnerauth.Runner) error {
	return s.runnerRoutingDeps().PinRoutingLeasePolicies(ctx, rows, runners...)
}

func runnerExcluded(exclusions []runnerauth.Exclusion) error {
	return runnerfleet.RunnerExcluded(exclusions)
}

func (s *Service) validateRunnerLease(c echo.Context) error {
	return s.runnerRoutingDeps().ValidateRunnerLease(c)
}

func runnerLeaseMutationRoute(path string) bool {
	return runnerfleet.RunnerLeaseMutationRoute(path)
}

func (s *Service) authorizeRunnerLeaseProject(ctx context.Context, scope hubscope.NativeScope) error {
	return s.runnerRoutingDeps().AuthorizeRunnerLeaseProject(ctx, scope)
}

func runnerIdentityMatchesHeldLease(identity runnerstore.Identity, organization tracker.OrganizationID, runnerID, tokenID string, machine tracker.MachineID, now time.Time) bool {
	return runnerfleet.RunnerIdentityMatchesHeldLease(identity, organization, runnerID, tokenID, machine, now)
}

func (s *Service) validateRunnerLeaseTenant(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, fencing tracker.FencingToken) (runnerauth.Runner, error) {
	return s.runnerRoutingDeps().ValidateRunnerLeaseTenant(ctx, scope, leaseID, fencing)
}

var errLeaseSelectorUnbound = runnerfleet.ErrLeaseSelectorUnbound

func checkLeaseSelector(pinned policystore.LeasePolicy, identity runnerstore.Identity, legacyChecked bool) error {
	return runnerfleet.CheckLeaseSelector(pinned, identity, legacyChecked)
}

func (s *Service) checkLegacyLeaseSelector(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID) error {
	return s.runnerRoutingDeps().CheckLegacyLeaseSelector(ctx, scope, leaseID)
}

func (s *Service) runnerNotifyDispatch() func(tracker.OrganizationID) {
	if s.notifications == nil {
		return nil
	}
	return func(org tracker.OrganizationID) { s.notifications.notify(dispatchNotificationKey(org)) }
}

func (s *Service) runnerCapacityDeps() runnerfleet.CapacityDeps {
	return runnerfleet.CapacityDeps{
		Repository:        s.config.TenantRepository,
		Now:               s.config.Clock,
		HTTPError:         s.nativeAPIError,
		InvalidRequest:    s.invalidAPIRequest,
		Details:           s.runDetailStore,
		ReadRouting:       s.readRunnerRouting,
		AuthorizeCapacity: s.authorizeRunnerCapacity,
		PlansEnabled:      func() bool { return s.database.plansService().Plans() != nil },
		Entitlement: func(ctx context.Context, query hubapi.NativeQueryer, now time.Time) (hostedplans.HostedEntitlement, error) {
			return hubapi.FeatureResult(s.database.plansService().Entitlement(ctx, query, now))
		},
	}
}

type runnerCapacityChange = runnerfleet.RunnerCapacityChange

func (s *Service) persistRunnerCapacityCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerCapacityChange) (any, error) {
	return s.runnerCapacityDeps().PersistRunnerCapacityCommand(ctx, scope, resource, request)
}

func (s *Service) updateRunnerCapacity(c echo.Context) error {
	return s.runnerCapacityDeps().UpdateRunnerCapacity(c)
}

func (s *Service) runnerCapacityView(ctx context.Context, query hubapi.NativeQueryer, r runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
	return s.runnerCapacityDeps().RunnerCapacityView(ctx, query, r, backend, now)
}
