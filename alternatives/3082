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
		diagnostics := *view.Diagnostics
		diagnostics.Records = slices.Clone(view.Diagnostics.Records)
		diagnostics.Admissions = slices.Clone(view.Diagnostics.Admissions)
		pruneExpiredProjectDiagnostics(&diagnostics, receivedAt)
		diagnostics.Counts = cloneDiagnosticCounts(view.Diagnostics.Counts)
		diagnostics.Unavailable = cloneDiagnosticUnavailable(view.Diagnostics.Unavailable)
		for i := range diagnostics.Records {
			diagnostics.Records[i].Unavailable = cloneDiagnosticUnavailable(diagnostics.Records[i].Unavailable)
			diagnostics.Records[i].LatestError = runnerauth.DiagnosticError(diagnostics.Records[i].LatestError)
		}
		for i := range diagnostics.Admissions {
			diagnostics.Admissions[i].Unavailable = cloneDiagnosticUnavailable(view.Diagnostics.Admissions[i].Unavailable)
			diagnostics.Admissions[i].RepositoryReservations = slices.Clone(view.Diagnostics.Admissions[i].RepositoryReservations)
		}
		observation.Diagnostics = &diagnostics
		diagnosticsAt := diagnostics.ObservedAt
		observation.DiagnosticsAt = &diagnosticsAt
		if !diagnostics.RuntimeObservedAt.IsZero() {
			runtimeAt := diagnostics.RuntimeObservedAt
			observation.RuntimeObservedAt = &runtimeAt
		}
		attempts = make([]runnerstore.ProjectDiagnosticAttempt, 0, len(diagnostics.Records))
		for position, record := range diagnostics.Records {
			if record.Generation > math.MaxInt64 {
				return runnerstore.ErrInvalidRecord
			}
			attempt := runnerstore.ProjectDiagnosticAttempt{
				Position: position, RecordKey: record.Key, IssueID: record.IssueID, LocalAttemptID: record.LocalAttemptID,
				NativeAttemptID: record.NativeAttemptID, Generation: int64(record.Generation), Fence: record.Fence, Stage: record.Stage,
				Running: slices.Contains(record.Membership, "running"), NativeClaim: slices.Contains(record.Membership, "native_claim"),
				HostOperation: record.HostOperation, OperationPending: record.OperationPending, OperationStartedAt: diagnosticTime(record.OperationStartedAt),
				OperationObservedAt: diagnosticTime(record.OperationObservedAt), LatestErrorCode: record.LatestErrorCode, LatestError: record.LatestError,
				DeferredAt: diagnosticTime(record.DeferredAt), RetryAt: diagnosticTime(record.RetryAt), RetryAttempt: record.RetryAttempt,
				RecoveryOwner: record.RecoveryOwner, LeaseExpiresAt: diagnosticTime(record.LeaseExpiresAt),
				Membership: slices.Clone(record.Membership), Unavailable: cloneDiagnosticUnavailable(record.Unavailable),
			}
			attempt.ProviderCompletedAt = diagnosticTime(record.ProviderCompletedAt)
			attempt.LeaseRenewedAt = diagnosticTime(record.LeaseRenewedAt)
			attempts = append(attempts, attempt)
		}
	}
	return writer.ReplaceRunnerProjectObservation(ctx, observation, attempts)
}

func diagnosticTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func cloneDiagnosticCounts(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	result := make(map[string]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneDiagnosticUnavailable(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
