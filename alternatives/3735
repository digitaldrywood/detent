package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) previewProviderCandidates(c echo.Context) error {
	var request tracker.NativeCapacityPreview
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.Limit < 0 || request.Limit > 100 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Candidate limit must be between 1 and 100"))
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Landing capacity must be non-negative"))
	}
	page, err := s.previewProviderCandidatesTenant(c.Request().Context(), hubscope.NativeRequestScope(c), request, s.config.Clock().UTC())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, page)
}

func (s *Service) previewProviderCandidatesTenant(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeCapacityPreview, now time.Time) (tracker.NativeCapacityPage, error) {
	ctx = s.withRequestDetails(ctx)
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	var candidates []claimstore.Candidate
	var plans []hubrecords.NativeClaimPlan
	var projectPolicy policystore.Revision
	var identity runnerstore.Identity
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return tracker.NativeCapacityPage{}, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		identity, err = r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if !runnerIdentityCanClaim(identity, scope, request.MachineID, now) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return tracker.ErrMachineNotFound
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !nativeClaimProjectEnabled(project) {
			return ErrNoClaimableWork
		}
		projectPolicy, err = r.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if projectPolicy.PolicyID != request.PolicyID {
			return hubapi.PolicyMismatch("Claim configuration changed; load the current project policy before previewing candidates")
		}
		query := nativeTenantCandidateQuery(scope, request.NativeClaim, "", now)
		query.Limit = limit
		if request.After > 0 {
			anchor, err := r.WorkItemByID(ctx, scope.Project, int64(request.After))
			if err != nil {
				return corestore.ErrInvalidCursor
			}
			query.AfterWorkItemID = anchor.NativeID
		}
		page, err := r.CandidatePage(ctx, query)
		if errors.Is(err, corestore.ErrInvalidCursor) && query.AfterWorkItemID != "" {
			return providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
		if err != nil {
			return err
		}
		candidates = page.Items
		shared := hubrecords.NativeClaimPlan{Project: project, Policy: projectPolicy}
		batchReader, canBatch := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if canBatch && request.SessionID == "" && len(candidates) > 0 {
			rows, err := batchReader.ClaimPreviewPlanRows(ctx, scope.Project, candidates)
			if err != nil {
				return err
			}
			if len(rows) != len(candidates) {
				return corestore.ErrRevisionConflict
			}
			for index, row := range rows {
				candidate := candidates[index]
				if errors.Is(row.Err, corestore.ErrNotFound) {
					s.reportIncompletePreviewCandidate(ctx, scope, candidate, row.Err)
					continue
				}
				if row.Err != nil {
					return row.Err
				}
				if row.Item.Revision != candidate.Revision || row.Item.Archived {
					continue
				}
				plan := nativeClaimPlanFromBatch(shared, candidate, row)
				if err := validateClaimSnapshotPolicy(scope, plan); err != nil {
					return err
				}
				plans = append(plans, plan)
			}
			if err := readNativeClaimChangeFacts(ctx, r, scope, plans); err != nil {
				return err
			}
			if err := readNativeClaimReleaseDeliveryFacts(ctx, r, scope, plans); err != nil {
				return err
			}
		} else {
			for _, candidate := range candidates {
				claim := request.NativeClaim
				claim.WorkItemID = candidate.WorkItemID
				plan, err := s.readNativeClaimPlanRows(ctx, scope, claim, now, r, &candidate, shared)
				if errors.Is(err, corestore.ErrNotFound) {
					s.reportIncompletePreviewCandidate(ctx, scope, candidate, err)
					continue
				}
				if errors.Is(err, ErrNoClaimableWork) {
					continue
				}
				if err != nil {
					return err
				}
				plans = append(plans, plan)
			}
		}
		return nil
	})
	if err != nil {
		return tracker.NativeCapacityPage{}, err
	}
	if err := s.prepareNativeClaimLegacyAuditFacts(ctx, scope, plans); err != nil {
		return tracker.NativeCapacityPage{}, err
	}
	if err := s.prepareNativeClaimApprovals(ctx, scope, plans); err != nil {
		return tracker.NativeCapacityPage{}, err
	}
	refs := []attachment.ArchiveReference{}
	if len(plans) > 0 {
		refs = append(refs, eventstore.Reference(projectPolicy.PayloadObjectKey, projectPolicy.PayloadSHA256, projectPolicy.PayloadBytes, projectPolicy.PayloadContentBytes))
	}
	for _, plan := range plans {
		v := plan.Version
		refs = append(refs, eventstore.Reference(v.PayloadObjectKey, v.PayloadSHA256, v.PayloadBytes, v.PayloadContentBytes))
		if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending {
			c := plan.ChangeRecord
			v := plan.ChangeVersion
			refs = append(refs, eventstore.Reference(c.PayloadObjectKey, c.PayloadSHA256, c.PayloadBytes, c.PayloadContentBytes), eventstore.Reference(v.PayloadObjectKey, v.PayloadSHA256, v.PayloadBytes, v.PayloadContentBytes))
			p := plan.ChangePolicy.Project
			refs = append(refs, eventstore.Reference(p.PayloadObjectKey, p.PayloadSHA256, p.PayloadBytes, p.PayloadContentBytes))
			if plan.ChangePolicy.HasReview {
				r := plan.ChangePolicy.Review
				refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
			}
		}
	}
	s.prefetchDetails(ctx, string(scope.Organization), refs)
	var descriptor *policy.Descriptor
	if len(plans) > 0 {
		loaded, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, projectPolicy)
		if err != nil {
			return tracker.NativeCapacityPage{}, err
		}
		descriptor = &loaded
		if err := loaded.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
			return tracker.NativeCapacityPage{}, &hubapi.NativeError{Code: "selector_no_match", Message: err.Error(), Status: http.StatusConflict}
		}
	}
	for index := range plans {
		claim := request.NativeClaim
		claim.WorkItemID = plans[index].Candidate.WorkItemID
		plan, err := s.hydrateNativeClaimPlan(ctx, scope, claim, now, plans[index], descriptor)
		if errors.Is(err, ErrNoClaimableWork) {
			plans[index] = hubrecords.NativeClaimPlan{}
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
		if plan.Candidate.WorkItemID == "" {
			continue
		}
		if !nativeClaimIssueContractAllowed(plan) || hubapi.StringsEqualFold(plan.Candidate.State, "Merging") && !plan.ChangeReady {
			continue
		}
		if plan.LandingPending && !plan.ChangeReady {
			continue
		}
		if plan.LandingRequired || request.LandingCapacity > 0 {
			if !landingCapacityChecked {
				landingCapacityAvailable, err = s.nativeClaimLandingCapacityAvailable(ctx, scope.Organization, scope.Project, request.LandingCapacity, now)
				if err != nil {
					return tracker.NativeCapacityPage{}, err
				}
				landingCapacityChecked = true
			}
			if !landingCapacityChecked || !landingCapacityAvailable {
				continue
			}
		}
		page.Items = append(page.Items, s.nativeIssueResponse(plan.Issue))
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

func (s *Service) reportIncompletePreviewCandidate(ctx context.Context, scope hubscope.NativeScope, candidate claimstore.Candidate, err error) {
	s.config.Logger.WarnContext(ctx, "skip incomplete preview candidate", "organization_id", scope.Organization, "project_id", scope.Project, "work_item_id", candidate.WorkItemID, "error", err)
}
