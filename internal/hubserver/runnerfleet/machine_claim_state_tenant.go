package runnerfleet

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/attemptworkers"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d HeartbeatDeps) ReadRegistrationClaimStateTenant(ctx context.Context, scope hubscope.NativeScope, snapshot *runnerauth.RoutingSnapshot) error {
	key := CleanupCursorKey{Organization: scope.Organization, Project: scope.Project, Runner: snapshot.RunnerID}
	after, err := d.ReadCleanupCursor(key)
	if err != nil {
		return err
	}
	var page runnerstore.ClaimStatePage
	var candidates []hubrecords.TenantAttemptSnapshot
	err = d.ReadRunnerResponseSnapshot(ctx, scope.Organization, scope.Credential.ID, snapshot.RunnerID, scope.Project, func(ctx context.Context, r tenantstore.Reader, now time.Time) error {
		identity, err := attemptworkers.AuthorizeRunnerProjectOperation(ctx, r, scope, snapshot.RunnerID, runnerauth.Heartbeat, now)
		if err != nil {
			return err
		}
		if err := ReadRunnerResponseRouting(ctx, r, identity, snapshot); err != nil {
			return err
		}
		reader, ok := r.(runnerstore.ClaimStateReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		page, err = reader.RunnerClaimState(ctx, identity.RunnerID, scope.Project, after, ReleasedExecutionsPageSize, now)
		if err != nil {
			return err
		}
		candidates, err = d.ReadRunnerCleanupCandidates(ctx, r, scope, page.Unclassified)
		return err
	})
	if err != nil {
		return err
	}
	page.State.ReleasedExecutions = append(page.State.ReleasedExecutions, page.Candidates...)
	if err := d.ResolveRunnerCleanupCandidates(ctx, scope, snapshot.RunnerID, candidates, &page.State); err != nil {
		return err
	}
	d.AdvanceCleanupCursor(key, page.Next)
	snapshot.ClaimState = &page.State
	return nil
}

func (d HeartbeatDeps) RegistrationCleanupRequired(candidate hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData, runner string) (bool, error) {
	if !d.AttemptRunMatches(candidate, run) || run.RunnerID != runner {
		return false, artifact.ErrIntegrity
	}
	if candidate.Attempt.CleanupAcknowledgedAt != nil {
		return false, nil
	}

	return NativeRunCleanupRequired(run), nil
}

func NativeRunCleanupRequired(run tracker.NativeRunData) bool {
	if run.Identity == nil || run.Identity.Role == "" || run.Runtime == nil || run.Runtime.WorkerLaunch == nil {
		return false
	}
	launch := run.Runtime.WorkerLaunch
	return launch.PID > 0 && !launch.ProcessStart.IsZero()
}

func (d HeartbeatDeps) ReadRunnerCleanupCandidates(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, candidates []runnerauth.ReleasedExecution) ([]hubrecords.TenantAttemptSnapshot, error) {
	values := make([]hubrecords.TenantAttemptSnapshot, 0, len(candidates))
	for _, candidate := range candidates {
		var value hubrecords.TenantAttemptSnapshot
		err := func() error {
			var err error
			value.Attempt, err = r.Attempt(ctx, candidate.AttemptID)
			if err != nil {
				return err
			}
			value.Lease, err = r.Lease(ctx, value.Attempt.LeaseID)
			if err != nil {
				return err
			}
			value.Event, err = r.LatestRunEvent(ctx, scope.Project, candidate.AttemptID)
			if err != nil {
				return err
			}
			if value.Event.WorkItemID != candidate.WorkItemID || value.Lease.IssueID != value.Attempt.IssueID || value.Lease.ReleasedAt == nil || value.Lease.FencingToken != value.Attempt.FencingToken || value.Lease.MachineID != string(scope.Credential.Runner.MachineID) {
				return artifact.ErrIntegrity
			}
			return hubrecords.ReadTenantAttemptRunPayload(ctx, r, &value)
		}()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) {
				return nil, err
			}
			if errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) && value.Attempt.ID != "" && !value.Attempt.WorkerOutstanding {
				value.Event.WorkItemID = ""
				value.Payload = nil
				values = append(values, value)
				continue
			}
			if d.Logger != nil {
				d.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.Organization, "project_id", scope.Project, "attempt_id", candidate.AttemptID, "error", err)
			}
			continue
		}
		values = append(values, value)
	}
	return values, nil
}

func (d HeartbeatDeps) ResolveRunnerCleanupCandidates(ctx context.Context, scope hubscope.NativeScope, runner string, candidates []hubrecords.TenantAttemptSnapshot, state *runnerauth.ClaimState) error {
	for _, candidate := range candidates {
		if candidate.Event.WorkItemID == "" && candidate.Attempt.ID != "" && !candidate.Attempt.WorkerOutstanding {
			if err := d.Repository.WriteTenant(ctx, scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
				return w.SetAttemptRuntimeHeartbeat(ctx, candidate.Attempt.ID, candidate.Attempt.Sequence, nil, false)
			}); err != nil {
				return err
			}
			continue
		}
		run, err := d.ReadAttemptRun(ctx, candidate)
		if err == nil && (!d.AttemptRunMatches(candidate, run) || run.RunnerID != runner || run.MachineID != scope.Credential.Runner.MachineID) {
			err = artifact.ErrIntegrity
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) {
				return err
			}
			if errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) && !candidate.Attempt.WorkerOutstanding {
				if writeErr := d.Repository.WriteTenant(ctx, scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
					return w.SetAttemptRuntimeHeartbeat(ctx, candidate.Attempt.ID, candidate.Attempt.Sequence, nil, false)
				}); writeErr != nil {
					return writeErr
				}
				continue
			}
			if d.Logger != nil {
				d.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.Organization, "project_id", scope.Project, "attempt_id", candidate.Attempt.ID, "error", err)
			}
			continue
		}
		required := NativeRunCleanupRequired(run)
		var heartbeat *time.Time
		if run.Runtime != nil {
			value := run.Runtime.HeartbeatAt
			heartbeat = &value
		}
		if err := d.Repository.WriteTenant(ctx, scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
			return w.SetAttemptRuntimeHeartbeat(ctx, candidate.Attempt.ID, candidate.Attempt.Sequence, heartbeat, required)
		}); err != nil {
			return err
		}
		if required && candidate.Attempt.CleanupAcknowledgedAt == nil {
			state.ReleasedExecutions = append(state.ReleasedExecutions, runnerauth.ReleasedExecution{ProjectID: scope.Project, WorkItemID: candidate.Event.WorkItemID, AttemptID: candidate.Attempt.ID})
		}
	}
	slices.SortFunc(state.ReleasedExecutions, func(a, b runnerauth.ReleasedExecution) int { return strings.Compare(a.AttemptID, b.AttemptID) })
	return nil
}
