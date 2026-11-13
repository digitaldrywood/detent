package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantNativeRunEventParticipant struct {
	service *Service
}

func (p tenantNativeRunEventParticipant) validateNativeRunEventOwner(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	return p.validateRunEventOwner(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateRunEventOwner(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority, human bool) error {
	if authority == nil || authority.OrganizationID == "" || authority.ProjectID == "" || authority.WorkItem.NativeID == "" || authority.Lease.LeaseID == "" {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	event := authority.Event
	if human && (event.Type != "run.finished" || event.Data.Outcome != "cancelled" || event.Data.Sequence <= 1 || authority.UsageCorrection || authority.UsageFacts != nil || len(authority.UsageCosts) != 0) {
		return tracker.ErrInvalidClaimRequest
	}
	if !authority.Termination {
		if err := validateNativeRunEvent(event); err != nil {
			return err
		}
	}
	if event.Data.Sequence <= 0 || event.Data.LeaseID != tracker.LeaseID(authority.Lease.LeaseID) || int64(event.Data.FencingToken) != authority.Lease.FencingToken {
		return &leasestore.FenceError{Check: "event_fence"}
	}
	if authority.Scope.Organization != authority.OrganizationID || authority.Scope.Project != authority.ProjectID || authority.Lease.OrganizationID != authority.OrganizationID {
		return corestore.ErrTenantMismatch
	}
	project, err := reader.Project(ctx, authority.ProjectID)
	if err != nil {
		return err
	}
	if project.DeletedAt != nil {
		return corestore.ErrNotFound
	}
	item, err := reader.WorkItem(ctx, authority.ProjectID, authority.WorkItem.NativeID)
	if err != nil {
		return err
	}
	if item.ID != authority.WorkItem.ID || item.ProjectID != authority.ProjectID || item.Archived {
		return corestore.ErrRevisionConflict
	}
	lease, err := reader.Lease(ctx, authority.Lease.LeaseID)
	if err != nil {
		return err
	}
	now := p.service.config.Clock().UTC()
	if lease.IssueID != item.ID || lease.FencingToken != authority.Lease.FencingToken || lease.MachineID != authority.Lease.MachineID || lease.SessionID != authority.Lease.SessionID || !authority.Termination && (lease.ReleasedAt != nil || !lease.ExpiresAt.After(now)) {
		return &leasestore.FenceError{Check: "lease_current"}
	}
	if event.Data.MachineID != tracker.MachineID(lease.MachineID) || event.Data.SessionID != lease.SessionID || !human && event.Data.RunnerID != authority.Scope.Credential.Runner.RunnerID {
		return nativeExecutionConflict("Execution identity must match the authenticated lease owner")
	}
	if human {
		if err := p.service.authorizeNativeWorkflowScope(ctx, reader, authority.Scope); err != nil {
			return err
		}
	} else {
		identity, err := reader.RunnerForLease(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		if identity.OrganizationID != authority.OrganizationID || identity.RunnerID == "" || identity.RunnerID != authority.Scope.Credential.Runner.RunnerID || identity.TokenID != authority.Scope.Credential.ID || identity.MachineID != tracker.MachineID(lease.MachineID) || identity.RemovedAt != nil || !runnerStateHoldsLease(identity.State) || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
			return &leasestore.FenceError{Check: "runner_owner"}
		}
		allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, authority.ProjectID, now)
		if err != nil {
			return err
		}
		if !allowed {
			return tracker.ErrMachineNotFound
		}
	}
	pinned, err := reader.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
	if err != nil {
		return err
	}
	if pinned.OrganizationID != authority.OrganizationID || pinned.ProjectID != authority.ProjectID || pinned.PolicyID != event.Data.PolicyID {
		return hubapi.PolicyMismatch("Run event policy must match the policy pinned to its lease")
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ValidateNativeRunEvent(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	return p.validateNativeRunEvent(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateNativeRunEvent(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority, human bool) error {
	if err := p.validateRunEventOwner(ctx, reader, authority, human); err != nil {
		return err
	}
	if authority.ReplayPrefix {
		if authority.Event.Data.Sequence == 1 {
			attempt, err := reader.Attempt(ctx, authority.Event.Data.AttemptID)
			if err != nil {
				return err
			}
			authority.DispatchGeneration = attempt.DispatchGeneration
		}
		return nil
	}
	event := authority.Event
	item, lease := authority.WorkItem, authority.Lease
	if event.Data.Sequence == 1 {
		dispatch, ok := reader.(tenantstore.DispatchReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		state, err := dispatch.IssueDispatchState(ctx, authority.ProjectID, item.NativeID)
		if err != nil {
			return err
		}
		if state.Revision != lease.WorkItemRevision {
			return &leasestore.FenceError{Check: "dispatch_revision"}
		}
		authority.DispatchGeneration = state.Generation
		if event.Type == "run.started" && event.Data.Identity != nil {
			if err := validateTenantProviderAttempt(ctx, reader, authority.Scope.Credential.Runner.RunnerID, event.Data, p.service.config.Clock().UTC()); err != nil {
				return err
			}
		}
	}
	if event.Data.Sequence > 1 {
		attempt, err := reader.Attempt(ctx, event.Data.AttemptID)
		if err != nil {
			return err
		}
		if attempt.ID != event.Data.AttemptID || attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.RunID != event.Data.RunID || attempt.Sequence != event.Data.Sequence && attempt.Sequence+1 != event.Data.Sequence {
			return leasestore.ErrAttemptConflict
		}
		if attempt.Status != "running" && attempt.Sequence != event.Data.Sequence {
			reconnecting := attempt.Status == "interrupted" && event.Type == "run.observed" && event.Data.Outcome == "" && event.Data.Runtime != nil && event.Data.Runtime.LocalAttemptID == 0 && event.Data.Runtime.Generation == 0
			terminating := authority.Termination && event.Type == "run.finished" && event.Data.Outcome == "cancelled" && (attempt.Status == "interrupted" || attempt.Status == "cancelled")
			if !terminating && !reconnecting && (!authority.UsageCorrection || event.Type != "run.finished" || attempt.Status != event.Data.Outcome) {
				return leasestore.ErrAttemptConflict
			}
		}
		if authority.UsageFacts != nil {
			facts := authority.UsageFacts
			if event.Type != "run.finished" || len(event.Data.Usage) == 0 || len(authority.UsageCosts) != len(event.Data.Usage) || facts.WorkItemID != string(item.NativeID) || facts.RunnerID != authority.Scope.Credential.Runner.RunnerID || facts.MachineID != lease.MachineID || !facts.StartedAt.Equal(attempt.StartedAt) || !facts.StartedAt.Before(facts.EndedAt) {
				return leasestore.ErrAttemptConflict
			}
			machine, err := reader.Machine(ctx, lease.MachineID)
			if err != nil {
				return err
			}
			placement := "local"
			if machine.SpriteName != "" {
				placement = "sprite"
			}
			if facts.Placement != placement {
				return leasestore.ErrAttemptConflict
			}
			for i, record := range authority.UsageCosts {
				observation := record.Observation
				usage := event.Data.Usage[i]
				expectedPlacement := facts.Placement
				if authority.UsageCorrection {
					for _, prior := range authority.UsageReport.Historical {
						old := prior.Observation
						if prior.OrganizationID == authority.OrganizationID && prior.ProjectID == authority.ProjectID && old.Provider == observation.Provider && old.ProviderAccount == observation.ProviderAccount && old.SourceID == observation.SourceID && old.From.Equal(observation.From) && old.To.Equal(observation.To) && old.AttemptID == attempt.ID && old.WorkItemID == facts.WorkItemID && old.RunnerID == facts.RunnerID && old.MachineID == facts.MachineID {
							expectedPlacement = old.Placement
							break
						}
					}
				}
				if record.OrganizationID != authority.OrganizationID || record.ProjectID != authority.ProjectID || observation.AttemptID != attempt.ID || observation.WorkItemID != facts.WorkItemID || observation.RunnerID != facts.RunnerID || observation.MachineID != facts.MachineID || observation.Placement != expectedPlacement || !observation.From.Equal(usage.From) || !observation.To.Equal(usage.To) {
					return leasestore.ErrAttemptConflict
				}
			}
		}
		authority.DispatchGeneration = attempt.DispatchGeneration
	}
	if !authority.Termination {
		if err := p.service.validateTenantPRPublication(ctx, reader, authority); err != nil {
			return err
		}
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ApplyNativeRunEvent(ctx context.Context, writer tenantstore.Writer, authority NativeRunEventAuthority) error {
	if !authority.Termination {
		if publication := tenantRunPRPublication(authority.Event.Data); publication != nil {
			if err := writer.PutPRPublication(ctx, tenantPRPublicationRecord(authority, *publication)); err != nil {
				return err
			}
		}
	}
	if len(authority.UsageReport.Totals) > 0 {
		reports, ok := writer.(reportstore.UsageWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		if err := reports.RecordAttemptUsage(ctx, authority.UsageReport); err != nil {
			return err
		}
	}
	if len(authority.UsageCosts) > 0 {
		if _, err := writer.RecordBillingCosts(ctx, authority.UsageCosts); err != nil {
			return err
		}
		reports, ok := writer.(reportstore.UsageWriter)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		if err := reports.RefreshReportUsage(ctx, authority.UsageCosts); err != nil {
			return err
		}
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ValidateNativeLeaseMutation(ctx context.Context, reader tenantstore.Reader, authority *NativeLeaseAuthority) error {
	return p.validateNativeLeaseMutation(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateNativeLeaseMutation(ctx context.Context, reader tenantstore.Reader, authority *NativeLeaseAuthority, human bool) error {
	if authority == nil || authority.Scope.Organization == "" || authority.Scope.Project == "" || authority.Lease.LeaseID == "" {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	if human && (authority.Operation != "release" || authority.Request.Reason != "cancelled" || !authority.Request.CancelAttempt) {
		return tracker.ErrInvalidClaimRequest
	}
	if authority.Lease.OrganizationID != authority.Scope.Organization || !human && (authority.Scope.Credential.Scope != hubapi.APIScopeWorker || authority.Scope.Credential.Runner.RunnerID == "") {
		return tracker.ErrMachineNotFound
	}
	item, err := reader.WorkItemByID(ctx, authority.Scope.Project, authority.Lease.IssueID)
	if err != nil {
		return err
	}
	if item.Archived {
		return corestore.ErrNotFound
	}
	authority.WorkItem = item
	lease, err := reader.Lease(ctx, authority.Lease.LeaseID)
	if err != nil {
		return err
	}
	now := p.service.config.Clock().UTC()
	if !human {
		identity, err := reader.RunnerForLease(ctx, lease.LeaseID)
		if err != nil {
			return err
		}
		if identity.OrganizationID != authority.Scope.Organization || identity.RunnerID == "" || identity.RunnerID != authority.Scope.Credential.Runner.RunnerID || identity.TokenID != authority.Scope.Credential.ID || identity.MachineID != tracker.MachineID(lease.MachineID) || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) {
			return tracker.ErrMachineNotFound
		}
		if identity.State == "disabled" {
			return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
		}
		if !runnerStateHoldsLease(identity.State) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, authority.Scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return tracker.ErrMachineNotFound
		}
	}
	if lease.IssueID != item.ID || lease.FencingToken != authority.Lease.FencingToken || lease.MachineID != authority.Lease.MachineID || lease.SessionID != authority.Lease.SessionID || (lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) && (authority.Operation != "renew" || authority.Request.LandingCapacity <= 0 && lease.LandingCapacity <= 0)) || int64(authority.Request.FencingToken) != lease.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	if human {
		if err := p.service.authorizeNativeWorkflowScope(ctx, reader, authority.Scope); err != nil {
			return err
		}
	}
	if authority.Operation != "renew" && authority.Operation != "release" {
		return tracker.ErrInvalidClaimRequest
	}
	if authority.Operation == "renew" {
		return checkMonthlyLeasePG(ctx, reader, authority.Scope.Project, lease.LeaseID, now)
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error {
	return nil
}

func (p tenantNativeRunEventParticipant) NativeLeaseResponse(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
	item, err := reader.WorkItemByID(ctx, scope.Project, lease.IssueID)
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	if item.ProjectID != scope.Project || item.Archived {
		return NativeLeaseResponseDetails{}, corestore.ErrNotFound
	}
	identity, err := reader.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	pinned, err := reader.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	var reservation *providercapacity.Reservation
	stored, err := reader.ProviderReservation(ctx, tracker.LeaseID(lease.LeaseID))
	if err == nil {
		reservation = &stored.Reservation
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return NativeLeaseResponseDetails{}, err
	}
	tier := identity.IsolationTier
	if lease.IsolationTier != nil {
		tier = *lease.IsolationTier
	}
	return NativeLeaseResponseDetails{
		WorkItemID: item.NativeID, PolicyID: pinned.PolicyID,
		IsolationPolicy:     &isolation.Policy{Tier: tier, HostServices: slices.Clone(identity.HostServices)},
		ProviderReservation: reservation,
	}, nil
}

var _ NativeRunEventParticipant = tenantNativeRunEventParticipant{}

func runnerStateHoldsLease(state string) bool {
	return state == "active" || state == "draining"
}
