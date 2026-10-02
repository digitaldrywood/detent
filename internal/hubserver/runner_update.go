package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type runnerUpdateChange struct {
	ExpectedRevision      int64  `json:"expected_revision"`
	ExpectedBuildRevision string `json:"expected_build_revision"`
	Service               string `json:"service"`
	Version               string `json:"version"`
	Release               bool   `json:"release"`
	FromRelease           bool   `json:"from_release"`
	Confirm               bool   `json:"confirm"`
	IdempotencyKey        string `json:"idempotency_key,omitempty"`
}

func (r runnerUpdateChange) delivery(id string, at time.Time) runnerauth.UpdateRequest {
	return runnerauth.UpdateRequest{RequestedAt: at, ID: id, Service: r.Service, Version: r.Version, Release: r.Release, FromRelease: r.FromRelease, ExpectedBuildRevision: r.ExpectedBuildRevision}
}

func runnerUpdateReady(r runnerauth.Runner, request runnerUpdateChange, now time.Time) error {
	view := r.UpdateView(now)
	if view.Status == "unavailable" || r.Update != nil && r.Update.Discovery == "unknown" {
		return &nativeError{Code: "unavailable", Message: "The enrolled runner has no current installed update owner; upgrade or reconnect that runner", status: http.StatusServiceUnavailable}
	}
	if view.Status == "requested" || view.Status == "draining" || view.Status == "uncertain" || view.Status == "applied" || view.Status == "restart_requested" {
		return &nativeError{Code: "revision_conflict", Message: "The existing runner update has not settled; read its current receipt", status: http.StatusConflict}
	}
	if request.ExpectedRevision != r.Revision || request.ExpectedBuildRevision != r.Update.Revision || request.Version != r.Update.AvailableVersion || !request.Release && !r.Update.Pending {
		return nativeConflict(tracker.Revision(r.Revision))
	}
	return nil
}

func (s *Service) readRunnerUpdate(ctx context.Context, scope nativeScope, resource string) (any, error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
		return nil, err
	}
	runner, err := readRunner(ctx, tx, scope.organization, resource, now)
	if err != nil {
		return nil, err
	}
	return runner.UpdateView(now), nil
}

func (s *Service) applyRunnerUpdateCommand(ctx context.Context, scope nativeScope, resource string, request runnerUpdateChange) (any, error) {
	if request.ExpectedRevision < 1 || request.delivery("validate", s.config.now()).Validate() != nil || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativeInvalid("A selected service, observed runner/build revision, release version and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &nativeError{Code: "confirmation_required", Message: "Confirm the selected runner update and coordinated restart", status: http.StatusPreconditionRequired}
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.requireRunnerAdministration(ctx, tx, scope, now); err != nil {
		return nil, err
	}
	runner, err := readRunner(ctx, tx, scope.organization, resource, now)
	if err != nil {
		return nil, err
	}
	operation := "runner_update " + resource
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	raw, found, err := nativeCommandReceipt(ctx, tx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return json.RawMessage(raw), tx.Commit()
	}
	if err := runnerUpdateReady(runner, request, now); err != nil {
		return nil, err
	}
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	delivery := request.delivery(newNativeID("update"), now)
	runner.UpdateRequest = &delivery
	settings, err := marshalNative(settingsFromRouting(runner.Routing))
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET routing_settings_json = ?, revision = revision + 1 WHERE organization_id = ? AND id = ?", settings, scope.organization, resource); err != nil {
		return nil, err
	}
	runner.Revision++
	view := runner.UpdateView(now)
	raw, err = marshalNative(view)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)", scope.organization, scope.credential.ID, operation, request.IdempotencyKey, hash, raw, formatHubTime(now)); err != nil {
		return nil, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return view, nil
}

func storeRunnerUpdateObservation(ctx context.Context, tx *sql.Tx, scope nativeScope, report *runnerauth.UpdateObservation, protocol int, version, platform, architecture string, now time.Time) error {
	if report != nil {
		if report.Validate() != nil || protocol != 2 || strings.TrimPrefix(report.Running.Version, "v") != strings.TrimPrefix(version, "v") || report.Running.OS != platform || report.Running.Architecture != architecture {
			return nativeInvalid("Update observation must identify this runner's actual process and supported protocol")
		}
		runner, err := readRunner(ctx, tx, scope.organization, scope.credential.Runner.RunnerID, now)
		if err != nil {
			return err
		}
		if report.Receipt != nil {
			matches := runner.UpdateRequest != nil && report.Receipt.Request == *runner.UpdateRequest
			previous := runner.Update != nil && runner.Update.Receipt != nil && report.Receipt.Request == runner.Update.Receipt.Request
			if !matches && !previous {
				accepted, err := runnerUpdateReceiptMatches(ctx, tx, scope, report.Receipt.Request)
				if err != nil {
					return err
				}
				if !accepted {
					return nativeInvalid("Update receipt does not belong to this enrolled runner request")
				}
			}
		}
		report.ReceivedAt = now
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runner_identities SET update_observation_json = ? WHERE organization_id = ? AND id = ?", string(raw), scope.organization, scope.credential.Runner.RunnerID)
	return err
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
		return invalidAPIRequest(c, err)
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
