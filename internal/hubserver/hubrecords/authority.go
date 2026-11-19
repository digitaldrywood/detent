package hubrecords

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeVersionAuthority interface {
	Prepare(context.Context, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, time.Time) (ChangeVersionApproval, error)
	Authorize(context.Context, tenantstore.Writer, hubscope.NativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, ChangeVersionApproval, time.Time) error
}

type NativeRunEventAuthority struct {
	BatchThrough          int64
	Termination           bool
	Scope                 hubscope.NativeScope
	OrganizationID        tracker.OrganizationID
	ProjectID             tracker.ProjectID
	WorkItem              corestore.WorkItem
	Lease                 leasestore.Lease
	DispatchGeneration    int64
	UsageCorrection       bool
	ReplayPrefix          bool
	Publication           *PreparedNativePRPublication
	UsageCosts            []billingstore.CostObservation
	UsageReport           reportstore.AttemptUsage
	UsageFacts            *RunnerUsageFacts
	Landing               *NativeLandingAuthority
	Event                 tracker.NativeRunEvent
	RequireSelector       bool
	LegacySelectorChecked bool
}

type NativeLandingAuthority struct {
	Change eventstore.Change
	Method string
}

type PreparedNativePRPublication struct {
	Change   eventstore.Change
	Version  eventstore.Version
	Record   eventstore.NativePRPublication
	Diff     leasestore.AttemptDiff
	Retained bool
}

type RunnerUsageFacts struct {
	WorkItemID string
	RunnerID   string
	MachineID  string
	Placement  string
	StartedAt  time.Time
	EndedAt    time.Time
}

type NativeRunEventParticipant interface {
	ValidateNativeRunEvent(context.Context, tenantstore.Reader, *NativeRunEventAuthority) error
	ApplyNativeRunEvent(context.Context, tenantstore.Writer, NativeRunEventAuthority) error
	ValidateNativeLeaseMutation(context.Context, tenantstore.Reader, *NativeLeaseAuthority) error
	ApplyNativeLeaseMutation(context.Context, tenantstore.Writer, NativeLeaseAuthority) error
	NativeLeaseResponse(context.Context, tenantstore.Reader, hubscope.NativeScope, leasestore.Lease) (NativeLeaseResponseDetails, error)
}

type NativeLeaseAuthority struct {
	Scope                 hubscope.NativeScope
	WorkItem              corestore.WorkItem
	Lease                 leasestore.Lease
	Request               tracker.NativeLeaseMutation
	Operation             string
	LegacySelectorChecked bool
}

type NativeLeaseResponseDetails struct {
	WorkItemID          tracker.NativeWorkItemID
	PolicyID            string
	IsolationPolicy     *isolation.Policy
	ProviderReservation *providercapacity.Reservation
	DependencyFacts     *NativeClaimBlockerSummary
}
