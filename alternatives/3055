package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerCapacityChange struct {
	runnerauth.CapacityRequest
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

func freshCapacityConfig(r runnerauth.Runner, now time.Time) bool {
	return r.CapacityConfig != nil && !now.Before(r.CapacityConfig.ObservedAt) && now.Before(r.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) && !now.Before(r.LastHeartbeatAt) && now.Before(r.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout))
}

func (s *Service) authorizeRunnerCapacity(ctx context.Context, reader tenantstore.Reader, scope nativeScope) error {
	if scope.credential.Runner.RunnerID != "" {
		return accessstore.ErrDenied
	}
	if scope.credential.Hosted == nil {
		current, err := reader.AccessCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		if current.Scope != string(apiScopeAdmin) || current.NativeOnly {
			return accessstore.ErrDenied
		}
	} else {
		scope.credential.ManageRunners = true
	}
	return s.authorizeAccessScope(ctx, reader, scope, true)
}

func (s *Service) readRunnerCapacity(ctx context.Context, scope nativeScope, resource, backend string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if backend != "" && (runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("0", 64), Capacity: 1, Backend: backend}).Validate() != nil {
		return nil, nativePublicInvalid("Backend must be a bounded provider identifier")
	}
	if scope.credential.Runner.RunnerID != "" {
		return nil, accessstore.ErrDenied
	}
	r, err := s.readRunnerRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	now := s.config.now().UTC()
	var recommendation runnerRecommendation
	to := now.Truncate(time.Hour)
	from := to.Add(-719 * time.Hour)
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.authorizeRunnerCapacity(ctx, reader, scope); err != nil {
			return err
		}
		segments, err := reader.RunnerHostMetrics(ctx, resource, from, to)
		if err != nil {
			return err
		}
		hours, err := recommendationHoursFromSegments(segments)
		if err != nil {
			return err
		}
		recommendation, err = recommendRunner(r.Routing, hours, nil)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	recommendation.From, recommendation.To, recommendation.Partial = from, to, true
	recommendation.Suggested = suggestedLimit{Current: r.CapacityLimit, Direction: "insufficient_evidence", Reasons: []string{"Historical runner occupancy and process usage are unavailable."}}
	for i := range recommendation.Periods {
		recommendation.Periods[i].Status = "insufficient_evidence"
		recommendation.Periods[i].Slots = nil
	}
	view, err := s.runnerCapacityView(ctx, nil, r, backend, now)
	if err != nil {
		return nil, err
	}
	return runnerCapacityRead{CapacityView: view, Recommendation: recommendation, ProviderCapacity: r.ProviderCapacity}, nil
}

func (s *Service) updateRunnerCapacityCommand(ctx context.Context, scope nativeScope, resource string, request runnerCapacityChange) (any, error) {
	return s.persistRunnerCapacityCommand(ctx, scope, resource, request)
}

func (s *Service) persistRunnerCapacityCommand(ctx context.Context, scope nativeScope, resource string, request runnerCapacityChange) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if request.ExpectedRevision < 1 || request.Validate() != nil || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("A bounded capacity, configuration revision, runner revision and idempotency key are required")
	}
	if scope.credential.Runner.RunnerID != "" {
		return nil, accessstore.ErrDenied
	}
	r, err := s.readRunnerRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	authorize := func(reader tenantstore.Reader) error { return s.authorizeRunnerCapacity(ctx, reader, scope) }
	operation := "runner_capacity " + resource
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	var receipt eventstore.Receipt
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := authorize(reader); err != nil {
			return err
		}
		var err error
		receipt, err = reader.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if receipt.CommandKey != "" {
		if receipt.RequestHash != hash {
			return nil, nativeConflict(0)
		}
		return content.Receipt(ctx, receipt)
	}
	r.CapacityLimit, r.CapacityRequest, r.Revision = request.Capacity, &request.CapacityRequest, r.Revision+1
	view, err := s.runnerCapacityView(ctx, nil, r, request.Backend, s.config.now().UTC())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := authorize(writer); err != nil {
			return err
		}
		previous, err := writer.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			receipt = previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		identity, err := writer.RunnerIdentity(ctx, resource)
		if err != nil {
			return err
		}
		if identity.Revision != request.ExpectedRevision || identity.Revision != r.Revision-1 {
			return nativeConflict(tracker.Revision(identity.Revision))
		}
		current := runnerauth.Runner{CapacityConfig: identity.CapacityConfig, LastHeartbeatAt: identity.LastHeartbeatAt}
		if !freshCapacityConfig(current, s.config.now().UTC()) || identity.CapacityConfig.Revision != request.ExpectedConfigRevision {
			return &nativeError{Code: "revision_conflict", Message: "Current enrolled runner configuration evidence is required; read runner capacity again", status: http.StatusConflict, publicMessage: true}
		}
		identity.CapacityLimit, identity.CapacityRequest = int64(request.Capacity), &request.CapacityRequest
		if err := writer.UpdateRunnerRouting(ctx, request.ExpectedRevision, identity); err != nil {
			return err
		}
		receipt, err = writer.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	return content.Receipt(ctx, receipt)
}

func (s *Service) getRunnerCapacity(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.readRunnerCapacity(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), c.QueryParam("backend"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) updateRunnerCapacity(c echo.Context) error {
	var request runnerCapacityChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok {
		return s.nativeAPIError(c, nativeNotFound())
	}
	value, err := s.updateRunnerCapacityCommand(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (s *Service) runnerCapacityView(ctx context.Context, query nativeQueryer, r runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
	view := r.CapacityView(backend, now)
	if s.database.plans() != nil {
		entitlement, err := s.database.hostedEntitlement(ctx, query, now)
		if err != nil {
			return view, err
		}
		if !slices.Contains(entitlement.Features, "native_execution") {
			zero := 0
			view.Limits = append(view.Limits, runnerauth.CapacityLimit{Owner: "plan", Limit: &zero, Constraint: "The current plan does not permit native execution.", ObservedAt: now})
			view.Effective = &zero
			view.Status = "partially_applied"
		}
	}
	view.Projects = make(map[tracker.ProjectID]runnerauth.ProjectCapacityView)
	for project := range r.ProjectRanks {
		if !r.AllowsProject(project) {
			continue
		}
		capacity := runnerauth.ProjectCapacityView{Used: r.ProjectUsed[project]}
		if limit, configured := r.ProjectCapacityLimits[project]; configured {
			capacity.Desired, capacity.Applied = &limit, &limit
			if limit == 0 {
				capacity.Effective = &limit
			}
		}
		if view.Effective != nil {
			effective := *view.Effective
			if capacity.Applied != nil {
				effective = min(effective, *capacity.Applied)
			}
			capacity.Effective = &effective
		}
		view.Projects[project] = capacity
	}
	return view, nil
}
