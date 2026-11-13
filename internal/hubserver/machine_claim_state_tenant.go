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
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readRegistrationClaimStateTenant(ctx context.Context, scope hubscope.NativeScope, snapshot *runnerauth.RoutingSnapshot) error {
	key := releasedExecutionCursorKey{organization: scope.Organization, project: scope.Project, runner: snapshot.RunnerID}
	after, err := s.runnerCleanupCursor(key)
	if err != nil {
		return err
	}
	var page runnerstore.ClaimStatePage
	var candidates []hubrecords.TenantAttemptSnapshot
	err = s.readRunnerResponseSnapshot(ctx, scope.Organization, scope.Credential.ID, snapshot.RunnerID, scope.Project, func(ctx context.Context, r tenantstore.Reader, now time.Time) error {
		identity, err := r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		credential, err := r.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.Organization, snapshot.RunnerID, scope.Credential.ID, scope.Credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return hubscope.RunnerUnauthorized()
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
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
		page, err = reader.RunnerClaimState(ctx, identity.RunnerID, scope.Project, after, releasedExecutionsPageSize, now)
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

func registrationCleanupRequired(candidate hubrecords.TenantAttemptSnapshot, run tracker.NativeRunData, runner string) (bool, error) {
	if !nativeAttemptRunMatches(candidate, run) || run.RunnerID != runner {
		return false, artifact.ErrIntegrity
	}
	if candidate.Attempt.CleanupAcknowledgedAt != nil {
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

func (s *Service) readRunnerCleanupCandidates(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, candidates []runnerauth.ReleasedExecution) ([]hubrecords.TenantAttemptSnapshot, error) {
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
			if s.config.Logger != nil {
				s.config.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.Organization, "project_id", scope.Project, "attempt_id", candidate.AttemptID, "error", err)
			}
			continue
		}
		values = append(values, value)
	}
	return values, nil
}

func (s *Service) resolveRunnerCleanupCandidates(ctx context.Context, scope hubscope.NativeScope, runner string, candidates []hubrecords.TenantAttemptSnapshot, state *runnerauth.ClaimState) error {
	for _, candidate := range candidates {
		run, err := s.readNativeAttemptRunTenant(ctx, candidate)
		if err == nil && (!nativeAttemptRunMatches(candidate, run) || run.RunnerID != runner || run.MachineID != scope.Credential.Runner.MachineID) {
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
				s.config.Logger.Warn("runner cleanup proof unavailable", "organization_id", scope.Organization, "project_id", scope.Project, "attempt_id", candidate.Attempt.ID, "error", err)
			}
			continue
		}
		required := nativeRunCleanupRequired(run)
		var heartbeat *time.Time
		if run.Runtime != nil {
			value := run.Runtime.HeartbeatAt
			heartbeat = &value
		}
		if err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Background, func(w tenantstore.Writer) error {
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
