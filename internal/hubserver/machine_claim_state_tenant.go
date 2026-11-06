package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readRegistrationClaimStateTenant(ctx context.Context, scope nativeScope, snapshot *runnerauth.RoutingSnapshot) error {
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	key := releasedExecutionCursorKey{organization: scope.organization, project: scope.project, runner: snapshot.RunnerID}
	position, _ := s.cleanupPollCursors.Load(key)
	after, _ := position.(string)
	var page runnerstore.ClaimStatePage
	var candidates []tenantAttemptSnapshot
	err := repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		identity, err := r.RunnerIdentityByCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		credential, err := r.AccessCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		now := s.config.now().UTC()
		if credential.OrganizationID != scope.organization || credential.Scope != string(apiScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.credential.Hash, now) || !runnerIdentityMatchesHeldLease(identity, scope.organization, snapshot.RunnerID, scope.credential.ID, scope.credential.Runner.MachineID, now) || !slices.Contains(identity.Operations, runnerauth.Heartbeat) {
			return runnerUnauthorized()
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return nativeNotFound()
		}
		routingReader, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		record, err := routingReader.RunnerRoutingRecord(ctx, identity.RunnerID, now)
		if err != nil {
			return err
		}
		urgent, err := r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		runner := runnerFromRoutingRecord(record, now, urgent)
		snapshot.Revision, snapshot.Routing = runner.Revision, runner.Routing
		listing, err := r.RunnerRepositoryList(ctx, identity.RunnerID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil && listing.Pending {
			snapshot.RepositoryListRequest = listing.RequestID
		}
		reader, ok := r.(runnerstore.ClaimStateReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		page, err = reader.RunnerClaimState(ctx, identity.RunnerID, scope.project, after, releasedExecutionsPageSize, now)
		if err != nil {
			return err
		}
		for _, candidate := range page.Candidates {
			var value tenantAttemptSnapshot
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
			if value.event.WorkItemID != candidate.WorkItemID || value.lease.ReleasedAt == nil || value.lease.FencingToken != value.attempt.FencingToken || value.lease.MachineID != string(identity.MachineID) {
				return artifact.ErrIntegrity
			}
			if err := readTenantAttemptPayload(ctx, r, &value); err != nil {
				return err
			}
			candidates = append(candidates, value)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(candidates) > 0 && s.runDetailStore() == nil {
		return nativeRunEventStorageUnavailable()
	}
	for _, candidate := range candidates {
		run, err := s.readNativeAttemptRunTenant(ctx, candidate)
		if err != nil {
			return err
		}
		required, err := registrationCleanupRequired(candidate, run, snapshot.RunnerID)
		if err != nil {
			return err
		}
		if required {
			page.State.ReleasedExecutions = append(page.State.ReleasedExecutions, runnerauth.ReleasedExecution{ProjectID: scope.project, WorkItemID: candidate.event.WorkItemID, AttemptID: candidate.attempt.ID})
		}
	}
	if page.Next == "" {
		s.cleanupPollCursors.Delete(key)
	} else {
		s.cleanupPollCursors.Store(key, page.Next)
	}
	snapshot.ClaimState = &page.State
	return nil
}

func registrationCleanupRequired(candidate tenantAttemptSnapshot, run tracker.NativeRunData, runner string) (bool, error) {
	if !nativeAttemptRunMatches(candidate, run) || run.RunnerID != runner {
		return false, artifact.ErrIntegrity
	}

	if run.Identity == nil || run.Identity.Role == "" || run.Runtime == nil || run.Runtime.WorkerLaunch == nil {
		return false, nil
	}
	launch := run.Runtime.WorkerLaunch
	return launch.PID > 0 && !launch.ProcessStart.IsZero(), nil
}
