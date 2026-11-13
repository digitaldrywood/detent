package hubserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readRegistrationClaimStateTenant(ctx context.Context, scope nativeScope, snapshot *runnerauth.RoutingSnapshot) error {
	key := releasedExecutionCursorKey{organization: scope.organization, project: scope.project, runner: snapshot.RunnerID}
	after, err := s.runnerCleanupCursor(key)
	if err != nil {
		return err
	}
	var page runnerstore.ClaimStatePage
	var candidates []tenantAttemptSnapshot
	err = s.readRunnerResponseSnapshot(ctx, scope.organization, scope.credential.ID, snapshot.RunnerID, scope.project, func(ctx context.Context, r tenantstore.Reader, now time.Time) error {
		identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		credential, err := r.AccessCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		if credential.OrganizationID != scope.organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.organization, snapshot.RunnerID, scope.credential.ID, scope.credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return runnerUnauthorized()
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return hubapi.NativeNotFound()
		}
		if err := readRunnerResponseRouting(ctx, r, identity, snapshot); err != nil {
			return err
		}
		reader, ok := r.(runnerstore.ClaimStateReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		page, err = reader.RunnerClaimState(ctx, identity.RunnerID, scope.project, after, releasedExecutionsPageSize, now)
		if err != nil {
			return err
		}
		candidates, err = s.readRunnerCleanupCandidates(ctx, r, scope, page.Unclassified)
		return err
	})
	if err != nil {
		return err
	}
	page.State.ReleasedExecutions = append(page.State.ReleasedExecutions, page.Candidates...)
	if err := s.resolveRunnerCleanupCandidates(ctx, scope, snapshot.RunnerID, candidates, &page.State); err != nil {
		return err
	}
	s.advanceRunnerCleanupCursor(key, page.Next)
	snapshot.ClaimState = &page.State
	return nil
}

func registrationCleanupRequired(candidate tenantAttemptSnapshot, run tracker.NativeRunData, runner string) (bool, error) {
	if !nativeAttemptRunMatches(candidate, run) || run.RunnerID != runner {
		return false, artifact.ErrIntegrity
	}
	if candidate.attempt.CleanupAcknowledgedAt != nil {
		return false, nil
	}

	return nativeRunCleanupRequired(run), nil
}

func nativeRunCleanupRequired(run tracker.NativeRunData) bool {
	if run.Identity == nil || run.Identity.Role == "" || run.Runtime == nil || run.Runtime.WorkerLaunch == nil {
		return false
	}
	launch := run.Runtime.WorkerLaunch
	return launch.PID > 0 && !launch.ProcessStart.IsZero()
}

func (s *Service) runnerCleanupCursor(key releasedExecutionCursorKey) (string, error) {
	if position, found := s.cleanupPollCursors.Load(key); found {
		after, ok := position.(string)
		if !ok {
			return "", errors.New("runner cleanup cursor has invalid type")
		}
		return after, nil
	}
	return "", nil
}

func (s *Service) advanceRunnerCleanupCursor(key releasedExecutionCursorKey, next string) {
	if next == "" {
		s.cleanupPollCursors.Delete(key)
	} else {
		s.cleanupPollCursors.Store(key, next)
	}
}

func (s *Service) readRunnerCleanupCandidates(ctx context.Context, r tenantstore.Reader, scope nativeScope, candidates []runnerauth.ReleasedExecution) ([]tenantAttemptSnapshot, error) {
	values := make([]tenantAttemptSnapshot, 0, len(candidates))
	for _, candidate := range candidates {
		var value tenantAttemptSnapshot
		err := func() error {
			var err error
			value.attempt, err = r.Attempt(ctx, candidate.AttemptID)
			if err != nil {
				return err
			}
			value.lease, err = r.Lease(ctx, value.attempt.LeaseID)
			if err != nil {
				return err
			}
			value.event, err = r.LatestRunEvent(ctx, scope.project, candidate.AttemptID)
			if err != nil {
				return err
			}
			if value.event.WorkItemID != candidate.WorkItemID || value.lease.IssueID != value.attempt.IssueID || value.lease.ReleasedAt == nil || value.lease.FencingToken != value.attempt.FencingToken || value.lease.MachineID != string(scope.credential.Runner.MachineID) {
				return artifact.ErrIntegrity
			}
			return readTenantAttemptRunPayload(ctx, r, &value)
		}()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) {
				return nil, err
			}
			if s.config.Logger != nil {
				s.config.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.organization, "project_id", scope.project, "attempt_id", candidate.AttemptID, "error", err)
			}
			continue
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Service) resolveRunnerCleanupCandidates(ctx context.Context, scope nativeScope, runner string, candidates []tenantAttemptSnapshot, state *runnerauth.ClaimState) error {
	for _, candidate := range candidates {
		run, err := s.readNativeAttemptRunTenant(ctx, candidate)
		if err == nil && (!nativeAttemptRunMatches(candidate, run) || run.RunnerID != runner || run.MachineID != scope.credential.Runner.MachineID) {
			err = artifact.ErrIntegrity
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, artifact.ErrIntegrity) {
				return err
			}
			if s.config.Logger != nil {
				s.config.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.organization, "project_id", scope.project, "attempt_id", candidate.attempt.ID, "error", err)
			}
			continue
		}
		required := nativeRunCleanupRequired(run)
		var heartbeat *time.Time
		if run.Runtime != nil {
			value := run.Runtime.HeartbeatAt
			heartbeat = &value
		}
		if err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Background, func(w tenantstore.Writer) error {
			return w.SetAttemptRuntimeHeartbeat(ctx, candidate.attempt.ID, candidate.attempt.Sequence, heartbeat, required)
		}); err != nil {
			return err
		}
		if required && candidate.attempt.CleanupAcknowledgedAt == nil {
			state.ReleasedExecutions = append(state.ReleasedExecutions, runnerauth.ReleasedExecution{ProjectID: scope.project, WorkItemID: candidate.event.WorkItemID, AttemptID: candidate.attempt.ID})
		}
	}
	slices.SortFunc(state.ReleasedExecutions, func(a, b runnerauth.ReleasedExecution) int { return strings.Compare(a.AttemptID, b.AttemptID) })
	return nil
}
