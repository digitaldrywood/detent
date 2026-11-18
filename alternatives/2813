package postgres

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const DoctorProposalOperation = "doctor.proposal.initialize"

type DoctorProposalInitialization struct {
	OrganizationID          tracker.OrganizationID
	ProjectID               tracker.ProjectID
	WorkItemID              tracker.NativeWorkItemID
	Filer                   tracker.Actor
	ExpectedRevision        int64
	ExpectedEventSequence   int64
	ExpectedSourceKey       *string
	ExpectedVersionSHA256   string
	ExpectedState           string
	TargetState             string
	ContentSettingsRevision int64
	SettingsRevision        int64
	WorkflowSourceRevision  string
	PolicyID                string
	PolicySHA256            string
	Fence                   leasestore.Fence
	At                      time.Time
	Version                 eventstore.CollaborationVersion
	Event                   eventstore.Event
	Receipt                 eventstore.Receipt
}
type DoctorProposalApply func(context.Context, tenantstore.Writer) error
type DoctorProposalInitializer interface {
	InitializeDoctorProposalLane(context.Context, DoctorProposalInitialization, DoctorProposalApply) (eventstore.Receipt, error)
}
type DoctorProposalWriter interface {
	tenantstore.Writer
	DoctorProposalInitializer
}

func (r *Repository) WriteDoctorProposals(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, run func(DoctorProposalWriter) error) error {
	if run == nil {
		return corestore.ErrInvalidPage
	}
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return run(s) })
}
func (s *session) InitializeDoctorProposalLane(ctx context.Context, arg DoctorProposalInitialization, apply DoctorProposalApply) (eventstore.Receipt, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return eventstore.Receipt{}, err
	}
	receipt := arg.Receipt
	if arg.ProjectID == "" || arg.WorkItemID == "" || arg.Filer.Kind == "" || arg.Filer.PrincipalID == "" || arg.ExpectedRevision != 1 || arg.ExpectedEventSequence < 0 || !artifact.ValidHash(arg.ExpectedVersionSHA256, 64) || arg.TargetState == "" || arg.ContentSettingsRevision < 0 || arg.SettingsRevision < 1 || arg.PolicyID == "" || !artifact.ValidHash(arg.PolicySHA256, 64) || arg.At.IsZero() || receipt.OrganizationID != s.organization || receipt.ActorID == "" || receipt.Operation != DoctorProposalOperation || receipt.CommandKey != string(arg.ProjectID)+"/"+string(arg.WorkItemID) || !artifact.ValidHash(receipt.RequestHash, 64) {
		return eventstore.Receipt{}, eventstore.ErrInvalidRecord
	}
	previous, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
	if err == nil {
		if previous.RequestHash != receipt.RequestHash {
			return eventstore.Receipt{}, corestore.ErrRevisionConflict
		}
		return previous, nil
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		return eventstore.Receipt{}, err
	}
	if err := s.LockWorkItemProject(ctx, arg.ProjectID); err != nil {
		return eventstore.Receipt{}, err
	}
	project, err := s.ProjectRecord(ctx, arg.ProjectID)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	if project.Core.DeletedAt != nil || project.Core.Profile != "native" || project.Core.ContentSettingsRevision != arg.ContentSettingsRevision || project.Core.WorkflowSourceRevision != arg.WorkflowSourceRevision || project.Settings.Revision != arg.SettingsRevision {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	policy, err := s.ProjectPolicy(ctx, arg.ProjectID)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	if policy.PolicyID != arg.PolicyID || policy.PayloadSHA256 != arg.PolicySHA256 {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	targetFound, dispatchable := false, false
	for _, state := range project.States {
		if state.Name == arg.TargetState {
			targetFound, dispatchable = true, state.Dispatchable
			break
		}
	}
	if !targetFound {
		return eventstore.Receipt{}, eventstore.ErrInvalidRecord
	}
	current, err := s.LockWorkItem(ctx, arg.ProjectID, arg.WorkItemID)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	if current.Archived || current.Revision != arg.ExpectedRevision || current.EventSequence != arg.ExpectedEventSequence || !reflect.DeepEqual(current.NativeSourceKey, arg.ExpectedSourceKey) || stringValue(current.WorkflowStateName) != arg.ExpectedState || stringValue(current.ActorKind) != arg.Filer.Kind || stringValue(current.ActorPrincipalID) != arg.Filer.PrincipalID {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	source, err := s.CollaborationVersion(ctx, string(arg.WorkItemID), arg.ExpectedRevision)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	if source.ProjectID != arg.ProjectID || source.WorkItemID != arg.WorkItemID || source.PayloadSHA256 != arg.ExpectedVersionSHA256 {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	if arg.ExpectedState == arg.TargetState {
		return s.PutReceipt(ctx, receipt)
	}
	event, version := arg.Event, arg.Version
	if apply == nil || version.OrganizationID != s.organization || version.ProjectID != arg.ProjectID || version.WorkItemID != arg.WorkItemID || version.RecordID != string(arg.WorkItemID) || version.Revision != arg.ExpectedRevision+1 || event.OrganizationID != s.organization || event.ProjectID != arg.ProjectID || event.WorkItemID != arg.WorkItemID || event.Type != "workflow.transitioned" || event.EventRevision != version.Revision || event.FromState != arg.ExpectedState || event.ToState != arg.TargetState || event.ActorKind != arg.Filer.Kind || event.ActorPrincipalID != arg.Filer.PrincipalID || event.Sequence != 0 && event.Sequence != arg.ExpectedEventSequence+1 || !recordTime(event.RecordedAt).Equal(recordTime(arg.At)) {
		return eventstore.Receipt{}, eventstore.ErrInvalidRecord
	}
	latest, err := s.LatestAttempt(ctx, current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return eventstore.Receipt{}, err
	}
	if err == nil {
		if latest.LeaseID != arg.Fence.LeaseID || latest.FencingToken != arg.Fence.FencingToken {
			return eventstore.Receipt{}, tracker.ErrStaleFencingToken
		}
	} else if arg.Fence != (leasestore.Fence{}) {
		return eventstore.Receipt{}, tracker.ErrStaleFencingToken
	}
	if _, err := s.ActiveLeaseForIssue(ctx, current.ID); err == nil {
		return eventstore.Receipt{}, tracker.ErrLeaseConflict
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return eventstore.Receipt{}, err
	}
	if err := apply(ctx, s); err != nil {
		return eventstore.Receipt{}, err
	}
	changed, err := s.WorkItem(ctx, arg.ProjectID, arg.WorkItemID)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	if changed.ID != current.ID || changed.Revision != arg.ExpectedRevision+1 || changed.EventSequence != arg.ExpectedEventSequence || stringValue(changed.WorkflowStateName) != arg.TargetState {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	if _, err := s.CreateCollaborationVersion(ctx, version); err != nil {
		return eventstore.Receipt{}, err
	}
	if _, err := s.AppendEvent(ctx, event); err != nil {
		return eventstore.Receipt{}, err
	}
	if dispatchable {
		if err := s.RequestIssueDispatch(ctx, arg.ProjectID, arg.WorkItemID, arg.Fence, arg.At); err != nil {
			return eventstore.Receipt{}, err
		}
	}
	return s.PutReceipt(ctx, receipt)
}
