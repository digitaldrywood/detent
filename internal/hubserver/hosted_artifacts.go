package hubserver

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func (s *Service) hostedArtifactAllowances(c echo.Context) error {
	runtimePlans := s.database.plans()
	credential, _, err := s.authenticateAPIRequest(c)
	if err != nil || credential.Scope != apiScopeWorker || !credential.NativeOnly || credential.Hosted != nil || credential.Runner.RunnerID != "" || c.Request().Header.Get(echo.HeaderAuthorization) == "" {
		return s.hostedJSONError(c, http.StatusForbidden, "A scoped artifact publisher credential is required")
	}
	if runtimePlans == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	var usage artifact.Usage
	if err := decodeAPIJSON(c, &usage); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	ctx := c.Request().Context()
	legacyBinding, err := legacyHostedArtifactBinding(ctx, s.database.reader, c.Param("service"), s.config.Hosted.OrganizationID, credential.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM artifact_services s JOIN token_grants g ON g.token_id=s.publisher_token_id AND g.project_id=s.project_id AND g.organization_id=s.organization_id WHERE s.id=? AND s.organization_id=? AND s.publisher_token_id=? AND ((s.binding_mode='hosted' AND s.hosted_opt_in=1) OR ((s.binding_mode IS NULL OR s.hosted_opt_in IS NULL) AND s.binding_json=?))`, c.Param("service"), s.config.Hosted.OrganizationID, credential.ID, legacyBinding).Scan(&count); err != nil {
		return s.nativeAPIError(c, err)
	}
	if count == 0 {
		return s.hostedJSONError(c, http.StatusForbidden, "Artifact publisher is not bound to this service")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_artifact_usage(singleton,service_id,usage_json,observed_at,retained_bytes,reserved_bytes) VALUES(1,?,'{}',0,0,0) ON CONFLICT DO NOTHING", c.Param("service")); err != nil {
		return s.nativeAPIError(c, err)
	}
	var service string
	var observed int64
	if err := tx.QueryRowContext(ctx, "SELECT service_id,observed_at FROM hosted_artifact_usage WHERE singleton=1").Scan(&service, &observed); err != nil {
		return s.nativeAPIError(c, err)
	}
	if service != c.Param("service") {
		return s.nativeAPIError(c, &hostedLimitError{Resource: "hosted_artifact_services", Allowance: 1, Consumption: 1})
	}
	now := s.config.now()
	entitlement, err := s.database.hostedEntitlement(ctx, tx, now)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	for _, amount := range []int64{usage.RetainedBytes, usage.ReservedBytes, usage.RelayBytes, usage.StorageRequests} {
		if amount < 0 || amount > 1<<50 {
			return s.nativeAPIError(c, nativePublicInvalid("Artifact usage is invalid"))
		}
	}
	if usage.ObservedAt.After(now.Add(time.Minute)) {
		return s.nativeAPIError(c, nativePublicInvalid("Artifact usage clock is invalid"))
	}
	if !usage.ObservedAt.IsZero() && usage.ObservedAt.UnixNano() > observed {
		raw, err := json.Marshal(usage)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE hosted_artifact_usage SET usage_json=?,observed_at=?,retained_bytes=?,reserved_bytes=? WHERE singleton=1", string(raw), usage.ObservedAt.UnixNano(), usage.RetainedBytes, usage.ReservedBytes); err != nil {
			return s.nativeAPIError(c, err)
		}
		window := now.Unix() / runtimePlans.WindowSeconds * runtimePlans.WindowSeconds
		if usage.WindowStart.Unix() == window {
			for _, sample := range []struct {
				name   string
				amount int64
			}{{"relay_bytes", usage.RelayBytes}, {"storage_requests", usage.StorageRequests}} {
				if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_usage_windows(window_start,metric,amount) VALUES(?,?,?) ON CONFLICT(window_start,metric) DO UPDATE SET amount=max(amount,excluded.amount)`, window, sample.name, sample.amount); err != nil {
					return s.nativeAPIError(c, err)
				}
			}
		}
	}
	limits := artifact.Limits{RelayBytes: entitlement.Allowances["relay_bytes"], WindowSeconds: runtimePlans.WindowSeconds, TelemetrySeconds: runtimePlans.WindowSeconds * runtimePlans.RetentionWindows}
	if slices.Contains(entitlement.Features, "hosted_artifacts") {
		limits.RetainedBytes = entitlement.Allowances["artifact_retained_bytes"]
		limits.ReservedBytes = entitlement.Allowances["artifact_reserved_bytes"]
		limits.ArtifactBytes = entitlement.Allowances["artifact_bytes"]
		limits.UploadBytes = entitlement.Allowances["artifact_upload_bytes"]
		limits.RetentionSeconds = entitlement.Allowances["artifact_retention_seconds"]
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, limits)
}
