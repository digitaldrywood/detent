package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readRunnerSessionPayload(ctx context.Context, record runnersession.Record) (runnersession.Snapshot, error) {
	raw, err := s.runDetailStore().ReadDetail(ctx, string(record.Organization), record.Reference)
	if err != nil {
		return runnersession.Snapshot{}, err
	}
	var value runnersession.Snapshot
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	value.ID, value.Revision = record.ID, record.Revision
	return value, nil
}

func (s *Service) runnerSessionRead(ctx context.Context, scope nativeScope, id int64, lookup *runnersession.ResumeLookup) (runnersession.Record, error) {
	var record runnersession.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		if lookup == nil {
			record, err = reader.RunnerSession(ctx, scope.project, id)
		} else {
			var attemptID string
			if lookup.Lookup.NativeAttemptID != "" {
				item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(lookup.Lookup.IssueID))
				if err != nil {
					return err
				}
				source, err := r.Attempt(ctx, lookup.Lookup.NativeAttemptID)
				if err != nil {
					return err
				}
				if source.IssueID != item.ID {
					return nativeNotFound()
				}
				attemptID = source.ID
			} else if lookup.CurrentAttempt || lookup.Lookup.WorkAttemptID > 0 {
				lease, err := r.Lease(ctx, string(lookup.LeaseID))
				if err != nil {
					return err
				}
				item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(lookup.Lookup.IssueID))
				if err != nil {
					return err
				}
				now, err := reader.RunnerSessionTime(ctx)
				if err != nil {
					return err
				}
				if lease.FencingToken != int64(lookup.FencingToken) || lease.IssueID != item.ID || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
					return tracker.ErrStaleFencingToken
				}
				attempt, err := r.LatestAttempt(ctx, item.ID)
				if err != nil {
					return err
				}
				if attempt.LeaseID != lease.LeaseID {
					return tracker.ErrStaleFencingToken
				}
				attemptID = attempt.ID
			}
			record, err = reader.FindRunnerSession(ctx, scope.project, lookup.Lookup.IssueID, attemptID, lookup.Lookup)
		}
		if err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(record.IssueID))
		if err == nil && !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(item.ActorPrincipalID)}) {
			return nativeNotFound()
		}
		return err
	})
	return record, err
}

func (s *Service) getRunnerSession(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	record, err := s.runnerSessionRead(ctx, nativeRequestScope(c), id, nil)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.readRunnerSessionPayload(ctx, record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) findRunnerSession(c echo.Context) error {
	var lookup runnersession.ResumeLookup
	if err := decodeAPIJSON(c, &lookup); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	record, err := s.runnerSessionRead(ctx, nativeRequestScope(c), 0, &lookup)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.readRunnerSessionPayload(ctx, record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) runnerSessionFence(ctx context.Context, r tenantstore.Reader, scope nativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error) {
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	owner, err := r.RunnerForLease(ctx, string(request.LeaseID))
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	var lease leasestore.Lease
	if lock {
		locker, ok := r.(leasestore.FencedLeaseLocker)
		if !ok {
			return leasestore.Attempt{}, policystore.Revision{}, nativeRunEventStorageUnavailable()
		}
		lease, err = locker.LockLease(ctx, string(request.LeaseID))
	} else {
		lease, err = r.Lease(ctx, string(request.LeaseID))
	}
	if err != nil {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	item, err := r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(issue))
	if err != nil {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	clock, ok := r.(runnersession.Reader)
	if !ok {
		return leasestore.Attempt{}, policystore.Revision{}, nativeRunEventStorageUnavailable()
	}
	now, err := clock.RunnerSessionTime(ctx)
	if err != nil {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	if lease.IssueID != item.ID || lease.FencingToken != int64(request.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || scope.credential.Runner.RunnerID == "" || lease.MachineID != string(scope.credential.Runner.MachineID) {
		return leasestore.Attempt{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	if owner.RunnerID != scope.credential.Runner.RunnerID {
		return leasestore.Attempt{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	attempt, err := r.LatestAttempt(ctx, item.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return leasestore.Attempt{}, policystore.Revision{}, err
	}
	if err != nil || attempt.LeaseID != lease.LeaseID || attempt.Status != "running" {
		return leasestore.Attempt{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	bound, err := r.LeasePolicy(ctx, request.LeaseID)
	if err != nil {
		return attempt, policystore.Revision{}, err
	}
	current, err := r.ProjectPolicy(ctx, scope.project)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return attempt, current, err
	}
	if err != nil || current.PolicyID != bound.PolicyID {
		return attempt, current, tracker.ErrStaleFencingToken
	}
	return attempt, current, nil
}

func runnerSessionProcessAuthority(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Service) mutateRunnerSession(c echo.Context) error {
	var request runnersession.Mutation
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.Key == "" || len(request.Key) > 128 || request.LeaseID == "" || request.FencingToken < 1 || request.Policy != nil {
		return s.nativeAPIError(c, nativePublicInvalid("session mutation requires a key and execution fence"))
	}
	count := 0
	for _, present := range []bool{request.Start != nil, request.Finish != nil, request.Identity != nil, request.Provider != nil, request.Resume != nil, request.Progress != nil, request.Phase != nil, request.Profile != nil, request.Usage != nil, request.Worker != nil, request.Reaped != nil} {
		if present {
			count++
		}
	}
	if count != 1 || request.Profile != nil && request.Phase != nil || request.Progress != nil && request.Progress.LastProgressAt.IsZero() {
		return s.nativeAPIError(c, nativePublicInvalid("one typed session mutation is required"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	var record runnersession.Record
	var snapshot runnersession.Snapshot
	if request.Start != nil {
		if request.SessionID != 0 || request.ExpectedRevision != 0 || request.Start.StartedAt.IsZero() || request.Start.IssueID == "" || len(request.ProcessKey) < 32 {
			return s.nativeAPIError(c, nativePublicInvalid("new session requires an issue, start time and process key"))
		}
		record = runnersession.Record{Organization: scope.organization, Project: scope.project, IssueID: request.Start.IssueID, StartKey: request.Key, StartedAt: request.Start.StartedAt, Revision: 1}
		snapshot.Start, snapshot.Identity = *request.Start, request.Start.RuntimeIdentity
		snapshot.Resume = store.SessionResumeState{ResumedFromSessionID: request.Start.ResumedFromSessionID, OrphanRecoveryOutcome: request.Start.OrphanRecoveryOutcome, OrphanRecoveryFallbackReason: request.Start.OrphanRecoveryFallbackReason}
		snapshot.Provider = store.SessionProviderIdentity{ThreadID: request.Start.ProviderThreadID, SessionID: request.Start.ProviderSessionID}
		snapshot.Authority = runnerSessionProcessAuthority(request.ProcessKey)

	}
	command := eventstore.RunCommand{Project: scope.project, WorkItem: tracker.NativeWorkItemID(record.IssueID), LeaseID: string(request.LeaseID), SessionID: request.SessionID}
	var attempt leasestore.Attempt
	var approved policystore.Revision
	err := s.readNativeRunCommand(ctx, scope, command, func(ctx context.Context, r tenantstore.Reader) error {
		if request.Start == nil {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			reader, ok := r.(runnersession.Reader)
			if !ok {
				return nativeRunEventStorageUnavailable()
			}
			var err error
			record, err = reader.RunnerSession(ctx, scope.project, request.SessionID)
			if err != nil {
				return err
			}
			if request.ExpectedRevision != record.Revision {
				return corestore.ErrRevisionConflict
			}
		}
		var err error
		attempt, approved, err = s.runnerSessionFence(ctx, r, scope, request, record.IssueID, false)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	command.WorkItem = tracker.NativeWorkItemID(record.IssueID)
	if request.Start == nil {
		snapshot, err = s.readRunnerSessionPayload(ctx, record)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		record.Revision++
	}

	if request.Start != nil {
		record.AttemptID, record.PolicyID = attempt.ID, approved.PolicyID
		snapshot.Policy, err = eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, approved)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	} else if record.AttemptID != attempt.ID {
		return s.nativeAPIError(c, tracker.ErrStaleFencingToken)
	}
	if request.Identity != nil {
		snapshot.Identity = *request.Identity
	}
	if request.Provider != nil {
		if request.Provider.ThreadID != "" {
			snapshot.Provider.ThreadID = request.Provider.ThreadID
		}
		if request.Provider.SessionID != "" {
			snapshot.Provider.SessionID = request.Provider.SessionID
		}
	}
	if request.Resume != nil {
		snapshot.Resume = *request.Resume
	}
	if (request.Worker != nil || request.Reaped != nil) && (snapshot.Authority == "" || subtle.ConstantTimeCompare([]byte(snapshot.Authority), []byte(runnerSessionProcessAuthority(request.ProcessKey))) != 1) {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	request.ProcessKey = ""
	if request.Worker != nil {
		reserved := request.Worker.PID == 0 && request.Worker.GroupID == 0 && request.Worker.StartedAt.IsZero() && request.Worker.CleanupRoot != "" && request.Worker.CleanupPath != ""
		started := request.Worker.PID > 0 && request.Worker.GroupID > 0 && !request.Worker.StartedAt.IsZero()
		if !reserved && !started {
			return s.nativeAPIError(c, nativePublicInvalid("worker process requires PID, group and start identity"))
		}
		snapshot.Worker = request.Worker
		snapshot.Reaped = nil
		record.WorkerPID, record.WorkerGroupID = request.Worker.PID, request.Worker.GroupID
		record.WorkerStartedAt = nil
		if started {
			record.WorkerStartedAt = &request.Worker.StartedAt
		}
		record.WorkerReapedAt = nil
	}
	if request.Reaped != nil {
		if snapshot.Worker == nil || request.Reaped.ReapedAt.IsZero() {
			return s.nativeAPIError(c, nativePublicInvalid("process reap requires a registered process and timestamp"))
		}
		snapshot.Reaped = request.Reaped
		record.WorkerReapedAt = &request.Reaped.ReapedAt
	}
	if request.Progress != nil {
		snapshot.Progress = request.Progress
	}
	if request.Finish != nil {
		if request.Finish.CompletedAt.IsZero() {
			return s.nativeAPIError(c, nativePublicInvalid("session finish requires completion time"))
		}
		snapshot.Finish = request.Finish
		record.CompletedAt = &request.Finish.CompletedAt
		if !request.Finish.RuntimeIdentity.IsZero() {
			snapshot.Identity = request.Finish.RuntimeIdentity
		}
		if request.Finish.ResumedFromSessionID > 0 {
			snapshot.Resume.ResumedFromSessionID = request.Finish.ResumedFromSessionID
		}
		if request.Finish.ProviderThreadID != "" {
			snapshot.Provider.ThreadID = request.Finish.ProviderThreadID
		}
		if request.Finish.ProviderSessionID != "" {
			snapshot.Provider.SessionID = request.Finish.ProviderSessionID
		}
	}
	record.BackendID, record.BackendKind, record.Role, record.Model = snapshot.Start.AgentBackendID, snapshot.Start.AgentBackendKind, snapshot.Start.AgentRole, snapshot.Start.RequestedModel
	if record.Model == "" {
		record.Model = snapshot.Start.Model
	}
	record.PRNumber, record.PRHeadSHA, record.PRBaseSHA = snapshot.Start.PRNumber, snapshot.Start.PRHeadSHA, snapshot.Start.PRBaseSHA
	record.ProviderThreadID, record.ProviderSessionID = snapshot.Provider.ThreadID, snapshot.Provider.SessionID
	record.FinalState = "running"
	if snapshot.Finish != nil {
		record.FinalState = snapshot.Finish.FinalState
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	record.Reference, err = s.runDetailStore().StoreDetail(ctx, string(scope.organization), raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateNativeDetailReference(ctx, s.runDetailStore(), scope.organization, record.Reference, raw); err != nil {
		return s.nativeAPIError(c, err)
	}
	historyRaw, err := json.Marshal(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	historyRef, err := s.runDetailStore().StoreDetail(ctx, string(scope.organization), historyRaw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateNativeDetailReference(ctx, s.runDetailStore(), scope.organization, historyRef, historyRaw); err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.writeNativeRunCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
		locked, policy, err := s.runnerSessionFence(ctx, w, scope, request, record.IssueID, true)
		if err != nil {
			return err
		}
		if locked.ID != record.AttemptID || policy.PolicyID != record.PolicyID {
			return tracker.ErrStaleFencingToken
		}
		writer, ok := w.(runnersession.Writer)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		if request.Start != nil {
			record, err = writer.CreateRunnerSession(ctx, record, runnersession.History{Key: request.Key, Reference: historyRef})
			return err
		}
		record.ReceiptID, err = writer.UpdateRunnerSession(ctx, record, runnersession.History{SessionID: record.ID, Revision: record.Revision, Key: request.Key, Reference: historyRef})
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	snapshot.ID, snapshot.Revision = record.ID, record.Revision
	snapshot.ReceiptID = record.ReceiptID
	return c.JSON(http.StatusOK, snapshot)
}

func (s *Service) getRunnerSessionHistory(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeRequestScope(c)
	if _, err := s.runnerSessionRead(ctx, scope, id, nil); err != nil {
		return s.nativeAPIError(c, err)
	}
	var entries []runnersession.History
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		entries, err = reader.RunnerSessionHistory(ctx, scope.project, id, revision-1, 1)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(entries) != 1 || entries[0].Revision != revision {
		return s.nativeAPIError(c, nativeNotFound())
	}
	raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.organization), entries[0].Reference)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) getRunnerSessionProcesses(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	if scope.credential.Runner.RunnerID == "" {
		return s.nativeAPIError(c, runnerUnauthorized())
	}
	var records []runnersession.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		var err error
		records, err = reader.RunnerSessionProcesses(ctx, scope.project, string(scope.credential.Runner.MachineID), c.QueryParam("attempt"), 200)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result := runnersession.Processes{More: len(records) == 200}
	for _, record := range records {
		value, err := s.readRunnerSessionPayload(ctx, record)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if value.Worker == nil {
			result.Unobserved = true
			continue
		}
		if value.Worker.PID < 1 || value.Worker.StartedAt.IsZero() {
			result.Unobserved = true
		}
		result.Workers = append(result.Workers, store.WorkerProcess{SessionID: record.ID, IssueID: record.IssueID, WorkerProcessIdentity: value.Worker.WorkerProcessIdentity, CleanupRoot: value.Worker.CleanupRoot, CleanupPath: value.Worker.CleanupPath})
	}
	return c.JSON(http.StatusOK, result)
}
