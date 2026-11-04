package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeClaimPlan struct {
	candidate            claimstore.Candidate
	project              projectstore.Record
	policy               policystore.Revision
	descriptor           policy.Descriptor
	item                 corestore.WorkItem
	issue                tracker.NativeIssue
	version              eventstore.CollaborationVersion
	linked               *tracker.LinkedIssueSource
	linkedRecord         *eventstore.LinkedSource
	change               tracker.NativeChangeReference
	changeReady          bool
	changeRecord         eventstore.Change
	changePolicy         currentChangePolicyRows
	changeVersion        eventstore.Version
	changePolicyRevision policystore.Revision
	latestAttempt        leasestore.Attempt
	latestRunEvent       eventstore.Event
	latestAttemptFound   bool
	latestRunEventFound  bool
	landingPending       bool
	landingRequired      bool
	replay               *leasestore.Lease
}

const nativeClaimCandidatePageSize = 100

type nativeClaimCandidateScan struct {
	candidates []claimstore.Candidate
	pageNext   claimstore.CandidateCursor
	cursor     claimstore.SelectionCursor
}

func (s *Service) claimNativeIssueTenant(ctx context.Context, scope nativeScope, request tracker.NativeClaim, ttl time.Duration) (leasestore.Lease, error) {
	if s.config.TenantRepository == nil || s.config.NativeRunEvents == nil || scope.credential.Runner.RunnerID == "" {
		return leasestore.Lease{}, nativeRunEventStorageUnavailable()
	}
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 1 {
		request.WorkItemID = request.ProviderCandidates[0].WorkItemID
	}
	if request.WorkItemID != "" && len(request.ProviderCandidates) > 0 && !slices.ContainsFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool {
		return candidate.WorkItemID == request.WorkItemID
	}) {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	if s.runDetailStore() == nil {
		return leasestore.Lease{}, nativeRunEventStorageUnavailable()
	}
	now := s.config.now().UTC()
	if request.WorkItemID == "" && len(request.ProviderCandidates) == 0 {
		firstPlan, firstPlanErr := s.readNativeClaimPlan(ctx, scope, request, now)
		if firstPlanErr != nil && !errors.Is(firstPlanErr, ErrNoClaimableWork) {
			return leasestore.Lease{}, firstPlanErr
		}
		firstPlanValid := firstPlanErr == nil
		if firstPlanValid && firstPlan.replay != nil {
			return s.claimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, firstPlan, nil)
		}
		scan, err := s.readNativeClaimCandidates(ctx, scope, request, now)
		if err != nil {
			return leasestore.Lease{}, err
		}
		var contractRefusal error
		position := scan.cursor.Position
		for _, candidate := range scan.candidates {
			var plan nativeClaimPlan
			if firstPlanValid && candidate.WorkItemID == firstPlan.candidate.WorkItemID {
				plan = firstPlan
			} else {
				candidateRequest := request
				candidateRequest.WorkItemID = candidate.WorkItemID
				plan, err = s.readNativeClaimPlan(ctx, scope, candidateRequest, now)
				if errors.Is(err, ErrNoClaimableWork) {
					continue
				}
				if err != nil {
					return leasestore.Lease{}, err
				}
			}
			if !nativeClaimIssueContractAllowed(plan) {
				contractRefusal = &nativeError{Code: "issue_contract_unsatisfied", Message: "Current issue contract does not allow dispatch", status: http.StatusConflict}
				position = claimCandidateCursor(candidate)
				continue
			}
			if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && !plan.changeReady {
				position = claimCandidateCursor(candidate)
				continue
			}
			candidateRequest := request
			candidateRequest.WorkItemID = candidate.WorkItemID
			scan.cursor.Position = position
			return s.claimNativeIssueTenantWithPlan(ctx, scope, candidateRequest, ttl, now, plan, &scan.cursor)
		}
		scan.cursor.Position = scan.pageNext
		if err := s.persistNativeClaimCursor(ctx, scope, scan.cursor); err != nil {
			return leasestore.Lease{}, err
		}
		if contractRefusal != nil {
			return leasestore.Lease{}, contractRefusal
		}
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	plan, err := s.readNativeClaimPlan(ctx, scope, request, now)
	if err != nil {
		return leasestore.Lease{}, err
	}
	return s.claimNativeIssueTenantWithPlan(ctx, scope, request, ttl, now, plan, nil)
}

func (s *Service) readNativeClaimCandidates(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimCandidateScan, error) {
	var scan nativeClaimCandidateScan
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return ErrNoClaimableWork
		}
		policy, err := r.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if policy.PolicyID != request.PolicyID {
			return policyMismatch("Claim configuration changed; load the current project policy before claiming")
		}
		query := nativeTenantCandidateQuery(scope, request, "", now)
		sourceRevision, err := nativeClaimSelectionSourceRevision(project, policy, query)
		if err != nil {
			return err
		}
		cursor, err := r.SelectionCursor(ctx, scope.credential.Runner.RunnerID, scope.project)
		if errors.Is(err, corestore.ErrNotFound) {
			cursor = claimstore.SelectionCursor{OrganizationID: scope.organization, RunnerID: scope.credential.Runner.RunnerID, ProjectID: scope.project, SourceRevision: sourceRevision}
		} else if err != nil {
			return err
		} else if cursor.SourceRevision != sourceRevision {
			cursor.SourceRevision = sourceRevision
			cursor.Position = claimstore.CandidateCursor{}
		} else if cursor.Position.WorkItemID != "" {
			anchorQuery := query
			anchorQuery.WorkItemID = cursor.Position.WorkItemID
			anchorQuery.IncludeLeasedAnchor = true
			anchorQuery.Limit = 1
			anchor, err := r.CandidatePage(ctx, anchorQuery)
			if errors.Is(err, corestore.ErrNotFound) || err == nil && (len(anchor.Items) != 1 || claimCandidateCursor(anchor.Items[0]) != cursor.Position) {
				cursor.Position = claimstore.CandidateCursor{}
			} else if err != nil {
				return err
			}
		}
		query.Limit = nativeClaimCandidatePageSize
		query.After = cursor.Position
		page, err := r.CandidatePage(ctx, query)
		if err != nil {
			return err
		}
		scan = nativeClaimCandidateScan{candidates: page.Items, pageNext: page.Next, cursor: cursor}
		return nil
	})
	return scan, err
}

func (s *Service) claimNativeIssueTenantWithPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, ttl time.Duration, now time.Time, plan nativeClaimPlan, cursor *claimstore.SelectionCursor) (leasestore.Lease, error) {
	if !nativeClaimIssueContractAllowed(plan) {
		return leasestore.Lease{}, &nativeError{Code: "issue_contract_unsatisfied", Message: "Current issue contract does not allow dispatch", status: http.StatusConflict}
	}
	if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && !plan.changeReady {
		return leasestore.Lease{}, ErrNoClaimableWork
	}
	if len(request.ProviderCandidates) > 0 && !slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) {
		candidate := slices.IndexFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool {
			return candidate.WorkItemID == plan.candidate.WorkItemID
		})
		if candidate < 0 || int64(request.ProviderCandidates[candidate].Revision) != plan.candidate.Revision {
			return leasestore.Lease{}, providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
	}
	var lease leasestore.Lease
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.now().UTC()
		if !runnerIdentityCanClaim(identity, scope, request.MachineID, authoritativeNow) {
			return tracker.ErrMachineNotFound
		}
		if err := plan.descriptor.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
			return policyMismatch(err.Error())
		}
		if identity.State != "active" || identity.CapacityConfig == nil || !authoritativeNow.Before(identity.CapacityConfig.ObservedAt.Add(runnerauth.HeartbeatTimeout)) || authoritativeNow.Before(identity.CapacityConfig.ObservedAt) {
			return ErrNoClaimableWork
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, authoritativeNow)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNoClaimableWork
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if request.LandingCapacity > 0 || plan.landingRequired {
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return ErrNoClaimableWork
		}
		item, err := w.WorkItemByID(ctx, scope.project, plan.candidate.ID)
		if err != nil {
			return err
		}
		if item.NativeID != plan.candidate.WorkItemID || item.Archived || plan.replay == nil && (item.Revision != plan.candidate.Revision || item.DispatchGeneration != plan.item.DispatchGeneration) {
			return ErrNoClaimableWork
		}
		currentLinked, err := w.LinkedSource(ctx, scope.project, item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && (plan.linkedRecord == nil || !sameNativeClaimLinkedSource(currentLinked, *plan.linkedRecord)) || errors.Is(err, corestore.ErrNotFound) && plan.linkedRecord != nil {
			return corestore.ErrRevisionConflict
		}
		workspaceItem, err := w.IsWorkspaceWorkItem(ctx, scope.project, item.NativeID)
		if err != nil {
			return err
		}
		workspaceLane := slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability)
		if workspaceItem && !workspaceLane || !workspaceItem && slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability) && len(request.ProviderCandidates) == 0 {
			return ErrNoClaimableWork
		}
		if plan.replay == nil {
			query := nativeTenantCandidateQuery(scope, request, plan.candidate.WorkItemID, authoritativeNow)
			page, err := w.CandidatePage(ctx, query)
			if err != nil {
				return err
			}
			if len(page.Items) != 1 || page.Items[0].Revision != plan.candidate.Revision {
				return ErrNoClaimableWork
			}
		}
		item, err = w.LockWorkItem(ctx, scope.project, item.NativeID)
		if err != nil {
			return err
		}
		if item.Archived || plan.replay == nil && (item.Revision != plan.candidate.Revision || item.DispatchGeneration != plan.item.DispatchGeneration) {
			return corestore.ErrRevisionConflict
		}
		if plan.replay == nil {
			latest, err := w.LatestAttempt(ctx, item.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			current := plan
			current.item = item
			current.latestAttempt, current.latestAttemptFound = latest, err == nil
			if nativeClaimDispatchSatisfied(current) {
				return ErrNoClaimableWork
			}
		}
		if stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending {
			latest, err := w.LatestChange(ctx, scope.project, plan.candidate.WorkItemID)
			if err != nil || latest.ID != plan.change.ChangeID || latest.CurrentVersionID == nil || *latest.CurrentVersionID != plan.change.VersionID {
				if err != nil {
					return err
				}
				return corestore.ErrRevisionConflict
			}
			current, err := w.LockChange(ctx, plan.changeRecord.ID)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangeRecord(current, plan.changeRecord) {
				return corestore.ErrRevisionConflict
			}
			version, err := w.Version(ctx, current.ID, plan.change.VersionID)
			if err != nil {
				return err
			}
			if version.PolicyRevisionID != plan.changeVersion.PolicyRevisionID || version.HeadSHA != plan.changeVersion.HeadSHA {
				return corestore.ErrRevisionConflict
			}
			versionPolicy, err := w.ProjectPolicyRevision(ctx, scope.project, version.PolicyRevisionID)
			if err != nil {
				return err
			}
			if !sameNativeClaimPolicyRevision(versionPolicy, plan.changePolicyRevision) {
				return corestore.ErrRevisionConflict
			}
			currentPolicy, err := readCurrentChangePolicyRows(ctx, w, scope)
			if err != nil {
				return err
			}
			if !sameNativeClaimChangePolicyRows(currentPolicy, plan.changePolicy) {
				return corestore.ErrRevisionConflict
			}
			if err := verifyNativeClaimLandingEvent(ctx, w, item.ID, plan); err != nil {
				return err
			}
		}
		if plan.replay == nil && (plan.landingRequired || request.LandingCapacity > 0) {
			usage, err := w.LandingCapacityUsage(ctx, scope.project, authoritativeNow)
			if err != nil {
				return err
			}
			capacity := int64(request.LandingCapacity)
			if usage.Limit > 0 && (capacity <= 0 || usage.Limit < capacity) {
				capacity = usage.Limit
			}
			if usage.Unknown || capacity > 0 && usage.Active >= capacity {
				return ErrNoClaimableWork
			}
		}
		if plan.replay == nil {
			capacity := min(identity.CapacityLimit, identity.ReportedCapacity, int64(identity.CapacityConfig.LocalLimit), int64(identity.CapacityConfig.ClientLimit), int64(identity.CapacityConfig.RuntimeLimit))
			projectCapacity, projectCapacityConfigured := identity.ProjectCapacityLimits[scope.project]
			if projectCapacityConfigured {
				capacity = min(capacity, int64(projectCapacity))
			}
			availability := runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: identity.Availability.Windows, WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline}
			availableCapacity, err := availability.Capacity(authoritativeNow, int(capacity))
			if err != nil {
				return err
			}
			if availableCapacity <= 0 {
				return ErrNoClaimableWork
			}
			machine, err := w.LockMachine(ctx, string(identity.MachineID))
			if err != nil {
				return err
			}
			machineUsed, err := w.Usage(ctx, machine.ID)
			if err != nil {
				return err
			}
			if machine.Capacity <= 0 || machineUsed >= machine.Capacity {
				return ErrNoClaimableWork
			}
			runnerUsed, projectUsed, err := w.RunnerProjectUsage(ctx, identity.RunnerID, scope.project, authoritativeNow)
			if err != nil {
				return err
			}
			if runnerUsed >= int64(availableCapacity) || projectCapacityConfigured && projectUsed >= int64(projectCapacity) {
				return ErrNoClaimableWork
			}
		}
		var selectedReservation *providercapacity.Reservation
		if !workspaceItem && plan.replay == nil && len(request.ProviderCandidates) > 0 {
			reservation, err := selectTenantProviderReservation(ctx, w, identity, request, item.NativeID, authoritativeNow)
			if err != nil {
				return err
			}
			selectedReservation = &reservation
		}
		leaseID := newNativeID("lease")
		lease, err = w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(identity.MachineID), SessionID: request.SessionID, LeaseID: leaseID, TTL: ttl, LandingCapacity: int64(request.LandingCapacity)})
		if err != nil {
			return err
		}
		currentPolicy, err := w.ProjectPolicy(ctx, scope.project)
		if err != nil {
			return err
		}
		if !sameNativeClaimPolicyRevision(currentPolicy, plan.policy) || currentPolicy.ApprovedBy != plan.policy.ApprovedBy || !currentPolicy.ApprovedAt.Equal(plan.policy.ApprovedAt) {
			return corestore.ErrRevisionConflict
		}
		if lease.LeaseID != leaseID {
			owner, err := w.RunnerForLease(ctx, lease.LeaseID)
			if err != nil {
				return err
			}
			pinned, err := w.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
			if err != nil {
				return err
			}
			if owner.RunnerID != identity.RunnerID || pinned.PolicyID != plan.policy.PolicyID {
				return tracker.ErrLeaseConflict
			}
			reservation, foundReservation, err := readTenantProviderReservation(ctx, w, tracker.LeaseID(lease.LeaseID))
			if err != nil {
				return err
			}
			if !workspaceItem && (len(request.ProviderCandidates) > 0 || slices.Contains(request.Capabilities, tracker.NativeProviderCapacityCapability)) {
				candidate := nativeClaimProviderCandidate(request, item.NativeID)
				if !foundReservation || candidate == nil || reservation.Reservation.Requirement != candidate.Requirement {
					return tracker.ErrLeaseConflict
				}
			}
		} else {
			if err := w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: scope.organization, LeaseID: lease.LeaseID, RunnerID: identity.RunnerID, AssignedAt: authoritativeNow}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(ctx, policystore.LeasePolicy{OrganizationID: scope.organization, ProjectID: scope.project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: plan.policy.PolicyID, BoundAt: authoritativeNow}); err != nil {
				return err
			}
			if selectedReservation != nil {
				row := claimstore.ProviderReservation{OrganizationID: scope.organization, LeaseID: tracker.LeaseID(lease.LeaseID), Reservation: *selectedReservation}
				if err := w.CreateProviderReservation(ctx, row); err != nil {
					return err
				}
			}
		}
		organizationBudget, err := w.BillingMonthlyPolicy(ctx, "")
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		projectBudget, err := w.BillingMonthlyPolicy(ctx, scope.project)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if organizationBudget.Policy.Enabled || projectBudget.Policy.Enabled {
			return ErrBillingBudgetUnavailable
		}
		if cursor != nil {
			written, err := w.PutSelectionCursor(ctx, *cursor, cursor.Revision)
			if err != nil {
				return err
			}
			if !written {
				return ErrNoClaimableWork
			}
		}
		return nil
	})
	return lease, err
}

func (s *Service) persistNativeClaimCursor(ctx context.Context, scope nativeScope, cursor claimstore.SelectionCursor) error {
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		identity, err := w.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		authoritativeNow := s.config.now().UTC()
		if !runnerIdentityCanClaim(identity, scope, scope.credential.Runner.MachineID, authoritativeNow) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, authoritativeNow)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNoClaimableWork
		}
		written, err := w.PutSelectionCursor(ctx, cursor, cursor.Revision)
		if err != nil {
			return err
		}
		if !written {
			return ErrNoClaimableWork
		}
		return nil
	})
}

func claimCandidateCursor(candidate claimstore.Candidate) claimstore.CandidateCursor {
	return claimstore.CandidateCursor{Priority: candidate.Priority, StateRank: candidate.StateRank, LabelRank: candidate.LabelRank, AgingBoost: candidate.AgingBoost, LandingRank: candidate.LandingRank, LaneRank: candidate.LaneRank, UnblockerRank: candidate.UnblockerRank, Rank: candidate.Rank, Created: candidate.CreatedAt, WorkItemID: candidate.WorkItemID, SourceRevision: candidate.Revision}
}

func nativeClaimSelectionSourceRevision(project projectstore.Record, policy policystore.Revision, query claimstore.CandidateQuery) (string, error) {
	query.AvailableAt = time.Time{}
	query.After = claimstore.CandidateCursor{}
	query.AfterWorkItemID = ""
	query.Limit = 0
	query.IncludeLeasedAnchor = false
	return nativeCommandHash(struct {
		WorkflowSourceRevision string
		PolicyID               string
		PolicySHA256           string
		Query                  claimstore.CandidateQuery
	}{project.Core.WorkflowSourceRevision, policy.PolicyID, policy.PayloadSHA256, query})
}

func selectTenantProviderReservation(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) (providercapacity.Reservation, error) {
	candidate := nativeClaimProviderCandidate(request, item)
	if candidate == nil || candidate.Revision <= 0 || candidate.Requirement.Validate() != nil {
		return providercapacity.Reservation{}, providerWait("provider_candidate_changed", "Work item needs a current local provider selection before it can be claimed")
	}
	reports, err := w.RunnerProviderReports(ctx, identity.RunnerID)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	var selected *providercapacity.Report
	for index := range reports {
		if reports[index].Supports(candidate.Requirement) {
			selected = &reports[index]
			break
		}
	}
	if selected == nil {
		return providercapacity.Reservation{}, providerWait("provider_incompatible", "Runner does not advertise the selected backend and model")
	}
	capacity, err := w.LockRunnerProviderAccount(ctx, runnerstore.ProviderAccountKey{RunnerID: identity.RunnerID, Provider: selected.Provider, Backend: selected.Backend, AccountAlias: selected.AccountAlias, SharedAccountAlias: selected.SharedAccountAlias}, now)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	if !capacity.SelectedReport {
		return providercapacity.Reservation{}, providerWait("provider_incompatible", "Runner no longer advertises the selected backend and model")
	}
	used, err := w.ProviderReservationUsage(ctx, *selected, identity.RunnerID, now)
	if err != nil {
		return providercapacity.Reservation{}, err
	}
	if capacity.MaxConcurrent > 0 && used >= capacity.MaxConcurrent {
		return providercapacity.Reservation{}, providerWait("provider_capacity", "Shared provider concurrency is fully reserved")
	}
	return providercapacity.Reservation{Requirement: candidate.Requirement, Report: *selected, Reason: "Current provider account capacity is available"}, nil
}

func nativeClaimProviderCandidate(request tracker.NativeClaim, item tracker.NativeWorkItemID) *tracker.NativeCapacityCandidate {
	for index := range request.ProviderCandidates {
		if request.ProviderCandidates[index].WorkItemID == item {
			return &request.ProviderCandidates[index]
		}
	}
	return nil
}

func readTenantProviderReservation(ctx context.Context, r claimstore.Reader, lease tracker.LeaseID) (claimstore.ProviderReservation, bool, error) {
	reservation, err := r.ProviderReservation(ctx, lease)
	if errors.Is(err, corestore.ErrNotFound) {
		return claimstore.ProviderReservation{}, false, nil
	}
	if err != nil {
		return claimstore.ProviderReservation{}, false, err
	}
	return reservation, true, nil
}

func (s *Service) readNativeClaimPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time) (nativeClaimPlan, error) {
	plan, err := s.readNativeClaimPlanRows(ctx, scope, request, now, nil, nil, nativeClaimPlan{})
	if err != nil {
		return nativeClaimPlan{}, err
	}
	return s.hydrateNativeClaimPlan(ctx, scope, request, now, plan, nil)
}

func (s *Service) readNativeClaimPlanRows(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time, reader tenantstore.Reader, candidate *claimstore.Candidate, shared nativeClaimPlan) (nativeClaimPlan, error) {
	plan := shared
	readRows := func(r tenantstore.Reader) error {
		var err error
		if plan.project.Core.ID == "" {
			plan.project, err = r.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if plan.project.Core.Profile != "native" || plan.project.Core.DeletedAt != nil {
				return ErrNoClaimableWork
			}
			plan.policy, err = r.ProjectPolicy(ctx, scope.project)
			if err != nil {
				return err
			}
			if plan.policy.PolicyID != request.PolicyID {
				return policyMismatch("Claim configuration changed; load the current project policy before claiming")
			}
		}
		if request.SessionID != "" {
			lease, err := r.LeaseBySession(ctx, request.SessionID)
			if err == nil {
				if lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || lease.MachineID != string(request.MachineID) {
					return tracker.ErrLeaseConflict
				}
				item, err := r.WorkItemByID(ctx, scope.project, lease.IssueID)
				if err != nil {
					return err
				}
				if request.WorkItemID != "" && request.WorkItemID != item.NativeID {
					return tracker.ErrLeaseConflict
				}
				if len(request.ProviderCandidates) > 0 {
					candidate := slices.IndexFunc(request.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool { return candidate.WorkItemID == item.NativeID })
					if candidate < 0 || int64(request.ProviderCandidates[candidate].Revision) != item.Revision {
						return tracker.ErrLeaseConflict
					}
				}
				plan.replay = &lease
				plan.candidate = claimstore.Candidate{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: item.ID, Revision: item.Revision, State: valueOrEmpty(item.WorkflowStateName), CreatedAt: item.NativeCreatedAt}
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		if candidate != nil {
			if plan.replay != nil && plan.candidate.WorkItemID != candidate.WorkItemID {
				return tracker.ErrLeaseConflict
			}
			if plan.replay == nil {
				plan.candidate = *candidate
			}
		}
		if candidate == nil && plan.replay == nil {
			page, err := r.CandidatePage(ctx, nativeTenantCandidateQuery(scope, request, request.WorkItemID, now))
			if err != nil {
				return err
			}
			if len(page.Items) == 0 {
				return ErrNoClaimableWork
			}
			plan.candidate = page.Items[0]
		}
		plan.item, err = r.WorkItemByID(ctx, scope.project, plan.candidate.ID)
		if err != nil {
			return err
		}
		if plan.item.Revision != plan.candidate.Revision || plan.item.Archived {
			return ErrNoClaimableWork
		}
		plan.version, err = r.CollaborationVersion(ctx, string(plan.item.NativeID), plan.item.Revision)
		if err != nil {
			return err
		}
		if plan.item.NativeSourceKey != nil {
			linked, err := r.LinkedSource(ctx, scope.project, plan.item.NativeID)
			if err != nil {
				return err
			}
			plan.linkedRecord = &linked
		}
		plan.changeRecord, err = r.LatestChange(ctx, scope.project, plan.item.NativeID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && plan.changeRecord.CurrentVersionID != nil {
			plan.changeVersion, err = r.Version(ctx, plan.changeRecord.ID, *plan.changeRecord.CurrentVersionID)
			if err != nil {
				return err
			}
			plan.changePolicyRevision, err = r.ProjectPolicyRevision(ctx, scope.project, plan.changeVersion.PolicyRevisionID)
			if err != nil {
				return err
			}
			plan.change = tracker.NativeChangeReference{ChangeID: plan.changeRecord.ID, VersionID: plan.changeVersion.ID, HeadSHA: plan.changeVersion.HeadSHA}
		}
		plan.latestAttempt, err = r.LatestAttempt(ctx, plan.item.ID)
		if err == nil {
			plan.latestAttemptFound = true
			plan.latestRunEvent, err = r.LatestRunEvent(ctx, scope.project, plan.latestAttempt.ID)
			if err == nil {
				plan.latestRunEventFound = true
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		plan.landingPending = nativeClaimPendingLanding(plan)
		if (stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending) && plan.changeRecord.ID != "" {
			plan.changePolicy, err = readChangeSummaryPolicyRows(ctx, r, scope)
			if err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if reader != nil {
		err = readRows(reader)
	} else {
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, readRows)
	}
	if err != nil {
		return nativeClaimPlan{}, err
	}
	return plan, nil
}

func (s *Service) hydrateNativeClaimPlan(ctx context.Context, scope nativeScope, request tracker.NativeClaim, now time.Time, plan nativeClaimPlan, descriptor *policy.Descriptor) (nativeClaimPlan, error) {
	var err error
	if descriptor == nil {
		loaded, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, plan.policy)
		if err != nil {
			return nativeClaimPlan{}, err
		}
		descriptor = &loaded
	}
	plan.descriptor = *descriptor
	if plan.descriptor.ID != request.PolicyID {
		return nativeClaimPlan{}, policyMismatch("Claim configuration does not match the current project policy")
	}
	if plan.policy.GitHubPullRequest == nil || *plan.policy.GitHubPullRequest != plan.descriptor.Gates.GitHubPullRequest {
		return nativeClaimPlan{}, ErrNoClaimableWork
	}
	raw, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).CollaborationVersion(ctx, plan.version)
	if err != nil {
		return nativeClaimPlan{}, err
	}
	if err := json.Unmarshal(raw, &plan.issue); err != nil || plan.issue.OrganizationID != plan.item.OrganizationID || plan.issue.ProjectID != plan.item.ProjectID || plan.issue.WorkItemID != plan.item.NativeID || int64(plan.issue.Revision) != plan.item.Revision || int64(plan.issue.Number) != plan.item.Number || plan.issue.Title != plan.item.Title || plan.issue.Archived != plan.item.Archived || plan.issue.Actor.Kind != valueOrEmpty(plan.item.ActorKind) || plan.issue.Actor.PrincipalID != valueOrEmpty(plan.item.ActorPrincipalID) {
		return nativeClaimPlan{}, corestore.ErrRevisionConflict
	}
	if nativeClaimDispatchSatisfied(plan) {
		return nativeClaimPlan{}, ErrNoClaimableWork
	}
	plan.issue.State = valueOrEmpty(plan.item.WorkflowStateName)
	plan.issue.LastActivityAt = plan.item.LastActivityAt
	if plan.linkedRecord != nil {
		linked, found, err := s.readLinkedIssueSourceRecord(ctx, scope.organization, scope.project, plan.item.NativeID)
		if err != nil {
			return nativeClaimPlan{}, err
		}
		if !found {
			return nativeClaimPlan{}, corestore.ErrRevisionConflict
		}
		plan.linked = &linked
		plan.issue.LinkedSource = &linked
	}
	if stringsEqualFold(plan.candidate.State, "Merging") || plan.landingPending {
		if plan.changeRecord.ID == "" || plan.changeRecord.CurrentVersionID == nil || *plan.changeRecord.CurrentVersionID != plan.changeVersion.ID || plan.changeVersion.ID != plan.change.VersionID || plan.changeVersion.HeadSHA != plan.change.HeadSHA {
			return plan, nil
		}
		detail, err := s.nativeClaimCurrentChangeDetail(ctx, scope, plan, now)
		if err != nil {
			return nativeClaimPlan{}, err
		}
		state := changerequest.ReviewState(detail, plan.changeVersion.ID, plan.changeVersion.HeadSHA)
		currentVersion := valueOrEmpty(plan.changeRecord.CurrentVersionID)
		plan.changeReady = plan.changeRecord.OrganizationID == scope.organization && plan.changeRecord.ProjectID == scope.project && plan.changeRecord.WorkItemID == plan.candidate.WorkItemID && detail.Change.ID == plan.changeRecord.ID && int64(detail.Change.Revision) == plan.changeRecord.Revision && detail.Change.CurrentVersion == currentVersion && currentVersion == plan.changeVersion.ID && state.Current && !state.ChangesRequested && !state.RepairPending && nativeChangeLandingClaimable("Merging", &detail)
	}
	if stringsEqualFold(plan.candidate.State, "Merging") {
		if plan.changePolicyRevision.GitHubPullRequest == nil {
			return nativeClaimPlan{}, ErrNoClaimableWork
		}
		plan.landingRequired = *plan.changePolicyRevision.GitHubPullRequest
	} else if plan.landingPending {
		if plan.changePolicyRevision.GitHubPullRequest == nil || !*plan.changePolicyRevision.GitHubPullRequest {
			return nativeClaimPlan{}, ErrNoClaimableWork
		}
		plan.landingRequired = true
	}
	return plan, nil
}

func nativeClaimPendingLanding(plan nativeClaimPlan) bool {
	if !plan.latestAttemptFound || !plan.latestRunEventFound || !plan.latestRunEvent.LandingWaiting || plan.changeRecord.ID == "" || plan.changeRecord.CurrentVersionID == nil {
		return false
	}
	return plan.latestRunEvent.LandingChangeID == plan.changeRecord.ID && plan.latestRunEvent.LandingVersionID == plan.changeVersion.ID && plan.latestRunEvent.LandingHeadSHA == plan.changeVersion.HeadSHA && plan.changeVersion.ID == *plan.changeRecord.CurrentVersionID
}

func verifyNativeClaimLandingEvent(ctx context.Context, r tenantstore.Reader, issueID int64, plan nativeClaimPlan) error {
	attempt, err := r.LatestAttempt(ctx, issueID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.latestAttemptFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.latestAttemptFound || attempt.ID != plan.latestAttempt.ID || attempt.FencingToken != plan.latestAttempt.FencingToken || attempt.Sequence != plan.latestAttempt.Sequence {
		return corestore.ErrRevisionConflict
	}
	event, err := r.LatestRunEvent(ctx, plan.candidate.ProjectID, attempt.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		if plan.latestRunEventFound {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !plan.latestRunEventFound || event.ID != plan.latestRunEvent.ID || event.Sequence != plan.latestRunEvent.Sequence || event.LandingWaiting != plan.latestRunEvent.LandingWaiting || event.LandingChangeID != plan.latestRunEvent.LandingChangeID || event.LandingVersionID != plan.latestRunEvent.LandingVersionID || event.LandingHeadSHA != plan.latestRunEvent.LandingHeadSHA {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func sameNativeClaimChangeRecord(left, right eventstore.Change) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.ID == right.ID && left.Revision == right.Revision && left.Title == right.Title && valueOrEmpty(left.CurrentVersionID) == valueOrEmpty(right.CurrentVersionID) && left.EvidenceSequence == right.EvidenceSequence && left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimChangePolicyRows(left, right currentChangePolicyRows) bool {
	return sameNativeClaimProjectPolicy(left.Project, right.Project) && sameNativeClaimReviewPolicy(left.Review, right.Review) && left.hasReview == right.hasReview
}

func sameNativeClaimProjectPolicy(left, right policystore.Revision) bool {
	return sameNativeClaimPolicyRevision(left, right) && left.ApprovedBy == right.ApprovedBy && left.ApprovedAt.Equal(right.ApprovedAt)
}

func sameNativeClaimPolicyRevision(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && sameNativeClaimOptionalBool(left.GitHubPullRequest, right.GitHubPullRequest) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameNativeClaimReviewPolicy(left eventstore.ReviewPolicy, right eventstore.ReviewPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func sameNativeClaimLinkedSource(left, right eventstore.LinkedSource) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.WorkItemID == right.WorkItemID && left.URL == right.URL && left.TitleSupplied == right.TitleSupplied && left.BodySupplied == right.BodySupplied && left.TitleOwned == right.TitleOwned && left.BodyOwned == right.BodyOwned && left.SnapshotObjectKey == right.SnapshotObjectKey && left.SnapshotSHA256 == right.SnapshotSHA256 && left.SnapshotBytes == right.SnapshotBytes && left.SnapshotContentBytes == right.SnapshotContentBytes && stringValue(left.ExpectedSnapshotSHA256) == stringValue(right.ExpectedSnapshotSHA256)
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nativeClaimIssueContractAllowed(plan nativeClaimPlan) bool {
	if strings.Contains(strings.ToLower(plan.issue.Body), "detent:no-dispatch") {
		return false
	}
	if plan.issue.LinkedSource != nil && plan.issue.LinkedSource.Status != "complete" || strings.EqualFold(plan.issue.State, "Triage") || plan.issue.IssueContract != nil && plan.issue.IssueContract.Exempt {
		return true
	}
	contract, err := config.ResolvePolicyIssueContract(plan.descriptor)
	if err != nil {
		return false
	}
	target := "Todo"
	workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, plan.descriptor)
	if err != nil {
		return false
	}
	if workflow.Config.BacklogAdmission.TargetState != "" {
		target = workflow.Config.BacklogAdmission.TargetState
	}
	if !strings.EqualFold(plan.issue.State, target) {
		return true
	}
	if plan.issue.IssueContract != nil && plan.issue.IssueContract.ReleaseDelivery != nil {
		return false
	}
	evaluation := contract.Evaluate(connector.Issue{Title: plan.issue.Title, Description: plan.issue.Body, IssueContract: plan.issue.IssueContract})
	return evaluation.Satisfied()
}

func nativeTenantCandidateQuery(scope nativeScope, request tracker.NativeClaim, item tracker.NativeWorkItemID, now time.Time) claimstore.CandidateQuery {
	workItemIDs := make([]tracker.NativeWorkItemID, 0, len(request.ProviderCandidates))
	for _, candidate := range request.ProviderCandidates {
		workItemIDs = append(workItemIDs, candidate.WorkItemID)
	}
	return claimstore.CandidateQuery{
		ProjectID: scope.project, Scope: string(scope.project), WorkItemID: item, WorkItemIDs: workItemIDs,
		States: request.WorkflowStates, Authors: request.Authors, Assignees: request.Assignees,
		IncludeLabels: request.LabelInclude, ExcludeLabels: request.LabelExclude,
		WorkspaceLane:   slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability),
		RunnerSelection: true, PrioritizeUnblockers: request.PrioritizeUnblockers,
		StatePriority: request.DispatchPriorityByState, LabelPriority: request.DispatchPriorityByLabel,
		AvailableAt: now, Limit: 1,
	}
}

func runnerIdentityCanClaim(identity runnerstore.Identity, scope nativeScope, machine tracker.MachineID, now time.Time) bool {
	return identity.OrganizationID == scope.organization && identity.TokenID == scope.credential.ID && identity.RunnerID == scope.credential.Runner.RunnerID && identity.MachineID == machine && machine == scope.credential.Runner.MachineID && identity.State == "active" && identity.RemovedAt == nil && identity.ExpiresAt.After(now) && !now.Before(identity.LastHeartbeatAt) && now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) && slices.Contains(identity.Operations, runnerauth.Claim)
}

func stringsEqualFold(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}
