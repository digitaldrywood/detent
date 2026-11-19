package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerupdate"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	minimumSupportedRunnerVersion = runnerupdate.MinimumSupportedVersion
	urgentRunnerUpdatePath        = "/api/v2/organizations/:organization/runner-update/urgent"
)

type (
	runnerUpdateChange       = runnerupdate.Change
	urgentRunnerUpdate       = runnerupdate.UrgentUpdate
	urgentRunnerUpdateChange = runnerupdate.UrgentChange
)

func (s *Service) runnerUpdateService() *runnerupdate.Service {
	return runnerupdate.New(runnerupdate.Deps{
		Repository:      s.config.TenantRepository,
		Now:             s.config.Clock,
		Version:         s.config.Version,
		ReleaseClient:   s.config.RunnerReleaseClient,
		VerifySignature: s.config.Deps.VerifyReleaseSignature,
		Published:       &s.runnerPublishedReleases,
		ApplyUsage:      s.applyRunnerTenantUsage,
		Details:         s.runDetailStore,
		ContentContext:  s.contentContext,
	})
}

func runnerUpdateTarget(version string) string {
	return runnerupdate.Target(version)
}

func minimumRunnerVersion(version string) string {
	return runnerupdate.MinimumVersion(version)
}

func runnerClaimRefusal(minimum, reported string) string {
	return runnerupdate.ClaimRefusal(minimum, reported)
}

func runnerVersionError(minimum, reported string) error {
	return hubapi.FromFeatureError(runnerupdate.VersionError(minimum, reported))
}

func runnerUpdateReady(r runnerauth.Runner, request runnerUpdateChange, now time.Time) error {
	return hubapi.FromFeatureError(runnerupdate.Ready(r, request, now))
}

func runnerUpdateConflict(revision int64, message string) error {
	return hubapi.FromFeatureError(runnerupdate.Conflict(revision, message))
}

func validateRunnerUpdateObservation(report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string) error {
	return hubapi.FromFeatureError(runnerupdate.ValidateObservation(report, protocol, version, platform, architecture))
}

func applyUrgentRunnerRouting(r *runnerauth.Runner, urgent urgentRunnerUpdate, version string) {
	runnerupdate.ApplyUrgentRouting(r, urgent, version)
}

func (s *Service) runnerReleasePublished(ctx context.Context, version, platform, architecture string) bool {
	return s.runnerUpdateService().ReleasePublished(ctx, version, platform, architecture)
}

func (s *Service) recordWorkerHubVersion(previous *tracker.NativeRunData, current *tracker.NativeRunData) {
	s.runnerUpdateService().RecordWorkerHubVersion(previous, current)
}

func (s *Service) storeRunnerUpdateObservationTenant(ctx context.Context, writer tenantstore.Writer, identity runnerstore.Identity, report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string, now time.Time, releasePublished bool) error {
	return hubapi.FromFeatureError(s.runnerUpdateService().StoreObservation(ctx, writer, identity, report, protocol, version, platform, architecture, now, releasePublished))
}

func (s *Service) readRunnerUpdate(ctx context.Context, scope hubscope.NativeScope, resource string) (any, error) {
	scope.ManagedRunner = resource
	value, err := s.runnerUpdateService().View(ctx, s.authorization().Scope(scope), resource)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) applyRunnerUpdateCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request runnerUpdateChange) (any, error) {
	scope.ManagedRunner = resource
	value, err := s.runnerUpdateService().Apply(ctx, s.authorization().Scope(scope), resource, request)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) readUrgentRunnerUpdateCommand(ctx context.Context, scope hubscope.NativeScope) (any, error) {
	value, err := s.runnerUpdateService().ReadUrgent(ctx, s.authorization().Scope(scope))
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) markUrgentRunnerUpdateCommand(ctx context.Context, scope hubscope.NativeScope, request urgentRunnerUpdateChange) (any, error) {
	value, err := s.runnerUpdateService().MarkUrgent(ctx, s.authorization().Scope(scope), request)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) registerRunnerUpdateRoutes(e *echo.Echo) {
	runnerupdate.RegisterRoutes(e.Group(""), runnerupdate.HTTPDeps{
		Service: s.runnerUpdateService, Auth: s.requestAuth,
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			return hubscope.AuthCredential(credential), ok
		},
		Admin: s.requireInstanceAdmin(), Worker: s.requireAPIScope(hubapi.APIScopeWorker),
		Error:   func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		Invalid: s.invalidAPIRequest,
	})
}
