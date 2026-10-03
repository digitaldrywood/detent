package operatortool

import (
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func NativeRuntimeResult(name string, request WorkReadRequest, evidence tracker.NativeRuntimeEvidence) (Result, error) {
	if string(evidence.Issue.WorkItemID) != request.Reference {
		return Result{}, ErrAccessDenied
	}
	if name == GitHubScopeTimings {
		data, ok := evidence.GitHubTimings(request.NativeAttemptID, request.RunnerID)
		if !ok {
			return Result{}, explain.ErrNotFound
		}
		return nativeRuntimeEnvelope(request, evidence, data)
	}
	if name == WorkAttemptReceipt && (evidence.Attempt == nil || request.NativeAttemptID != "" && evidence.Attempt.AttemptID != request.NativeAttemptID || request.AttemptID != 0 && (evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.LocalAttemptID != request.AttemptID)) {
		return Result{}, explain.ErrNotFound
	}
	if name == BoardSessionHistory {
		if evidence.Attempt == nil || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.Activity == nil {
			return Result{}, ErrReadUnavailable
		}
		profile := *evidence.Attempt.Runtime.Activity
		page := OffsetPage(profile.Spans, request.Offset, request.Limit)
		profile.Spans = nil
		return nativeRuntimeEnvelope(request, evidence, struct {
			AttemptID string                                 `json:"attempt_id"`
			Freshness string                                 `json:"runtime_freshness"`
			Profile   workflowmetrics.ActivityProfile        `json:"profile"`
			Page      ReadPage[workflowmetrics.ActivitySpan] `json:"page"`
		}{evidence.Attempt.AttemptID, evidence.Attempt.RuntimeFreshness, profile, page})
	}
	if evidence.Attempt != nil && evidence.Attempt.Runtime != nil {
		attempt := *evidence.Attempt
		attempt.Runtime = attempt.Runtime.WithoutActivitySpans()
		evidence.Attempt = &attempt
	}
	evidence.Issue = evidence.Issue.RuntimeReference()
	return nativeRuntimeEnvelope(request, evidence, evidence)
}

func nativeRuntimeEnvelope[T any](request WorkReadRequest, evidence tracker.NativeRuntimeEvidence, data T) (Result, error) {
	return EncodeResult(WorkReadResult[T]{ProjectID: request.ProjectID, Reference: request.Reference, URL: WorkItemURL(request.ProjectID, request.Reference), GeneratedAt: evidence.ObservedAt, Freshness: explain.SourceAvailable, Data: data})
}
