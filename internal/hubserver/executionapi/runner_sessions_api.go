package executionapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SessionDeps struct {
	ValidateDetailReference func(context.Context, attachment.DetailStore, tracker.OrganizationID, attachment.ArchiveReference, []byte) error
	RegisterWorkerProcess   func(context.Context, tenantstore.Writer, hubscope.NativeScope, runnersession.Record, runnersession.Snapshot, attachment.ArchiveReference, leasestore.Fence) error
	Repository              tenantstore.Repository
	HTTPError               func(echo.Context, error, ...any) error
	InvalidRequest          func(echo.Context, error) error
	Details                 func() attachment.DetailStore
	AuthorizeAccess         func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	ReadCommand             func(context.Context, hubscope.NativeScope, eventstore.RunCommand, func(context.Context, tenantstore.Reader) error) error
	WriteCommand            func(context.Context, hubscope.NativeScope, eventstore.RunCommand, func(context.Context, tenantstore.Writer) error) error
}

func (d SessionDeps) ReadRunnerSessionPayload(ctx context.Context, record runnersession.Record) (runnersession.Snapshot, error) {
	raw, err := d.Details().ReadDetail(ctx, string(record.Organization), record.Reference)
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

func (d SessionDeps) runnerSessionRead(ctx context.Context, scope hubscope.NativeScope, id int64, lookup *runnersession.ResumeLookup) (runnersession.Record, error) {
	var record runnersession.Record
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		if lookup == nil {
			record, err = reader.RunnerSession(ctx, scope.Project, id)
		} else {
			var attemptID string
			if lookup.Lookup.NativeAttemptID != "" {
				item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(lookup.Lookup.IssueID))
				if err != nil {
					return err
				}
				source, err := r.Attempt(ctx, lookup.Lookup.NativeAttemptID)
				if err != nil {
					return err
				}
				if source.IssueID != item.ID {
					return hubapi.NativeNotFound()
				}
				attemptID = source.ID
			} else if lookup.CurrentAttempt || lookup.Lookup.WorkAttemptID > 0 {
				lease, err := r.Lease(ctx, string(lookup.LeaseID))
				if err != nil {
					return err
				}
				item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(lookup.Lookup.IssueID))
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
			record, err = reader.FindRunnerSession(ctx, scope.Project, lookup.Lookup.IssueID, attemptID, lookup.Lookup)
		}
		if err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(record.IssueID))
		if err == nil && !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(item.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		return err
	})
	return record, err
}

func (d SessionDeps) GetRunnerSession(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	record, err := d.runnerSessionRead(ctx, hubscope.NativeRequestScope(c), id, nil)
	if err != nil {
		return d.HTTPError(c, err)
	}
	value, err := d.ReadRunnerSessionPayload(ctx, record)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (d SessionDeps) FindRunnerSession(c echo.Context) error {
	var lookup runnersession.ResumeLookup
	if err := hubapi.DecodeAPIJSON(c, &lookup); err != nil {
		return d.InvalidRequest(c, err)
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	record, err := d.runnerSessionRead(ctx, hubscope.NativeRequestScope(c), 0, &lookup)
	if err != nil {
		return d.HTTPError(c, err)
	}
	value, err := d.ReadRunnerSessionPayload(ctx, record)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (d SessionDeps) RunnerSessionFence(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error) {
	attempt, _, current, err := d.RunnerSessionFenceHeld(ctx, r, scope, request, issue, lock, nil)
	return attempt, current, err
}

type HeldRunnerSessionAuthority struct {
	Owner runnerstore.Identity
}

func (d SessionDeps) RunnerSessionFenceHeld(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool, held *HeldRunnerSessionAuthority) (leasestore.Attempt, corestore.WorkItem, policystore.Revision, error) {
	var owner runnerstore.Identity
	if held != nil {
		owner = held.Owner
	} else {
		if err := d.AuthorizeAccess(ctx, r, scope, true); err != nil {
			return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
		}
		var err error
		owner, err = r.RunnerForLease(ctx, string(request.LeaseID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
		}
	}
	var err error
	var lease leasestore.Lease
	if lock {
		locker, ok := r.(leasestore.FencedLeaseLocker)
		if !ok {
			return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, hubapi.NativeRunEventStorageUnavailable()
		}
		lease, err = locker.LockLease(ctx, string(request.LeaseID))
	} else {
		lease, err = r.Lease(ctx, string(request.LeaseID))
	}
	if err != nil {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
	}
	item, err := r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(issue))
	if err != nil {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
	}
	clock, ok := r.(runnersession.Reader)
	if !ok {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, hubapi.NativeRunEventStorageUnavailable()
	}
	now, err := clock.RunnerSessionTime(ctx)
	if err != nil {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
	}
	if lease.IssueID != item.ID || lease.FencingToken != int64(request.FencingToken) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) || scope.Credential.Runner.RunnerID == "" || lease.MachineID != string(scope.Credential.Runner.MachineID) {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	if owner.RunnerID != scope.Credential.Runner.RunnerID {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	attempt, err := r.LatestAttempt(ctx, item.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, err
	}
	if err != nil || attempt.LeaseID != lease.LeaseID || attempt.Status != "running" {
		return leasestore.Attempt{}, corestore.WorkItem{}, policystore.Revision{}, tracker.ErrStaleFencingToken
	}
	bound, err := r.LeasePolicy(ctx, request.LeaseID)
	if err != nil {
		return attempt, item, policystore.Revision{}, err
	}
	current, err := r.ProjectPolicyRevision(ctx, scope.Project, bound.PolicyID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return attempt, item, current, err
	}
	if err != nil || bound.OrganizationID != scope.Organization || bound.ProjectID != scope.Project || bound.LeaseID != request.LeaseID || current.PolicyID != bound.PolicyID {
		return attempt, item, current, tracker.ErrStaleFencingToken
	}
	return attempt, item, current, nil
}

func RunnerSessionProcessAuthority(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (d SessionDeps) MutateRunnerSession(c echo.Context) error {
	var request runnersession.Mutation
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return d.InvalidRequest(c, err)
	}
	if request.Key == "" || len(request.Key) > 128 || request.LeaseID == "" || request.FencingToken < 1 || request.Policy != nil {
		return d.HTTPError(c, hubapi.NativePublicInvalid("session mutation requires a key and execution fence"))
	}
	count := 0
	for _, present := range []bool{request.Start != nil, request.Finish != nil, request.Identity != nil, request.Provider != nil, request.Resume != nil, request.Progress != nil, request.Phase != nil, request.Profile != nil, request.Usage != nil, request.Worker != nil, request.Reaped != nil} {
		if present {
			count++
		}
	}
	if count != 1 || request.Profile != nil && request.Phase != nil || request.Progress != nil && request.Progress.LastProgressAt.IsZero() {
		return d.HTTPError(c, hubapi.NativePublicInvalid("one typed session mutation is required"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	var record runnersession.Record
	var snapshot runnersession.Snapshot
	if request.Start != nil {
		if request.SessionID != 0 || request.ExpectedRevision != 0 || request.Start.StartedAt.IsZero() || request.Start.IssueID == "" || len(request.ProcessKey) < 32 {
			return d.HTTPError(c, hubapi.NativePublicInvalid("new session requires an issue, start time and process key"))
		}
		record = runnersession.Record{Organization: scope.Organization, Project: scope.Project, IssueID: request.Start.IssueID, StartKey: request.Key, StartedAt: request.Start.StartedAt, Revision: 1}
		snapshot.Start, snapshot.Identity = *request.Start, request.Start.RuntimeIdentity
		snapshot.Resume = store.SessionResumeState{ResumedFromSessionID: request.Start.ResumedFromSessionID, OrphanRecoveryOutcome: request.Start.OrphanRecoveryOutcome, OrphanRecoveryFallbackReason: request.Start.OrphanRecoveryFallbackReason}
		snapshot.Provider = store.SessionProviderIdentity{ThreadID: request.Start.ProviderThreadID, SessionID: request.Start.ProviderSessionID}
		snapshot.Authority = RunnerSessionProcessAuthority(request.ProcessKey)

	}
	command := eventstore.RunCommand{Project: scope.Project, WorkItem: tracker.NativeWorkItemID(record.IssueID), LeaseID: string(request.LeaseID), SessionID: request.SessionID}
	var attempt leasestore.Attempt
	var approved policystore.Revision
	err := d.ReadCommand(ctx, scope, command, func(ctx context.Context, r tenantstore.Reader) error {
		if request.Start == nil {
			if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
				return err
			}
			reader, ok := r.(runnersession.Reader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			var err error
			record, err = reader.RunnerSession(ctx, scope.Project, request.SessionID)
			if err != nil {
				return err
			}
			if request.ExpectedRevision != record.Revision {
				return corestore.ErrRevisionConflict
			}
		}
		var err error
		attempt, approved, err = d.RunnerSessionFence(ctx, r, scope, request, record.IssueID, false)
		return err
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	command.WorkItem = tracker.NativeWorkItemID(record.IssueID)
	if request.Start == nil {
		snapshot, err = d.ReadRunnerSessionPayload(ctx, record)
		if err != nil {
			return d.HTTPError(c, err)
		}
	}

	if request.Start != nil {
		record.AttemptID, record.PolicyID = attempt.ID, approved.PolicyID
		snapshot.Policy, err = eventstore.NewContent(d.Details()).ProjectPolicy(ctx, approved)
		if err != nil {
			return d.HTTPError(c, err)
		}
	} else if record.AttemptID != attempt.ID {
		return d.HTTPError(c, tracker.ErrStaleFencingToken)
	}
	duplicate, err := runnersession.ApplyFinalization(&snapshot, request)
	if err != nil {
		return d.HTTPError(c, hubapi.NativePublicInvalid(err.Error()))
	}
	if duplicate {
		err := d.WriteCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
			locked, policy, err := d.RunnerSessionFence(ctx, w, scope, request, record.IssueID, true)
			if err != nil {
				return err
			}
			if locked.ID != record.AttemptID || policy.PolicyID != record.PolicyID {
				return tracker.ErrStaleFencingToken
			}
			reader, ok := w.(runnersession.Reader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			current, err := reader.RunnerSession(ctx, scope.Project, record.ID)
			if err != nil {
				return err
			}
			if current.Revision != record.Revision || current.AttemptID != record.AttemptID {
				return corestore.ErrRevisionConflict
			}
			return nil
		})
		if err != nil {
			return d.HTTPError(c, err)
		}
		return c.JSON(http.StatusOK, snapshot)
	}
	if request.Start == nil {
		record.Revision++
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
	if (request.Worker != nil || request.Reaped != nil) && (snapshot.Authority == "" || subtle.ConstantTimeCompare([]byte(snapshot.Authority), []byte(RunnerSessionProcessAuthority(request.ProcessKey))) != 1) {
		return d.HTTPError(c, hubscope.RunnerUnauthorized())
	}
	request.ProcessKey = ""
	if request.Worker != nil {
		reserved := request.Worker.PID == 0 && request.Worker.GroupID == 0 && request.Worker.StartedAt.IsZero() && request.Worker.CleanupRoot != "" && request.Worker.CleanupPath != ""
		started := request.Worker.PID > 0 && request.Worker.GroupID > 0 && !request.Worker.StartedAt.IsZero()
		if !reserved && !started {
			return d.HTTPError(c, hubapi.NativePublicInvalid("worker process requires PID, group and start identity"))
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
			return d.HTTPError(c, hubapi.NativePublicInvalid("process reap requires a registered process and timestamp"))
		}
		snapshot.Reaped = request.Reaped
		record.WorkerReapedAt = &request.Reaped.ReapedAt
	}
	if request.Progress != nil {
		snapshot.Progress = request.Progress
	}
	if request.Finish != nil {
		if request.Finish.CompletedAt.IsZero() {
			return d.HTTPError(c, hubapi.NativePublicInvalid("session finish requires completion time"))
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
		return d.HTTPError(c, err)
	}
	record.Reference, err = d.Details().StoreDetail(ctx, string(scope.Organization), raw)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if err := d.ValidateDetailReference(ctx, d.Details(), scope.Organization, record.Reference, raw); err != nil {
		return d.HTTPError(c, err)
	}
	historyRaw, err := json.Marshal(request)
	if err != nil {
		return d.HTTPError(c, err)
	}
	historyRef, err := d.Details().StoreDetail(ctx, string(scope.Organization), historyRaw)
	if err != nil {
		return d.HTTPError(c, err)
	}
	if err := d.ValidateDetailReference(ctx, d.Details(), scope.Organization, historyRef, historyRaw); err != nil {
		return d.HTTPError(c, err)
	}
	err = d.WriteCommand(ctx, scope, command, func(ctx context.Context, w tenantstore.Writer) error {
		locked, policy, err := d.RunnerSessionFence(ctx, w, scope, request, record.IssueID, true)
		if err != nil {
			return err
		}
		if locked.ID != record.AttemptID || policy.PolicyID != record.PolicyID {
			return tracker.ErrStaleFencingToken
		}
		writer, ok := w.(runnersession.Writer)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		if request.Start != nil {
			record, err = writer.CreateRunnerSession(ctx, record, runnersession.History{Key: request.Key, Reference: historyRef})
			return err
		}
		record.ReceiptID, err = writer.UpdateRunnerSession(ctx, record, runnersession.History{SessionID: record.ID, Revision: record.Revision, Key: request.Key, Reference: historyRef})
		if err == nil && request.Worker != nil {
			return d.RegisterWorkerProcess(ctx, w, scope, record, snapshot, historyRef, leasestore.Fence{LeaseID: locked.LeaseID, FencingToken: locked.FencingToken})
		}
		return err
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	snapshot.ID, snapshot.Revision = record.ID, record.Revision
	snapshot.ReceiptID = record.ReceiptID
	return c.JSON(http.StatusOK, snapshot)
}

func (d SessionDeps) GetRunnerSessionHistory(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	id, err := strconv.ParseInt(c.Param("session"), 10, 64)
	if err != nil || id < 1 {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	revision, err := strconv.ParseInt(c.Param("revision"), 10, 64)
	if err != nil || revision < 1 {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	scope := hubscope.NativeRequestScope(c)
	if _, err := d.runnerSessionRead(ctx, scope, id, nil); err != nil {
		return d.HTTPError(c, err)
	}
	var entries []runnersession.History
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		entries, err = reader.RunnerSessionHistory(ctx, scope.Project, id, revision-1, 1)
		return err
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	if len(entries) != 1 || entries[0].Revision != revision {
		return d.HTTPError(c, hubapi.NativeNotFound())
	}
	raw, err := d.Details().ReadDetail(ctx, string(scope.Organization), entries[0].Reference)
	if err != nil {
		return d.HTTPError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (d SessionDeps) GetRunnerSessionProcesses(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope := hubscope.NativeRequestScope(c)
	if scope.Credential.Runner.RunnerID == "" {
		return d.HTTPError(c, hubscope.RunnerUnauthorized())
	}
	var records []runnersession.Record
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		records, err = reader.RunnerSessionProcesses(ctx, scope.Project, string(scope.Credential.Runner.MachineID), c.QueryParam("attempt"), 200)
		return err
	})
	if err != nil {
		return d.HTTPError(c, err)
	}
	result := runnersession.Processes{More: len(records) == 200}
	for _, record := range records {
		value, err := d.ReadRunnerSessionPayload(ctx, record)
		if err != nil {
			return d.HTTPError(c, err)
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

func (d SessionDeps) VerifyRunnerSessionFinalization(ctx context.Context, scope hubscope.NativeScope, record runnersession.Record, snapshot runnersession.Snapshot) error {
	if state := snapshot.Finalization; state != nil {
		if state.Input.Phase == nil || !state.Finished || !state.UsageRecorded || !state.PhaseRecorded {
			return runnersession.ErrFinalizationIncomplete
		}
		return nil
	}
	if record.Revision < 4 {
		return runnersession.ErrFinalizationIncomplete
	}
	var history []runnersession.History
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		reader, ok := r.(runnersession.Reader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		current, err := reader.RunnerSession(ctx, scope.Project, record.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, record) {
			return corestore.ErrRevisionConflict
		}
		history, err = reader.RunnerSessionHistory(ctx, scope.Project, record.ID, max(int64(0), record.Revision-runnersession.HistoryLimit), runnersession.HistoryLimit)
		return err
	})
	if err != nil {
		return err
	}
	if len(history) < 3 {
		return runnersession.ErrFinalizationIncomplete
	}
	entries := make([]runnersession.Mutation, 0, 3)
	for i := len(history) - 1; i >= 0; i-- {
		row := history[i]
		if row.SessionID != record.ID || row.Revision != record.Revision-int64(len(history)-1-i) {
			return runnersession.ErrFinalizationIncomplete
		}
		raw, err := d.Details().ReadDetail(ctx, string(scope.Organization), row.Reference)
		if err != nil {
			return fmt.Errorf("read session finalization history: %w", err)
		}
		var entry runnersession.Mutation
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("decode session finalization history: %w", err)
		}
		if entry.Key != row.Key {
			return runnersession.ErrFinalizationIncomplete
		}
		entries = append(entries, entry)
		if len(entries) >= 3 && runnersession.VerifyLegacyFinalization(snapshot, entries) == nil {
			return nil
		}
	}
	return runnersession.VerifyLegacyFinalization(snapshot, entries)
}
