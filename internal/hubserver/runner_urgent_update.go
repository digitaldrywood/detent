package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
	"github.com/labstack/echo/v4"
)

const urgentRunnerUpdatePath = "/api/v2/organizations/:organization/runner-update/urgent"

type urgentRunnerUpdate struct {
	Revision int64                     `json:"revision"`
	Request  *runnerauth.UpdateRequest `json:"request"`
}

type urgentRunnerUpdateChange struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Version          string `json:"version"`
	Confirm          bool   `json:"confirm"`
	IdempotencyKey   string `json:"idempotency_key"`
}

func readUrgentRunnerUpdate(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID) (urgentRunnerUpdate, error) {
	var raw string
	var value urgentRunnerUpdate
	if err := db.QueryRowContext(ctx, "SELECT urgent_runner_update_json FROM organizations WHERE id = ?", organization).Scan(&raw); err != nil {
		return value, err
	}
	err := json.Unmarshal([]byte(raw), &value)
	return value, err
}

func applyUrgentRunnerRouting(ctx context.Context, db nativeQueryer, r *runnerauth.Runner) error {
	urgent, err := readUrgentRunnerUpdate(ctx, db, r.OrganizationID)
	if err != nil || urgent.Request == nil {
		return err
	}
	var version string
	if r.Update != nil {
		version = r.Update.Running.Version
	} else if err := db.QueryRowContext(ctx, "SELECT version FROM machines WHERE organization_id = ? AND id = ?", r.OrganizationID, r.MachineID).Scan(&version); err != nil {
		return err
	}
	comparison, err := update.CompareVersions(urgent.Request.Version, version)
	if err != nil || comparison <= 0 {
		if r.Update != nil && r.Update.Receipt != nil && r.Update.Receipt.Request == *urgent.Request {
			r.UpdateRequest = urgent.Request
		}
		return nil
	}
	if r.State == "active" {
		r.State = "draining"
	}
	r.UpdateRequest = urgent.Request
	return nil
}

func runnerUpdateReceiptMatches(ctx context.Context, tx *sql.Tx, scope nativeScope, request runnerauth.UpdateRequest) (bool, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT routing_settings_json FROM runner_identities WHERE organization_id = ? AND id = ?", scope.organization, scope.credential.Runner.RunnerID).Scan(&raw); err != nil {
		return false, err
	}
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return false, err
	}
	if settings.UpdateRequest != nil && request == *settings.UpdateRequest {
		return true, nil
	}
	if !request.Urgent {
		return false, nil
	}
	err := tx.QueryRowContext(ctx, "SELECT response_json FROM native_commands WHERE organization_id = ? AND operation = 'urgent_runner_update' AND json_extract(response_json, '$.request.id') = ? LIMIT 1", scope.organization, request.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var urgent urgentRunnerUpdate
	if err := json.Unmarshal([]byte(raw), &urgent); err != nil {
		return false, err
	}
	return urgent.Request != nil && request == *urgent.Request, nil
}

func (s *Service) getUrgentRunnerUpdate(c echo.Context) error {
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: c.Get("hub_api_credential").(apiCredential)}
	value, err := s.readUrgentRunnerUpdateCommand(c.Request().Context(), scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) readUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope) (any, error) {
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, _ time.Time) (any, error) {
		return readUrgentRunnerUpdate(ctx, tx, scope.organization)
	})
}

func (s *Service) markUrgentRunnerUpdate(c echo.Context) error {
	var request urgentRunnerUpdateChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: c.Get("hub_api_credential").(apiCredential)}
	value, err := s.markUrgentRunnerUpdateCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusAccepted, value)
}

func (s *Service) markUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope, request urgentRunnerUpdateChange) (any, error) {
	request.Version = strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if _, err := update.CompareVersions(request.Version, request.Version); err != nil || request.ExpectedRevision < 0 || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativeInvalid("A release version, expected revision and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &nativeError{Code: "confirmation_required", Message: "Confirm draining all runners below the selected release", status: http.StatusPreconditionRequired}
	}
	return s.runnerAdminTransaction(ctx, scope, false, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		const operation = "urgent_runner_update"
		hash, err := nativeCommandHash(request)
		if err != nil {
			return nil, err
		}
		raw, found, err := nativeCommandReceipt(ctx, tx, scope, operation, request.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return json.RawMessage(raw), nil
		}
		current, err := readUrgentRunnerUpdate(ctx, tx, scope.organization)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(tracker.Revision(current.Revision))
		}
		if current.Request != nil {
			comparison, err := update.CompareVersions(request.Version, current.Request.Version)
			if err != nil || comparison < 0 {
				return nil, nativeInvalid("An urgent release cannot lower the existing update target")
			}
		}
		value := urgentRunnerUpdate{Revision: current.Revision + 1, Request: &runnerauth.UpdateRequest{ID: newNativeID("update"), RequestedAt: now, Service: "detent", Version: request.Version, Release: true, Urgent: true}}
		if current.Request != nil && current.Request.Version == request.Version {
			value.Request = current.Request
		}
		if err := value.Request.Validate(); err != nil {
			return nil, nativeInvalid(err.Error())
		}
		raw, err = marshalNative(value)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET urgent_runner_update_json = ? WHERE id = ?", raw, scope.organization); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO native_commands (organization_id, actor_id, operation, command_key, request_hash, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)", scope.organization, scope.credential.ID, operation, request.IdempotencyKey, hash, raw, formatHubTime(now)); err != nil {
			return nil, err
		}
		return value, nil
	})
}
