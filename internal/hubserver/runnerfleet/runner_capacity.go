package runnerfleet

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
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CapacityDeps struct {
	Repository        tenantstore.Repository
	Now               func() time.Time
	HTTPError         func(echo.Context, error, ...any) error
	InvalidRequest    func(echo.Context, error) error
	Details           func() attachment.DetailStore
	ReadRouting       func(context.Context, hubscope.NativeScope, string) (runnerauth.Runner, error)
	AuthorizeCapacity func(context.Context, tenantstore.Reader, hubscope.NativeScope) error
	PlansEnabled      func() bool
	Entitlement       func(context.Context, hubapi.NativeQueryer, time.Time) (hostedplans.HostedEntitlement, error)
}

type RunnerCapacityChange struct {
	runnerauth.CapacityRequest
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

func FreshCapacityConfig(r runnerauth.Runner, now time.Time) bool {
	return r.CapacityConfig != nil && !now.Before(r.CapacityConfig.ObservedAt) && now.Before(r.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) && !now.Before(r.LastHeartbeatAt) && now.Before(r.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout))
}

func (d CapacityDeps) PersistRunnerCapacityCommand(ctx context.Context, scope hubscope.NativeScope, resource string, request RunnerCapacityChange) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if request.ExpectedRevision < 1 || request.Validate() != nil || strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("A bounded capacity, configuration revision, runner revision and idempotency key are required")
	}
	if scope.Credential.Runner.RunnerID != "" {
		return nil, accessstore.ErrDenied
	}
	r, err := d.ReadRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	authorize := func(reader tenantstore.Reader) error { return d.AuthorizeCapacity(ctx, reader, scope) }
	operation := "runner_capacity " + resource
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	var receipt eventstore.Receipt
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := authorize(reader); err != nil {
			return err
		}
		var err error
		receipt, err = reader.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	content := eventstore.NewContent(d.Details())
	if receipt.CommandKey != "" {
		if receipt.RequestHash != hash {
			return nil, hubapi.NativeConflict(0)
		}
		return content.Receipt(ctx, receipt)
	}
	r.CapacityLimit, r.CapacityRequest, r.Revision = request.Capacity, &request.CapacityRequest, r.Revision+1
	view, err := d.RunnerCapacityView(ctx, nil, r, request.Backend, d.Now().UTC())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: d.Now()}, raw)
	if err != nil {
		return nil, err
	}
	err = d.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := authorize(writer); err != nil {
			return err
		}
		previous, err := writer.Receipt(ctx, scope.Credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
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
			return hubapi.NativeConflict(tracker.Revision(identity.Revision))
		}
		current := runnerauth.Runner{CapacityConfig: identity.CapacityConfig, LastHeartbeatAt: identity.LastHeartbeatAt}
		if !FreshCapacityConfig(current, d.Now().UTC()) || identity.CapacityConfig.Revision != request.ExpectedConfigRevision {
			return &hubapi.NativeError{Code: "revision_conflict", Message: "Current enrolled runner configuration evidence is required; read runner capacity again", Status: http.StatusConflict, PublicMessage: true}
		}
		identity.CapacityLimit, identity.CapacityRequest = int64(request.Capacity), &request.CapacityRequest
		if err := writer.UpdateRunnerRouting(ctx, request.ExpectedRevision, identity); err != nil {
			return err
		}
		receipt, err = writer.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return content.Receipt(ctx, receipt)
}

func (d CapacityDeps) UpdateRunnerCapacity(c echo.Context) error {
	var request RunnerCapacityChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return d.InvalidRequest(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
	if !ok {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	value, err := d.PersistRunnerCapacityCommand(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"), request)
	if err != nil {
		return d.HTTPError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, value)
}

func (d CapacityDeps) RunnerCapacityView(ctx context.Context, query hubapi.NativeQueryer, r runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
	view := r.CapacityView(backend, now)
	if d.PlansEnabled() {
		entitlement, err := d.Entitlement(ctx, query, now)
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
