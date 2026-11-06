package hubserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

type runnerUpdateChange struct {
	ExpectedRevision      int64  `json:"expected_revision"`
	ExpectedBuildRevision string `json:"expected_build_revision"`
	Service               string `json:"service"`
	Version               string `json:"version"`
	Release               bool   `json:"release"`
	FromRelease           bool   `json:"from_release"`
	Confirm               bool   `json:"confirm,omitempty"`
	IdempotencyKey        string `json:"idempotency_key,omitempty"`
}

func (r runnerUpdateChange) delivery(id string, at time.Time) runnerauth.UpdateRequest {
	return runnerauth.UpdateRequest{RequestedAt: canonicalRunnerUpdateTime(at), ID: id, Service: r.Service, Version: r.Version, Release: r.Release, FromRelease: r.FromRelease, ExpectedBuildRevision: r.ExpectedBuildRevision}
}

func canonicalRunnerUpdateTime(at time.Time) time.Time {
	return time.UnixMicro(at.UnixMicro()).UTC()
}

func runnerUpdateReady(r runnerauth.Runner, request runnerUpdateChange, now time.Time) error {
	view := r.UpdateView(now)
	if view.Status == "unavailable" || r.Update != nil && r.Update.Discovery == "unknown" {
		return &nativeError{Code: "unavailable", Message: "The enrolled runner has no current installed update owner; upgrade or reconnect that runner", status: http.StatusServiceUnavailable}
	}
	if view.Status == "requested" || view.Status == "applying" || view.Status == "draining" || view.Status == "uncertain" || view.Status == "applied" && (r.Update.Receipt == nil || !r.Update.Receipt.ExecutionReady()) || view.Status == "restart_requested" {
		return runnerUpdateConflict(r.Revision, "The existing runner update has not settled; read get_runner_update and wait for its running or refused receipt")
	}
	if request.ExpectedRevision != r.Revision {
		return runnerUpdateConflict(r.Revision, "The runner revision has changed; read get_runner_update and use its current revision")
	}
	if request.ExpectedBuildRevision != r.Update.Revision {
		return runnerUpdateConflict(r.Revision, "The observed build revision has changed; read get_runner_update and use its current observation revision")
	}
	repair := request.Release && strings.TrimPrefix(request.Version, "v") == strings.TrimPrefix(r.Update.Running.Version, "v") && r.Update.Running.Source == "release" && !r.Update.Running.VerifiedRelease
	if request.Version != r.Update.AvailableVersion && !repair {
		return runnerUpdateConflict(r.Revision, "The selected version differs from the observed available version; read get_runner_update and select its available version")
	}
	if !request.Release && !r.Update.Pending {
		return runnerUpdateConflict(r.Revision, "The runner has no pending automatic update; read get_runner_update and explicitly select a release")
	}
	return nil
}

func runnerUpdateConflict(revision int64, message string) error {
	return &nativeError{Code: "revision_conflict", Message: message, CurrentRevision: tracker.Revision(revision), status: http.StatusConflict, publicMessage: true}
}

func (s *Service) readRunnerUpdate(ctx context.Context, scope nativeScope, resource string) (any, error) {
	return s.runnerUpdateViewTenant(ctx, scope, resource)
}

func (s *Service) applyRunnerUpdateCommand(ctx context.Context, scope nativeScope, resource string, request runnerUpdateChange) (any, error) {
	return s.applyRunnerUpdateCommandTenant(ctx, scope, resource, request)
}

func validateRunnerUpdateObservation(report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string) error {
	if report != nil && (report.Validate() != nil || protocol != 2 || strings.TrimPrefix(report.Running.Version, "v") != strings.TrimPrefix(version, "v") || report.Running.OS != platform || report.Running.Architecture != architecture) {
		return nativePublicInvalid("Update observation must identify this runner's actual process and supported protocol")
	}
	return nil
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

func runnerUpdateSuperseded(current runnerauth.UpdateRequest, receipt *runnerauth.UpdateReceipt, target string) bool {
	reported := receipt != nil && receipt.Request.Matches(current)
	completed := reported && (receipt.ExecutionReady() || receipt.Status == "applied" && receipt.Running != nil && receipt.Applied != nil && receipt.Running.Matches(*receipt.Applied))
	if reported && !completed && !slices.Contains([]string{"running", "refused", "uncertain"}, receipt.Status) {
		return false
	}
	comparison, err := update.CompareVersions(current.Version, target)
	return err != nil || comparison < 0 || reported && comparison == 0
}
