package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) readNativeAnalyticsAttemptPopulationTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, runner string, usage []reportstore.UsageFact, usageClipped bool) (nativeAnalyticsAttemptPopulation, string, error) {
	out := nativeAnalyticsAttemptPopulation{Items: []nativeAnalyticsAttempt{}, Partial: usageClipped, Clipped: usageClipped}
	if len(usage) > maxAnalyticsPopulation+1 {
		return out, "", corestore.ErrInvalidPage
	}
	after, cursor, key, err := s.analyticsAttemptCursor(ctx, scope, w, runner, "attempts")
	if err != nil {
		return out, "", err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var page reportstore.AnalyticsAttemptPage
	var events []eventstore.Event
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(reportstore.AnalyticsAttemptReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsAttemptPage(ctx, reportstore.AnalyticsAttemptQuery{ProjectID: scope.project, Principal: principal, RunnerID: runner, RunnerIDs: analyticsCohortRunners(ctx), From: w.From, To: w.To, After: after, Limit: maxAnalyticsPopulation})
		if err != nil {
			return err
		}
		batch, ok := r.(eventstore.RunEventBatchReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page.Items = slices.DeleteFunc(page.Items, func(row reportstore.AnalyticsAttemptRow) bool { return !analyticsCohortAllows(ctx, row.WorkItemID) })
		ids := make([]string, len(page.Items))
		for i, row := range page.Items {
			ids[i] = row.AttemptID
		}
		selected, err := batch.LatestRunEvents(ctx, scope.project, ids)
		if err != nil {
			return err
		}
		byAttempt := make(map[string]eventstore.Event, len(selected))
		for _, event := range selected {
			if _, duplicate := byAttempt[event.AttemptID]; duplicate {
				return artifact.ErrIntegrity
			}
			byAttempt[event.AttemptID] = event
		}
		for _, row := range page.Items {
			event, ok := byAttempt[row.AttemptID]
			if !ok || event.OrganizationID != scope.organization || event.ProjectID != scope.project || event.WorkItemID != row.WorkItemID || event.AttemptID != row.AttemptID {
				return artifact.ErrIntegrity
			}
			events = append(events, event)
		}
		return nil
	})
	if err != nil {
		return out, "", err
	}
	out.Partial = out.Partial || page.Partial
	out.Clipped = out.Clipped || page.Partial
	counts := map[string]int{}
	observed := 0
	for _, row := range usage {
		if row.ProjectID != scope.project {
			return out, "", artifact.ErrIntegrity
		}
		if row.Period.Before(w.From) || !row.Period.Before(w.To) {
			continue
		}
		if observed == maxAnalyticsPopulation {
			out.Partial = true
			out.Clipped = true
			break
		}
		observed++
		counts[row.AttemptID]++
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for index, row := range page.Items {
		value, malformed, err := content.AnalyticsEvent(ctx, events[index])
		out.Malformed = out.Malformed || malformed
		out.Partial = out.Partial || malformed
		if err != nil {
			return out, "", fmt.Errorf("read analytics attempt: %w", err)
		}
		if value.Data.Run == nil || value.Data.Run.AttemptID != row.AttemptID || value.OrganizationID != scope.organization || value.ProjectID != scope.project || value.AggregateID != row.WorkItemID {
			return out, "", artifact.ErrIntegrity
		}
		run := value.Data.Run
		a := nativeAnalyticsAttempt{AttemptID: row.AttemptID, WorkItemID: string(row.WorkItemID), Status: row.Status, StartedAt: row.StartedAt, ObservedAt: row.UpdatedAt, UsageRowsObserved: counts[row.AttemptID]}
		a.ActivityRaw = "null"
		if run.Runtime != nil {
			runtime := run.Runtime
			a.Phases = runtime.Phases
			a.Identity = runtime.Identity
			a.Landing = runtime.Landing
			a.Pipeline = gate.PublicPipeline(runtime.Pipeline)
			a.PipelineDropped = runtime.PipelineDropped + len(runtime.Pipeline) - len(a.Pipeline)
			if runtime.Validation != nil {
				a.Pipeline = append(a.Pipeline, runtime.Validation.PipelineTiming("finalization"))
			}
			raw, err := json.Marshal(runtime.Activity)
			if err != nil {
				return out, "", err
			}
			a.ActivityRaw = string(raw)
			out.Partial = out.Partial || runtime.PhasesDropped > 0
		}
		if a.Identity.Role == "" && run.Identity != nil {
			a.Identity.Role = run.Identity.Role
		}
		out.Partial = out.Partial || len(a.Phases) == 0
		out.Items = append(out.Items, a)
	}
	next := ""
	if page.Next.ID != "" && len(key) == 0 {
		out.Partial = true
		out.Clipped = true
		out.ContinuationUnavailable = true
	} else if page.Next.ID != "" {
		next, err = s.encodeAnalyticsAttemptCursor(cursor, key, page.Next, "attempts")
		if err != nil {
			return out, "", err
		}
		if paging := analyticsPagingFromContext(ctx); paging != nil {
			paging.Next["attempts"] = next
		}
	}
	return out, next, nil
}

func (s *Service) readNativeAnalyticsUsagePopulationTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow) (reportstore.AnalyticsUsagePage, string, error) {
	after, cursor, key, err := s.analyticsUsageCursor(ctx, scope, w)
	if err != nil {
		return reportstore.AnalyticsUsagePage{}, "", err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return reportstore.AnalyticsUsagePage{}, "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var page reportstore.AnalyticsUsagePage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(reportstore.AnalyticsUsageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsUsagePage(ctx, reportstore.AnalyticsUsageQuery{ProjectID: scope.project, Principal: principal, From: w.From, To: w.To, After: after, Limit: maxAnalyticsPopulation})
		return err
	})
	if err != nil {
		return page, "", err
	}
	page.Items = slices.DeleteFunc(page.Items, func(row reportstore.UsageFact) bool {
		return !analyticsCohortAllows(ctx, row.WorkItemID) || !analyticsCohortRunnerAllows(ctx, row.RunnerID)
	})
	next := ""
	if !page.Next.Period.IsZero() && len(key) == 0 {
		page.Partial = true
		page.ContinuationUnavailable = true
	} else if !page.Next.Period.IsZero() {
		next, err = s.encodeAnalyticsUsageCursor(cursor, key, page.Next)
		if err != nil {
			return page, "", err
		}
		if paging := analyticsPagingFromContext(ctx); paging != nil {
			paging.Next["usage"] = next
		}
	}
	return page, next, nil
}
