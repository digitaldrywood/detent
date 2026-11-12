package runnerupdate

import (
	"context"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/update"
)

type UrgentUpdate struct {
	Revision int64                     `json:"revision"`
	Request  *runnerauth.UpdateRequest `json:"request"`
}

type UrgentChange struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Version          string `json:"version"`
	Confirm          bool   `json:"confirm"`
	IdempotencyKey   string `json:"idempotency_key"`
}

func ApplyUrgentRouting(r *runnerauth.Runner, urgent UrgentUpdate, version string) {
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
	if requestSettled(r, *urgent.Request) {
		r.UpdateRequest = urgent.Request
		return
	}
	r.UpdateRequest = urgent.Request
}

func requestSettled(r *runnerauth.Runner, request runnerauth.UpdateRequest) bool {
	if r.Update == nil || r.Update.Receipt == nil || !r.Update.Receipt.Request.Matches(request) {
		return false
	}
	receipt := r.Update.Receipt
	return receipt.Status == "refused" || receipt.Status == "uncertain" && receipt.FailureReason != ""
}

func (s *Service) ReadUrgent(ctx context.Context, scope hubapi.Scope) (any, error) {
	var value UrgentUpdate
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := scope.Authorize(ctx, reader, false); err != nil {
			return err
		}
		row, err := reader.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		value = UrgentUpdate{Revision: row.Revision, Request: row.Request}
		return nil
	})
	return value, err
}

func (s *Service) MarkUrgent(ctx context.Context, scope hubapi.Scope, request UrgentChange) (any, error) {
	request.Version = strings.TrimPrefix(strings.TrimSpace(request.Version), "v")
	if _, err := update.CompareVersions(request.Version, request.Version); err != nil || request.ExpectedRevision < 0 || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("A release version, expected revision and idempotency key are required")
	}
	if !request.Confirm {
		return nil, &hubapi.Error{Code: "confirmation_required", Message: "Confirm updating all runners below the selected release", Status: http.StatusPreconditionRequired, Public: true}
	}
	return s.markUrgent(ctx, scope, request)
}
