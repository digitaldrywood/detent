package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) previewProviderCandidates(c echo.Context) error {
	var request tracker.NativeCapacityPreview
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.Limit < 0 || request.Limit > 100 {
		return s.nativeAPIError(c, nativePublicInvalid("Candidate limit must be between 1 and 100"))
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, nativePublicInvalid("Landing capacity must be non-negative"))
	}
	if s.config.TenantRepository != nil {
		page, err := s.previewProviderCandidatesTenant(c.Request().Context(), nativeRequestScope(c), request, s.config.now().UTC())
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, page)
	}
	return s.runnerReadTransaction(c, func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error) {
		scope := nativeRequestScope(c)
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return nil, err
		}
		if err := authorizeClaimScope(ctx, tx, tracker.ClaimRequest{MachineID: request.MachineID}, &scope); err != nil {
			return nil, err
		}
		query := claimCandidateQuery{LandingCapacity: request.LandingCapacity, DispatchPriorityByState: request.DispatchPriorityByState, DispatchPriorityByLabel: request.DispatchPriorityByLabel, PrioritizeUnblockers: request.PrioritizeUnblockers, PolicyID: request.PolicyID, RequirePolicy: true, NativeScope: &scope, Scope: string(scope.project)}
		query.Limit, query.After, query.AvailableAt = request.Limit, request.After, now
		if _, err := validateClaimPolicy(ctx, tx, query, request.MachineID); err != nil {
			return nil, err
		}
		if scope.credential.Runner.RunnerID != "" {
			if err := validateRunnerDispatch(ctx, tx, scope, now); err != nil {
				return nil, err
			}
		}
		ids, err := claimCandidateIDs(ctx, tx, query, nil, nil, normalizedQueryStrings(request.WorkflowStates), normalizedQueryStrings(request.Authors), normalizedQueryStrings(request.Assignees), normalizedQueryStrings(request.LabelInclude), normalizedQueryStrings(request.LabelExclude), nil)
		if err != nil {
			return nil, err
		}
		page := tracker.NativeCapacityPage{Items: []tracker.NativeIssue{}}
		limit := request.Limit
		if limit == 0 {
			limit = 100
		}
		for _, id := range ids {
			lease, found, err := readUnreleasedLease(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if found && lease.session.ExpiresAt.After(now) {
				continue
			}
			claimable, _, err := nativeIssueContractClaimable(ctx, tx, scope, id)
			if err != nil {
				return nil, err
			}
			if !claimable {
				continue
			}
			ready, _, _, err := nativeLandingCandidateReady(ctx, tx, &scope, id, request.MachineID, request.LandingCapacity, now)
			if err != nil {
				return nil, err
			}
			if !ready {
				continue
			}
			var native string
			if err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id = ?", id).Scan(&native); err != nil {
				return nil, err
			}
			issue, _, err := readNativeIssue(ctx, tx, scope, native)
			if err != nil {
				return nil, err
			}
			page.Items = append(page.Items, s.nativeIssueResponse(issue))
		}
		if len(ids) == limit {
			page.Next = ids[len(ids)-1]
		}
		return page, nil
	})
}

func (s *Service) previewProviderCandidatesTenant(ctx context.Context, scope nativeScope, request tracker.NativeCapacityPreview, now time.Time) (tracker.NativeCapacityPage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	var candidates []claimstore.Candidate
	var plans []nativeClaimPlan
	var projectPolicy policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		if !runnerIdentityCanClaim(identity, scope, request.MachineID, now) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return tracker.ErrMachineNotFound
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return ErrNoClaimableWork
		}
		projectPolicy, err = r.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if projectPolicy.PolicyID != request.PolicyID {
			return policyMismatch("Claim configuration changed; load the current project policy before previewing candidates")
		}
		query := nativeTenantCandidateQuery(scope, request.NativeClaim, "", now)
		query.Limit = limit
		if request.After > 0 {
			anchor, err := r.WorkItemByID(ctx, scope.project, int64(request.After))
			if err != nil {
				return corestore.ErrInvalidCursor
			}
			query.AfterWorkItemID = anchor.NativeID
		}
		page, err := r.CandidatePage(ctx, query)
		if err != nil {
			return err
		}
		candidates = page.Items
		shared := nativeClaimPlan{project: project, policy: projectPolicy}
		for _, candidate := range candidates {
			claim := request.NativeClaim
			claim.WorkItemID = candidate.WorkItemID
			plan, err := s.readNativeClaimPlanRows(ctx, scope, claim, now, r, &candidate, shared)
			if errors.Is(err, ErrNoClaimableWork) {
				continue
			}
			if err != nil {
				return err
			}
			plans = append(plans, plan)
		}
		return nil
	})
	if err != nil {
		return tracker.NativeCapacityPage{}, err
	}
	var descriptor *policy.Descriptor
	if len(plans) > 0 {
		loaded, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, projectPolicy)
		if err != nil {
			return tracker.NativeCapacityPage{}, err
		}
		descriptor = &loaded
	}
	for index := range plans {
		claim := request.NativeClaim
		claim.WorkItemID = plans[index].candidate.WorkItemID
		plan, err := s.hydrateNativeClaimPlan(ctx, scope, claim, now, plans[index], descriptor)
		if errors.Is(err, ErrNoClaimableWork) {
			plans[index] = nativeClaimPlan{}
			continue
		}
		if err != nil {
			return tracker.NativeCapacityPage{}, err
		}
		plans[index] = plan
	}
	landingCapacityChecked := false
	landingCapacityAvailable := false
	page := tracker.NativeCapacityPage{Items: []tracker.NativeIssue{}}
	for _, plan := range plans {
		if plan.candidate.WorkItemID == "" {
			continue
		}
		if !nativeClaimIssueContractAllowed(plan) || stringsEqualFold(plan.candidate.State, "Merging") && !plan.changeReady {
			continue
		}
		if plan.landingPending && !plan.changeReady {
			continue
		}
		if plan.landingRequired || request.LandingCapacity > 0 {
			if !landingCapacityChecked {
				landingCapacityAvailable, err = s.nativeClaimLandingCapacityAvailable(ctx, scope.organization, scope.project, request.LandingCapacity, now)
				if err != nil {
					return tracker.NativeCapacityPage{}, err
				}
				landingCapacityChecked = true
			}
			if !landingCapacityChecked || !landingCapacityAvailable {
				continue
			}
		}
		page.Items = append(page.Items, s.nativeIssueResponse(plan.issue))
	}
	if len(candidates) == limit {
		page.Next = tracker.WorkItemID(candidates[len(candidates)-1].ID)
	}
	return page, nil
}

func (s *Service) nativeClaimLandingCapacityAvailable(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, requested int, now time.Time) (bool, error) {
	allowed := false
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		usage, err := r.LandingCapacityUsage(ctx, project, now)
		if err != nil {
			return err
		}
		capacity := int64(requested)
		if usage.Limit > 0 && (capacity <= 0 || usage.Limit < capacity) {
			capacity = usage.Limit
		}
		allowed = !usage.Unknown && (capacity <= 0 || usage.Active < capacity)
		return nil
	})
	return allowed, err
}
