package hubserver

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) hostedArtifactAllowances(c echo.Context) error {
	runtimePlans := s.database.plans()
	credential, _, err := s.authenticateAPIRequest(c)
	if err != nil || credential.Scope != apiScopeWorker || !credential.NativeOnly || credential.Hosted != nil || credential.Runner.RunnerID != "" || c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		return s.hostedJSONError(c, http.StatusForbidden, "A scoped artifact publisher credential is required")
	}
	if runtimePlans == nil || runtimePlans.WindowSeconds <= 0 || s.config.TenantRepository == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	var usage artifact.Usage
	if err := decodeAPIJSON(c, &usage); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	now := s.config.now()
	for _, amount := range []int64{usage.RetainedBytes, usage.ReservedBytes, usage.RelayBytes, usage.StorageRequests} {
		if amount < 0 || amount > 1<<50 {
			return s.nativeAPIError(c, nativePublicInvalid("Artifact usage is invalid"))
		}
	}
	if usage.ObservedAt.After(now.Add(time.Minute)) {
		return s.nativeAPIError(c, nativePublicInvalid("Artifact usage clock is invalid"))
	}
	ctx := c.Request().Context()
	var entitlement HostedEntitlement
	err = s.config.TenantRepository.WriteTenant(ctx, s.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.AccessCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		if !current.Valid(credential.Hash, now) || current.Scope != string(apiScopeWorker) || !current.NativeOnly {
			return corestore.ErrNotFound
		}
		if err := w.RecordHostedArtifactUsage(ctx, c.Param("service"), credential.ID, usage, now, runtimePlans.WindowSeconds); err != nil {
			return err
		}
		entitlement, err = s.database.hostedEntitlementRecord(ctx, w, now)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return s.hostedJSONError(c, http.StatusForbidden, "Artifact publisher is not bound to this service")
	}
	if errors.Is(err, artifact.ErrQuota) {
		return s.nativeAPIError(c, &hostedLimitError{Resource: "hosted_artifact_services", Allowance: 1, Consumption: 1})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	limits := artifact.Limits{RelayBytes: entitlement.Allowances["relay_bytes"], WindowSeconds: runtimePlans.WindowSeconds, TelemetrySeconds: runtimePlans.WindowSeconds * runtimePlans.RetentionWindows}
	if slices.Contains(entitlement.Features, "hosted_artifacts") {
		limits.RetainedBytes = entitlement.Allowances["artifact_retained_bytes"]
		limits.ReservedBytes = entitlement.Allowances["artifact_reserved_bytes"]
		limits.ArtifactBytes = entitlement.Allowances["artifact_bytes"]
		limits.UploadBytes = entitlement.Allowances["artifact_upload_bytes"]
		limits.RetentionSeconds = entitlement.Allowances["artifact_retention_seconds"]
	}
	return c.JSON(http.StatusOK, limits)
}
