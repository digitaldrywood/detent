package hubserver

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storeRunnerProjectObservationTenant(ctx context.Context, scope nativeScope, view runnerauth.ProjectConfiguration, receivedAt time.Time, writer runnerstore.Writer) error {
	if view.ProjectID != string(scope.project) || view.ObservedAt.IsZero() {
		return runnerstore.ErrInvalidRecord
	}
	digest, err := projectConfigurationDigest(view)
	if err != nil {
		return err
	}
	observation := runnerstore.ProjectObservation{
		OrganizationID: scope.organization,
		RunnerID:       scope.credential.Runner.RunnerID,
		ProjectID:      tracker.ProjectID(view.ProjectID),
		ObservedAt:     receivedAt,
		SnapshotDigest: digest,
	}
	var attempts []runnerstore.ProjectDiagnosticAttempt
	if view.Diagnostics != nil {
		diagnosticsAt := view.Diagnostics.ObservedAt
		observation.DiagnosticsAt = &diagnosticsAt
		if !view.Diagnostics.RuntimeObservedAt.IsZero() {
			runtimeAt := view.Diagnostics.RuntimeObservedAt
			observation.RuntimeObservedAt = &runtimeAt
		}
		attempts = make([]runnerstore.ProjectDiagnosticAttempt, 0, len(view.Diagnostics.Records))
		for position, record := range view.Diagnostics.Records {
			if record.Generation > math.MaxInt64 {
				return runnerstore.ErrInvalidRecord
			}
			attempt := runnerstore.ProjectDiagnosticAttempt{
				Position: position, RecordKey: record.Key, IssueID: record.IssueID, LocalAttemptID: record.LocalAttemptID,
				NativeAttemptID: record.NativeAttemptID, Generation: int64(record.Generation), Fence: record.Fence, Stage: record.Stage,
				Running: slices.Contains(record.Membership, "running"), NativeClaim: slices.Contains(record.Membership, "native_claim"),
				HostOperation: record.HostOperation, OperationPending: record.OperationPending,
			}
			if !record.ProviderCompletedAt.IsZero() {
				completedAt := record.ProviderCompletedAt
				attempt.ProviderCompletedAt = &completedAt
			}
			if !record.LeaseRenewedAt.IsZero() {
				renewedAt := record.LeaseRenewedAt
				attempt.LeaseRenewedAt = &renewedAt
			}
			attempts = append(attempts, attempt)
		}
	}
	return writer.ReplaceRunnerProjectObservation(ctx, observation, attempts)
}
