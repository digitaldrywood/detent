package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
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

func applyUrgentRunnerRouting(r *runnerauth.Runner, urgent urgentRunnerUpdate, version string) {
	if urgent.Request == nil {
		return
	}
	if r.UpdateRequest != nil {
		comparison, err := update.CompareVersions(urgent.Request.Version, r.UpdateRequest.Version)
		if err == nil && comparison < 0 {
			return
		}
	}
	if r.Update != nil {
		version = r.Update.Running.Version
	}
	comparison, err := update.CompareVersions(urgent.Request.Version, version)
	if err != nil || comparison <= 0 {
		if r.Update != nil && r.Update.Receipt != nil && r.Update.Receipt.Request.Matches(*urgent.Request) {
			r.UpdateRequest = urgent.Request
		}
		return
	}
	if runnerUpdateRequestSettled(r, *urgent.Request) {
		r.UpdateRequest = urgent.Request
		return
	}
	r.UpdateRequest = urgent.Request
}

func runnerUpdateRequestSettled(r *runnerauth.Runner, request runnerauth.UpdateRequest) bool {
	if r.Update == nil || r.Update.Receipt == nil || !r.Update.Receipt.Request.Matches(request) {
		return false
	}
	receipt := r.Update.Receipt
	return receipt.Status == "refused" || receipt.Status == "uncertain" && receipt.FailureReason != ""
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

func (s *Service) readUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope) (any, error) {
	var value urgentRunnerUpdate
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, reader, scope, false); err != nil {
			return err
		}
		row, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		value = urgentRunnerUpdate{Revision: row.Revision, Request: row.Request}
		return nil
	})
	return value, err
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

func (s *Service) markUrgentRunnerUpdateCommand(ctx context.Context, scope nativeScope, request urgentRunnerUpdateChange) (any, error) {
	request.Version = strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if _, err := update.CompareVersions(request.Version, request.Version); err != nil || request.ExpectedRevision < 0 || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("A release version, expected revision and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &nativeError{Code: "confirmation_required", Message: "Confirm updating all runners below the selected release", status: http.StatusPreconditionRequired, publicMessage: true}
	}
	return s.markUrgentRunnerUpdateTenant(ctx, scope, request)
}
