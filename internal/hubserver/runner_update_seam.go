package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

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
	runnerReceiptWrite       = runnerupdate.ReceiptWrite
)

func (s *Service) runnerUpdateService() *runnerupdate.Service {
	return runnerupdate.New(runnerupdate.Deps{
		Repository:      s.config.TenantRepository,
		Now:             s.config.now,
		Version:         s.config.Version,
		ReleaseClient:   s.config.RunnerReleaseClient,
		VerifySignature: s.config.runnerReleaseSignatureVerifier,
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
	return fromFeatureError(runnerupdate.VersionError(minimum, reported))
}

func runnerUpdateReady(r runnerauth.Runner, request runnerUpdateChange, now time.Time) error {
	return fromFeatureError(runnerupdate.Ready(r, request, now))
}

func runnerUpdateConflict(revision int64, message string) error {
	return fromFeatureError(runnerupdate.Conflict(revision, message))
}

func validateRunnerUpdateObservation(report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string) error {
	return fromFeatureError(runnerupdate.ValidateObservation(report, protocol, version, platform, architecture))
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
	return fromFeatureError(s.runnerUpdateService().StoreObservation(ctx, writer, identity, report, protocol, version, platform, architecture, now, releasePublished))
}

func (s *Service) readRunnerUpdate(ctx context.Context, scope nativeScope, resource string) (any, error) {
	scope.managedRunner = resource
	value, err := s.runnerUpdateService().View(ctx, s.featureScope(scope), resource)
	return value, fromFeatureError(err)
}

func (s *Service) applyRunnerUpdateCommand(ctx context.Context, scope nativeScope, resource string, request runnerUpdateChange) (any, error) {
	scope.managedRunner = resource
	value, err := s.runnerUpdateService().Apply(ctx, s.featureScope(scope), resource, request)
	return value, fromFeatureError(err)
}

func (s *Service) readUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope) (any, error) {
	value, err := s.runnerUpdateService().ReadUrgent(ctx, s.featureScope(scope))
	return value, fromFeatureError(err)
}

func (s *Service) markUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope, request urgentRunnerUpdateChange) (any, error) {
	value, err := s.runnerUpdateService().MarkUrgent(ctx, s.featureScope(scope), request)
	return value, fromFeatureError(err)
}

func (s *Service) getRunnerUpdate(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.readRunnerUpdate(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) applyRunnerUpdate(c echo.Context) error {
	var request runnerUpdateChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.applyRunnerUpdateCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusAccepted, value)
}

func (s *Service) getUrgentRunnerUpdate(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	value, err := s.readUrgentRunnerUpdateCommand(c.Request().Context(), scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) markUrgentRunnerUpdate(c echo.Context) error {
	var request urgentRunnerUpdateChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}
	value, err := s.markUrgentRunnerUpdateCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusAccepted, value)
}

func (s *Service) runnerReceiptScope(c echo.Context) (nativeScope, error) {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Runner.RunnerID == "" || credential.Runner.RunnerID != c.Param("runner") || string(credential.Runner.OrganizationID) != c.Param("organization") {
		return nativeScope{}, runnerUnauthorized()
	}
	return nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential, managedRunner: c.Param("runner")}, nil
}

func (s *Service) getRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err := s.runnerUpdateService().Receipt(c.Request().Context(), s.featureScope(scope), scope.managedRunner)
	if err != nil {
		return s.nativeAPIError(c, fromFeatureError(err))
	}
	return c.JSON(http.StatusOK, receipt)
}

func (s *Service) putRunnerUpdateReceipt(c echo.Context) error {
	scope, err := s.runnerReceiptScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var request runnerReceiptWrite
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.runnerUpdateService().PutReceipt(c.Request().Context(), s.featureScope(scope), scope.managedRunner, request); err != nil {
		return s.nativeAPIError(c, fromFeatureError(err))
	}
	return c.NoContent(http.StatusNoContent)
}
