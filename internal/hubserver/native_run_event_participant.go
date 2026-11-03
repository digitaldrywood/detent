package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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
	if authority == nil || authority.OrganizationID == "" || authority.ProjectID == "" || authority.WorkItem.NativeID == "" || authority.Lease.LeaseID == "" {
		return nativeRunEventStorageUnavailable()
	}
	event := authority.Event
	if err := validateNativeRunEvent(event); err != nil {
		return err
	}
	if event.Data.Sequence <= 0 || event.Data.LeaseID != tracker.LeaseID(authority.Lease.LeaseID) || int64(event.Data.FencingToken) != authority.Lease.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	if authority.Scope.organization != authority.OrganizationID || authority.Scope.project != authority.ProjectID || authority.Lease.OrganizationID != authority.OrganizationID {
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
	now := p.service.config.now().UTC()
	if lease.IssueID != item.ID || lease.FencingToken != authority.Lease.FencingToken || lease.MachineID != authority.Lease.MachineID || lease.SessionID != authority.Lease.SessionID || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	if event.Data.MachineID != tracker.MachineID(lease.MachineID) || event.Data.SessionID != lease.SessionID || event.Data.RunnerID != authority.Scope.credential.Runner.RunnerID {
		return nativeExecutionConflict("Execution identity must match the authenticated lease owner")
	}
	identity, err := reader.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return err
	}
	if identity.OrganizationID != authority.OrganizationID || identity.RunnerID == "" || identity.RunnerID != authority.Scope.credential.Runner.RunnerID || identity.TokenID != authority.Scope.credential.ID || identity.MachineID != tracker.MachineID(lease.MachineID) || identity.RemovedAt != nil || identity.State != "active" || !identity.ExpiresAt.After(now) || !slices.Contains(identity.Operations, runnerauth.Claim) {
		return tracker.ErrStaleFencingToken
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, authority.ProjectID, now)
	if err != nil {
		return err
	}
	if !allowed {
		return tracker.ErrMachineNotFound
	}
	pinned, err := reader.LeasePolicy(ctx, tracker.LeaseID(lease.LeaseID))
	if err != nil {
		return err
	}
	if pinned.OrganizationID != authority.OrganizationID || pinned.ProjectID != authority.ProjectID || pinned.PolicyID != event.Data.PolicyID {
		return policyMismatch("Run event policy must match the policy pinned to its lease")
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ValidateNativeRunEvent(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	if err := p.validateNativeRunEventOwner(ctx, reader, authority); err != nil {
		return err
	}
	event := authority.Event
	item, lease := authority.WorkItem, authority.Lease
	if event.Data.Sequence > 1 {
		attempt, err := reader.Attempt(ctx, event.Data.AttemptID)
		if err != nil {
			return err
		}
		if attempt.ID != event.Data.AttemptID || attempt.IssueID != item.ID || attempt.LeaseID != lease.LeaseID || attempt.FencingToken != lease.FencingToken || attempt.RunID != event.Data.RunID || attempt.Sequence != event.Data.Sequence && attempt.Sequence+1 != event.Data.Sequence {
			return leasestore.ErrAttemptConflict
		}
		if attempt.Status != "running" {
			if !authority.UsageCorrection || event.Type != "run.finished" || attempt.Status != event.Data.Outcome {
				return leasestore.ErrAttemptConflict
			}
		}
		if authority.UsageFacts != nil {
			facts := authority.UsageFacts
			if event.Type != "run.finished" || len(event.Data.Usage) == 0 || len(authority.UsageCosts) != len(event.Data.Usage) || facts.WorkItemID != string(item.NativeID) || facts.RunnerID != authority.Scope.credential.Runner.RunnerID || facts.MachineID != lease.MachineID || !facts.StartedAt.Equal(attempt.StartedAt) || !facts.StartedAt.Before(facts.EndedAt) {
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
				if record.OrganizationID != authority.OrganizationID || record.ProjectID != authority.ProjectID || observation.AttemptID != attempt.ID || observation.WorkItemID != facts.WorkItemID || observation.RunnerID != facts.RunnerID || observation.MachineID != facts.MachineID || observation.Placement != facts.Placement || !observation.From.Equal(usage.From) || !observation.To.Equal(usage.To) {
					return leasestore.ErrAttemptConflict
				}
			}
		}
		authority.DispatchGeneration = attempt.DispatchGeneration
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ApplyNativeRunEvent(ctx context.Context, writer tenantstore.Writer, authority NativeRunEventAuthority) error {
	if len(authority.UsageCosts) > 0 {
		if _, err := writer.RecordBillingCosts(ctx, authority.UsageCosts); err != nil {
			return err
		}
		reports, ok := writer.(reportstore.UsageWriter)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		if err := reports.RefreshReportUsage(ctx, authority.UsageCosts); err != nil {
			return err
		}
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ValidateNativeLeaseMutation(ctx context.Context, reader tenantstore.Reader, authority *NativeLeaseAuthority) error {
	if authority == nil || authority.Scope.organization == "" || authority.Scope.project == "" || authority.Lease.LeaseID == "" {
		return nativeRunEventStorageUnavailable()
	}
	if authority.Scope.credential.Scope != apiScopeWorker || authority.Scope.credential.Runner.RunnerID == "" || authority.Lease.OrganizationID != authority.Scope.organization {
		return tracker.ErrMachineNotFound
	}
	item, err := reader.WorkItemByID(ctx, authority.Scope.project, authority.Lease.IssueID)
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
	now := p.service.config.now().UTC()
	if lease.IssueID != item.ID || lease.FencingToken != authority.Lease.FencingToken || lease.MachineID != authority.Lease.MachineID || lease.SessionID != authority.Lease.SessionID || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || int64(authority.Request.FencingToken) != lease.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	identity, err := reader.RunnerForLease(ctx, lease.LeaseID)
	if err != nil {
		return err
	}
	if identity.OrganizationID != authority.Scope.organization || identity.RunnerID == "" || identity.RunnerID != authority.Scope.credential.Runner.RunnerID || identity.TokenID != authority.Scope.credential.ID || identity.MachineID != tracker.MachineID(lease.MachineID) || identity.RemovedAt != nil || identity.State != "active" || !identity.ExpiresAt.After(now) {
		return tracker.ErrStaleFencingToken
	}
	allowed, err := reader.RunnerProjectAuthorized(ctx, identity.RunnerID, authority.Scope.project, now)
	if err != nil {
		return err
	}
	if !allowed {
		return tracker.ErrMachineNotFound
	}
	if authority.Operation != "renew" && authority.Operation != "release" {
		return tracker.ErrInvalidClaimRequest
	}
	return nil
}

func (p tenantNativeRunEventParticipant) ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error {
	return nil
}

func (p tenantNativeRunEventParticipant) NativeLeaseResponse(ctx context.Context, reader tenantstore.Reader, scope nativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
	item, err := reader.WorkItemByID(ctx, scope.project, lease.IssueID)
	if err != nil {
		return NativeLeaseResponseDetails{}, err
	}
	if item.ProjectID != scope.project || item.Archived {
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
	return NativeLeaseResponseDetails{
		WorkItemID: item.NativeID, PolicyID: pinned.PolicyID,
		IsolationPolicy:     &isolation.Policy{Tier: identity.IsolationTier, HostServices: slices.Clone(identity.HostServices)},
		ProviderReservation: reservation,
	}, nil
}

var _ NativeRunEventParticipant = tenantNativeRunEventParticipant{}
