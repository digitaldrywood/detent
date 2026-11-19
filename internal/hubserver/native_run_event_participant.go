package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/executionapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type tenantNativeRunEventParticipant struct {
	service *Service
}

func (p tenantNativeRunEventParticipant) validateNativeRunEventOwner(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	return p.validateRunEventOwner(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateRunEventOwner(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority, human bool) error {
	return p.deps().ValidateRunEventOwner(ctx, reader, runnerEventCheck(authority), human)
}

func (p tenantNativeRunEventParticipant) ValidateNativeRunEvent(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority) error {
	return p.validateNativeRunEvent(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateNativeRunEvent(ctx context.Context, reader tenantstore.Reader, authority *NativeRunEventAuthority, human bool) error {
	return p.deps().ValidateRunEvent(ctx, reader, runnerEventCheck(authority), human, func(ctx context.Context, r tenantstore.Reader) error {
		return p.service.validateTenantPRPublication(ctx, r, authority)
	})
}

func (p tenantNativeRunEventParticipant) ApplyNativeRunEvent(ctx context.Context, writer tenantstore.Writer, authority NativeRunEventAuthority) error {
	if !authority.Termination {
		if publication := tenantRunPRPublication(authority.Event.Data); publication != nil {
			if err := writer.PutPRPublication(ctx, tenantPRPublicationRecord(authority, *publication)); err != nil {
				return err
			}
		}
	}
	return executionapi.ApplyRunEventUsage(ctx, writer, authority.UsageReport, authority.UsageCosts)
}

func (p tenantNativeRunEventParticipant) ValidateNativeLeaseMutation(ctx context.Context, reader tenantstore.Reader, authority *NativeLeaseAuthority) error {
	return p.validateNativeLeaseMutation(ctx, reader, authority, false)
}

func (p tenantNativeRunEventParticipant) validateNativeLeaseMutation(ctx context.Context, reader tenantstore.Reader, authority *NativeLeaseAuthority, human bool) error {
	var check *executionapi.LeaseMutationCheck
	if authority != nil {
		check = &executionapi.LeaseMutationCheck{Scope: authority.Scope, WorkItem: &authority.WorkItem, Lease: authority.Lease,
			Request: authority.Request, Operation: authority.Operation, LegacySelectorChecked: authority.LegacySelectorChecked}
	}
	return p.deps().ValidateLeaseMutation(ctx, reader, check, human)
}

func (p tenantNativeRunEventParticipant) ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error {
	return nil
}

func (p tenantNativeRunEventParticipant) NativeLeaseResponse(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, lease leasestore.Lease) (NativeLeaseResponseDetails, error) {
	result, err := executionapi.ReadLeaseResponse(ctx, reader, scope, lease)
	return NativeLeaseResponseDetails{WorkItemID: result.WorkItemID, PolicyID: result.PolicyID, IsolationPolicy: result.IsolationPolicy,
		ProviderReservation: result.ProviderReservation}, err
}

var _ NativeRunEventParticipant = tenantNativeRunEventParticipant{}

func (p tenantNativeRunEventParticipant) deps() executionapi.RunEventDeps {
	return executionapi.RunEventDeps{
		Now:               func() time.Time { return p.service.config.Clock() },
		ValidateEvent:     validateNativeRunEvent,
		AuthorizeWorkflow: p.service.authorizeNativeWorkflowScope,
		CheckMonthlyLease: checkMonthlyLeasePG,
	}
}
func runnerEventCheck(authority *NativeRunEventAuthority) *executionapi.RunEventCheck {
	if authority == nil {
		return nil
	}
	return &executionapi.RunEventCheck{
		OrganizationID: authority.OrganizationID, ProjectID: authority.ProjectID, Scope: authority.Scope, WorkItem: authority.WorkItem,
		Lease: authority.Lease, Event: authority.Event, Termination: authority.Termination, RequireSelector: authority.RequireSelector,
		LegacySelectorChecked: authority.LegacySelectorChecked, ReplayPrefix: authority.ReplayPrefix, DispatchGeneration: &authority.DispatchGeneration,
		UsageCorrection: authority.UsageCorrection, UsageFacts: authority.UsageFacts, UsageCosts: authority.UsageCosts, UsageReport: authority.UsageReport,
	}
}
