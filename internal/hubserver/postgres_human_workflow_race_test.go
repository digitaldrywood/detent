package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type humanWorkflowRaceRepository struct {
	tenantstore.Repository
	attempt         string
	terminalStatus  string
	locked          chan struct{}
	resume          chan struct{}
	once            sync.Once
	retryableWrites atomic.Int64
}

func (r *humanWorkflowRaceRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, fn func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		err := fn(humanWorkflowRaceWriter{Writer: w, owner: r})
		if errors.Is(err, corestore.ErrSerializationFailure) {
			r.retryableWrites.Add(1)
		}
		return err
	})
}

type humanWorkflowRaceWriter struct {
	tenantstore.Writer
	owner *humanWorkflowRaceRepository
}

func (w humanWorkflowRaceWriter) LockUnreleasedIssueLease(ctx context.Context, issue int64) (leasestore.Lease, error) {
	return w.Writer.(leasestore.UnreleasedIssueLeaseLocker).LockUnreleasedIssueLease(ctx, issue)
}

func (w humanWorkflowRaceWriter) LockLease(ctx context.Context, lease string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, lease)
}

func (w humanWorkflowRaceWriter) RecordAttemptEvent(ctx context.Context, event leasestore.AttemptEvent) (leasestore.Attempt, bool, error) {
	attempt, duplicate, err := w.Writer.RecordAttemptEvent(ctx, event)
	if err == nil && event.ID == w.owner.attempt && event.Status == w.owner.terminalStatus {
		w.owner.once.Do(func() { close(w.owner.locked) })
		select {
		case <-w.owner.resume:
		case <-ctx.Done():
			return attempt, duplicate, ctx.Err()
		}
	}
	return attempt, duplicate, err
}

func verifyHumanWorkflowCompletionRace(t *testing.T, service *Service, admin *sql.DB, project tracker.NativeProject, claim tracker.NativeClaim, owner, worker, variant string) {
	t.Helper()
	item := seedConfiguredHumanWorkflowIssue(t, service, project.OrganizationID, project.ID, owner, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-race-create"}, Title: "Example cancellation race", Type: "bug", Body: workItemRecordBody, State: "Todo"})
	base := "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID)
	next := claim
	next.WorkItemID, next.SessionID = item.WorkItemID, "human-race-"+variant+"-session"
	next.ProviderCandidates = append([]tracker.NativeCapacityCandidate(nil), claim.ProviderCandidates...)
	next.ProviderCandidates[0].WorkItemID, next.ProviderCandidates[0].Revision = item.WorkItemID, item.Revision
	response := performHubAPIRequest(t, service, http.MethodPost, base+"/claims", worker, next)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	attemptID := newNativeID("attempt")
	started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "human-race-start-" + variant}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: attemptID, RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: claim.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example-backend", Model: "example-model"}}}
	eventPath := base + "/work-items/" + string(item.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventPath, worker, started), http.StatusOK)
	repository := service.config.TenantRepository
	terminalStatus := "succeeded"
	if variant == "release" {
		terminalStatus = "cancelled"
	}
	race := &humanWorkflowRaceRepository{Repository: repository, attempt: attemptID, terminalStatus: terminalStatus, locked: make(chan struct{}), resume: make(chan struct{})}
	service.config.TenantRepository = race
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(race.resume) }) }
	completionDone, cancellationDone := make(chan struct{}), make(chan struct{})
	completionResponse, cancellationResponse := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	cancellationStarted := false
	defer func() {
		release()
		<-completionDone
		if cancellationStarted {
			<-cancellationDone
		}
		service.config.TenantRepository = repository
	}()
	completed := started
	completed.IdempotencyKey, completed.Type, completed.Data.Sequence, completed.Data.Outcome = "human-race-complete", "run.finished", 2, "succeeded"
	go func() {
		defer close(completionDone)
		if variant == "release" {
			completionResponse <- performHubAPIRequest(t, service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "cancelled", CancelAttempt: true})
		} else {
			completionResponse <- performHubAPIRequest(t, service, http.MethodPost, eventPath, worker, completed)
		}
	}()
	select {
	case <-race.locked:
	case response := <-completionResponse:
		t.Fatalf("completion failed before fenced barrier: status=%d", response.Code)
	}
	var completionPID int
	if err := admin.QueryRowContext(t.Context(), `SELECT a.pid FROM pg_stat_activity a WHERE a.datname=current_database() AND a.state='idle in transaction' AND EXISTS(SELECT 1 FROM pg_locks l WHERE l.pid=a.pid AND l.relation='native_attempts'::regclass AND l.granted)`).Scan(&completionPID); err != nil {
		t.Fatalf("capture frozen completion session: %v", err)
	}
	cancellationStarted = true
	cancel := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-race-cancel", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: item.Revision, State: "Cancelled", Reason: "user_requested"}
	if variant == "release" {
		cancel.LeaseID, cancel.FencingToken = "", 0
	}
	go func() {
		defer close(cancellationDone)
		cancellationResponse <- performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item.WorkItemID)+"/workflow", owner, cancel)
	}()
	testenv.WaitUntil(t, testenv.WaitContext(t, t.Context()), func() error {
		select {
		case <-cancellationDone:
			response := <-cancellationResponse
			t.Fatalf("cancellation returned before the lock barrier: status=%d", response.Code)
		default:
		}
		var waiting int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid))`, completionPID).Scan(&waiting); err != nil {
			return err
		}
		if waiting == 0 {
			return fmt.Errorf("cancellation has not reached the completion lock")
		}
		return nil
	})
	release()
	workerStatus := http.StatusOK
	if variant == "release" {
		workerStatus = http.StatusNoContent
	}
	requireNativeStatus(t, <-completionResponse, workerStatus)
	requireNativeStatus(t, <-cancellationResponse, http.StatusOK)
	if race.retryableWrites.Load() != 0 {
		t.Fatalf("completion/cancellation required %d retryable write attempts", race.retryableWrites.Load())
	}
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), attemptID)
		if err != nil {
			return err
		}
		row, err := r.WorkItem(t.Context(), project.ID, item.WorkItemID)
		if err != nil {
			return err
		}
		if attempt.Status != terminalStatus || attempt.Sequence != 2 || stringValue(row.WorkflowStateName) != "Cancelled" || row.Revision != int64(item.Revision+1) {
			return fmt.Errorf("completion winner or later human intent was lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
