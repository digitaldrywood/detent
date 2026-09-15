package orchestrator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestStopRecordedRun(t *testing.T) {
	t.Parallel()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := procgroup.Inspect(&exec.Cmd{Process: process})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		change  func(*StopRunRequest, *recordedStopStore)
		wantErr error
		pending bool
	}{
		{name: "processless"},
		{name: "pending route", pending: true, change: func(r *StopRunRequest, _ *recordedStopStore) { r.Destination = "Backlog" }},
		{name: "missing identity", wantErr: ErrStopRunInvalidIdentity, change: func(r *StopRunRequest, _ *recordedStopStore) { r.WorkAttemptID = 0 }},
		{name: "wrong project", wantErr: ErrStopRunStale, change: func(r *StopRunRequest, _ *recordedStopStore) { r.ProjectID = "other" }},
		{name: "wrong issue", wantErr: ErrStopRunStale, change: func(r *StopRunRequest, _ *recordedStopStore) { r.IssueID = "other" }},
		{name: "wrong attempt", wantErr: ErrStopRunStale, change: func(r *StopRunRequest, _ *recordedStopStore) { r.Attempt++ }},
		{name: "wrong session", wantErr: ErrStopRunStale, change: func(r *StopRunRequest, _ *recordedStopStore) { r.DetentSessionID++ }},
		{name: "wrong provider", wantErr: ErrStopRunStale, change: func(r *StopRunRequest, _ *recordedStopStore) { r.ProviderSessionID = "other" }},
		{name: "invalid route", wantErr: ErrStopRunInvalidRoute, change: func(r *StopRunRequest, _ *recordedStopStore) { r.Destination = "invalid" }},
		{name: "invalid priority", wantErr: ErrStopRunInvalidRoute, change: func(r *StopRunRequest, _ *recordedStopStore) { r.Destination = "Todo" }},
		{name: "already complete", wantErr: ErrStopRunStale, change: func(_ *StopRunRequest, s *recordedStopStore) { s.attempt.CompletedAt = time.Now() }},
		{name: "missing row", wantErr: ErrStopRunStale, change: func(_ *StopRunRequest, s *recordedStopStore) { s.readErr = store.ErrNotFound }},
		{name: "read failure", wantErr: errRecordedStop, change: func(_ *StopRunRequest, s *recordedStopStore) { s.readErr = errRecordedStop }},
		{name: "process lookup failure", wantErr: errRecordedStop, change: func(_ *StopRunRequest, s *recordedStopStore) { s.processErr = errRecordedStop }},
		{name: "completion failure", wantErr: errRecordedStop, change: func(_ *StopRunRequest, s *recordedStopStore) { s.completeErr = errRecordedStop }},
		{name: "live worker", wantErr: ErrStopped, change: func(_ *StopRunRequest, s *recordedStopStore) {
			s.processes = []store.WorkerProcess{{SessionID: 91, WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}}}
		}},
		{name: "different live session", change: func(_ *StopRunRequest, s *recordedStopStore) {
			s.processes = []store.WorkerProcess{{SessionID: 92, WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}}}
		}},
		{name: "exited worker", change: func(_ *StopRunRequest, s *recordedStopStore) {
			s.processes = []store.WorkerProcess{{SessionID: 91, WorkerProcessIdentity: store.WorkerProcessIdentity{PID: 2147483647, StartedAt: time.Now()}}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := StopRunRequest{ProjectID: "project", IssueID: "issue", Attempt: 1, WorkAttemptID: 1, DetentSessionID: 91}
			backend := &recordedStopStore{attempt: store.WorkAttempt{ID: 1, ProjectID: "project", IssueID: "issue", AttemptNumber: 1, DetentSessionID: 91, WorkerMetadataJSON: `{"run_mode":"implement"}`, MetricsJSON: `{"total_tokens":100}`}}
			if tt.change != nil {
				tt.change(&request, backend)
			}
			result, err := StopRecordedRun(t.Context(), backend, backend, request)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				if backend.completeErr == nil && backend.completion.AttemptID != 0 {
					t.Fatal("rejected stop changed the attempt")
				}
				return
			}
			if backend.completion.TerminalState != store.WorkAttemptTerminalOperatorStopped || backend.completion.MetricsJSON != backend.attempt.MetricsJSON {
				t.Fatalf("completion = %+v", backend.completion)
			}
			if tt.pending {
				metadata, ok := operatorStopMetadataFromAttempt(store.WorkAttempt{WorkerMetadataJSON: backend.completion.WorkerMetadataJSON})
				if !ok || metadata.Destination != "Backlog" || result.Outcome != "pending" {
					t.Fatalf("pending stop = %+v, metadata = %+v", result, metadata)
				}
			} else if result.Outcome != "stopped" {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

var errRecordedStop = errors.New("recorded stop store failure")

type recordedStopStore struct {
	store.WorkAttemptStore
	attempt                          store.WorkAttempt
	processes                        []store.WorkerProcess
	readErr, processErr, completeErr error
	completion                       store.WorkAttemptCompletion
}

func (s *recordedStopStore) WorkAttempt(context.Context, int64) (store.WorkAttempt, error) {
	return s.attempt, s.readErr
}
func (s *recordedStopStore) ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error) {
	return s.processes, s.processErr
}
func (s *recordedStopStore) MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error {
	return nil
}
func (s *recordedStopStore) CompleteWorkAttempt(_ context.Context, c store.WorkAttemptCompletion) error {
	s.completion = c
	return s.completeErr
}
