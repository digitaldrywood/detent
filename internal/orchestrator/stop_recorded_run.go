package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

// StopRecordedRun stops a processless durable attempt when its project runtime
// is unavailable. Explicit lane moves use the existing pending-stop recovery;
// without a destination this operation only terminates the recorded attempt.
func StopRecordedRun(ctx context.Context, attempts store.WorkAttemptStore, processes WorkerProcessStore, request StopRunRequest) (StopRunResult, error) {
	if attempts == nil || processes == nil {
		return StopRunResult{}, ErrStopped
	}
	if strings.TrimSpace(request.ProjectID) == "" || strings.TrimSpace(request.IssueID) == "" || request.WorkAttemptID <= 0 || request.Attempt < 0 {
		return StopRunResult{}, ErrStopRunInvalidIdentity
	}
	request.Reason = strings.TrimSpace(request.Reason)
	request.Destination = strings.TrimSpace(request.Destination)
	if utf8.RuneCountInString(request.Reason) > StopRunReasonMaxLength {
		return StopRunResult{}, ErrStopRunInvalidRoute
	}
	if request.Destination != "" {
		destination, ok := canonicalStopRunDestination(request.Destination)
		if !ok || destination == StopRunDestinationTodo && (request.Priority < 1 || request.Priority > 4) {
			return StopRunResult{}, ErrStopRunInvalidRoute
		}
		request.Destination = destination
	}
	if request.Destination != StopRunDestinationTodo {
		request.Priority = 0
	}
	attempt, err := attempts.WorkAttempt(ctx, request.WorkAttemptID)
	if errors.Is(err, store.ErrNotFound) {
		return StopRunResult{}, ErrStopRunStale
	}
	if err != nil {
		return StopRunResult{}, err
	}
	if attempt.ProjectID != request.ProjectID || attempt.IssueID != request.IssueID || attempt.AttemptNumber != request.Attempt || attempt.DetentSessionID != request.DetentSessionID || request.ProviderSessionID != "" && attempt.ProviderSessionID != request.ProviderSessionID {
		return StopRunResult{}, ErrStopRunStale
	}
	if !attempt.CompletedAt.IsZero() {
		return StopRunResult{}, ErrStopRunStale
	}
	workers, err := processes.ListActiveWorkerProcesses(ctx)
	if err != nil {
		return StopRunResult{}, err
	}
	for _, worker := range workers {
		if worker.SessionID != attempt.DetentSessionID {
			continue
		}
		observed, err := procgroup.Observe([]procgroup.Identity{{PID: worker.PID, GroupID: worker.GroupID, StartedAt: worker.StartedAt}})
		if err != nil {
			return StopRunResult{}, fmt.Errorf("inspect recorded worker: %w", err)
		}
		if observed[0].Alive {
			return StopRunResult{}, ErrStopped
		}
	}
	now := time.Now().UTC()
	result := StopRunResult{
		ProjectID:         attempt.ProjectID,
		IssueID:           attempt.IssueID,
		Identifier:        attempt.Identifier,
		Attempt:           attempt.AttemptNumber,
		WorkAttemptID:     attempt.ID,
		DetentSessionID:   attempt.DetentSessionID,
		ProviderSessionID: attempt.ProviderSessionID,
		Destination:       request.Destination,
		Priority:          request.Priority,
		Reason:            request.Reason,
		Outcome:           "stopped",
		RequestedAt:       now,
		CompletedAt:       now,
	}
	phase, message, nextAction := "operator_stopped", "operator stopped recorded attempt; no live worker", ""
	metadata := attempt.WorkerMetadataJSON
	if request.Destination != "" {
		result.Outcome = "pending"
		phase, message, nextAction = "operator_stop_pending", "operator stop requested; waiting for tracker transition", "move work item to "+request.Destination
		document := map[string]any{}
		if err := json.Unmarshal([]byte(metadata), &document); err != nil {
			return StopRunResult{}, fmt.Errorf("read recorded stop metadata: %w", err)
		}
		if document == nil {
			document = map[string]any{}
		}
		document["operator_stop"] = operatorStopMetadataFromResult(result, "")
		encoded, err := json.Marshal(document)
		if err != nil {
			return StopRunResult{}, fmt.Errorf("encode recorded stop metadata: %w", err)
		}
		metadata = string(encoded)
	}
	err = attempts.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{
		AttemptID:              attempt.ID,
		CompletedAt:            now,
		TerminalState:          store.WorkAttemptTerminalOperatorStopped,
		SessionFinalState:      string(store.WorkAttemptTerminalOperatorStopped),
		ErrorClass:             string(store.WorkAttemptTerminalOperatorStopped),
		ErrorMessage:           "operator requested run stop",
		Phase:                  phase,
		StatusMessage:          message,
		WorkerMetadataJSON:     metadata,
		MetricsJSON:            attempt.MetricsJSON,
		GitHubRateSnapshotJSON: attempt.GitHubRateSnapshotJSON,
		CIState:                attempt.CIState,
		CapacitySnapshotJSON:   attempt.CapacitySnapshotJSON,
		NextAction:             nextAction,
		DetentSessionID:        attempt.DetentSessionID,
		ProviderSessionID:      attempt.ProviderSessionID,
		RuntimeIdentity:        attempt.RuntimeIdentity,
	})
	if err != nil {
		return StopRunResult{}, err
	}
	return result, nil
}
