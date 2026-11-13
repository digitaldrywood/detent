package hubserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readNativeFailuresTenant(ctx context.Context, scope nativeScope, item tracker.NativeWorkItemID, w operatortool.AnalyticsWindow) ([]nativeFailure, bool, []string, error) {
	out := []nativeFailure{}
	unavailable := []string{}
	after, cursor, key, err := s.analyticsFactSeek(ctx, scope, w, "failures")
	if err != nil {
		return out, false, nil, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, false, nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	var page diagnosticstore.AnalyticsFailurePage
	var usage reportstore.AnalyticsUsagePage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(diagnosticstore.AnalyticsFailureReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		usageOwner, ok := r.(reportstore.AnalyticsUsageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsFailurePage(ctx, diagnosticstore.AnalyticsFailureQuery{ProjectID: scope.project, WorkItemID: item, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after.Failure})
		if err != nil {
			return err
		}
		page.Attempts = slices.DeleteFunc(page.Attempts, func(row diagnosticstore.Attempt) bool {
			return !analyticsCohortAllows(ctx, row.WorkItemID) || !analyticsCohortRunnerAllows(ctx, row.RunnerID)
		})
		if len(page.Attempts) == 0 {
			return nil
		}
		usage, err = usageOwner.ReadAnalyticsUsagePage(ctx, reportstore.AnalyticsUsageQuery{ProjectID: scope.project, Principal: principal, From: time.Time{}, To: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), Limit: maxAnalyticsPopulation})
		return err
	})
	if err != nil {
		return out, false, nil, err
	}
	partial := page.Partial || usage.Partial
	if usage.Partial {
		unavailable = append(unavailable, "failure_cost_complete_population")
	}
	if page.ProjectionMissing {
		unavailable = append(unavailable, "recorded_failure_projection_unavailable")
	}
	totals := map[string]diagnosticstore.Usage{}
	for _, row := range usage.Items {
		sum := totals[row.AttemptID]
		sum.Cost += row.Cost
		sum.Tokens += row.Input + row.Output
		totals[row.AttemptID] = sum
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, attempt := range page.Attempts {
		if attempt.Event.ID == "" {
			partial = true
			continue
		}
		value, _, err := content.AnalyticsEvent(ctx, attempt.Event)
		if errors.Is(err, artifact.ErrMissing) {
			partial = true
			unavailable = append(unavailable, "recorded_failure_detail_unavailable")
			continue
		}
		if err != nil {
			return out, false, nil, err
		}
		if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.ID || value.ProjectID != scope.project || value.AggregateID != attempt.WorkItemID {
			return out, false, nil, artifact.ErrIntegrity
		}
		example, class := failureFromAttempt(*value.Data.Run, attempt.Status)
		if example == "" {
			partial = true
			unavailable = append(unavailable, "recorded_failure_reason_unavailable")
			continue
		}
		at := attempt.Event.RecordedAt
		if runtime := value.Data.Run.Runtime; runtime != nil && runtime.Landing != nil && ((runtime.Landing.RefusalKind == "conflict" || runtime.Landing.RefusalKind == "base_protected") && !runtime.Landing.Landed || runtime.Landing.GateFailed) && !runtime.Landing.ObservedAt.IsZero() {
			at = runtime.Landing.ObservedAt
		}
		if at.Before(w.From) || !at.Before(w.To) {
			continue
		}
		cost := totals[attempt.ID]
		out = append(out, nativeFailure{Signature: strings.Clone(normalizeFailureSignature(example)), AttemptID: attempt.ID, WorkItem: string(attempt.WorkItemID), At: at, Started: attempt.StartedAt, Finished: attempt.UpdatedAt, Example: strings.Clone(boundedFailureText(failureFirstLine(example), maxFailureExampleLength)), Class: class, Cost: cost.Cost, Tokens: cost.Tokens})
	}
	if page.Next.ID != "" {
		if len(key) == 0 {
			partial = true
			unavailable = append(unavailable, "failures_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(cursor, key, analyticsFactCursor{Failure: page.Next}, "failures")
			if err != nil {
				return out, false, nil, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.Next["failures"] = token
			}
		}
	}
	return out, partial, slices.Compact(unavailable), nil
}
