package hubserver

import (
	"context"
	"sort"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readAnalyticsSchedulerPopulationTenant(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, name string) (nativeAnalyticsSchedulerPopulation, error) {
	out := nativeAnalyticsSchedulerPopulation{Events: []tracker.CollaborationEvent{}, Prior: map[string]tracker.CollaborationEvent{}}
	after, cursor, key, err := s.analyticsSchedulerCursor(ctx, scope, w, name)
	if err != nil {
		return out, err
	}
	defer clear(key)
	outcome := "skipped"
	if name == "capacity" {
		outcome = "claimed"
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, hubapi.NativeWorkItemStorageUnavailable()
	}
	var page eventstore.AnalyticsSchedulerPage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(eventstore.AnalyticsSchedulerReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsSchedulerPage(ctx, eventstore.AnalyticsSchedulerQuery{ProjectID: scope.project, Principal: principal, From: w.From, To: w.To, Outcome: outcome, After: after, Limit: maxAnalyticsPopulation})
		return err
	})
	if err != nil {
		return out, err
	}
	out.Partial = page.Partial
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, row := range page.Events {
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project || row.Type != "scheduler.decision" || row.SchedulerOutcome != outcome {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		event, err := content.Event(ctx, row)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, event)
		if prior, ok := page.Prior[row.ID]; ok && prior.Type == "scheduler.decision" {
			if prior.OrganizationID != scope.organization || prior.ProjectID != scope.project || prior.WorkItemID != row.WorkItemID || prior.Sequence >= row.Sequence {
				return out, artifact.ErrIntegrity
			}
			value, err := content.Event(ctx, prior)
			if err != nil {
				return out, err
			}
			out.Prior[row.ID] = value
		}
	}
	if page.Next.ID != "" {
		if len(key) == 0 {
			out.Partial = true
			out.ContinuationUnavailable = true
		} else {
			token, err := s.encodeAnalyticsSchedulerCursor(cursor, key, page.Next, name)
			if err != nil {
				return out, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.Next[name] = token
			}
		}
	}
	return out, nil
}

func (s *Service) projectNativeAnalyticsSkipsTenant(ctx context.Context, scope nativeScope, out *nativeAnalyticsProject) error {
	population, err := s.readAnalyticsSchedulerPopulationTenant(ctx, scope, out.Window, "decisions")
	if err != nil {
		return err
	}
	out.Partial = out.Partial || population.Partial
	if population.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "decisions_continuation_requires_secret_keys")
	}
	skips := map[string]nativeAnalyticsSkip{}
	for _, event := range population.Events {
		decision := event.Data.Decision
		if decision == nil {
			return artifact.ErrIntegrity
		}
		key := decision.Source + "\x00" + decision.Reason
		value := skips[key]
		value.Source = decision.Source
		value.Reason = decision.Reason
		value.Count++
		skips[key] = value
		out.DecisionsObserved++
		if event.RecordedAt.After(out.SourceAt) {
			out.SourceAt = event.RecordedAt
		}
	}
	for _, value := range skips {
		out.SkipReasons = append(out.SkipReasons, value)
	}
	sort.Slice(out.SkipReasons, func(i, j int) bool {
		a, b := out.SkipReasons[i], out.SkipReasons[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.Reason < b.Reason
	})
	return nil
}

func (s *Service) projectNativeAnalyticsCapacityWaitTenant(ctx context.Context, scope nativeScope, out *nativeAnalyticsProject) error {
	population, err := s.readAnalyticsSchedulerPopulationTenant(ctx, scope, out.Window, "capacity")
	if err != nil {
		return err
	}
	out.Partial = out.Partial || population.Partial
	if population.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "capacity_continuation_requires_secret_keys")
	}
	queue := nativeAnalyticsCapacityWait{Source: "native_scheduler_decisions", Coverage: "recorded_provider_capacity_wait_to_claim; claimed_in_window; clipped_to_window", Partial: true}
	for _, event := range population.Events {
		claim := event.Data.Decision
		if claim == nil {
			return artifact.ErrIntegrity
		}
		if claim.Source != "native_claim" {
			continue
		}
		queue.ClaimsObserved++
		finish := event.RecordedAt
		if finish.After(queue.SourceAt) {
			queue.SourceAt = finish
		}
		prior, ok := population.Prior[event.ID]
		start := prior.RecordedAt
		if !ok || !nativeAnalyticsCapacityInterval(prior.Data.Decision, claim, start, finish) {
			queue.IntervalsUnavailable++
			continue
		}
		start = hubapi.MaxTime(start, out.Window.From)
		queue.IntervalsObserved++
		queue.Seconds += finish.Sub(start).Seconds()
		for i := range out.Digest {
			bucket := &out.Digest[i]
			from, to := hubapi.MaxTime(start, bucket.From), hubapi.MinTime(finish, bucket.To)
			if to.After(from) {
				bucket.ProviderCapacityWaitSeconds += to.Sub(from).Seconds()
			}
		}
	}
	out.ProviderCapacityWait = queue
	if queue.SourceAt.After(out.SourceAt) {
		out.SourceAt = queue.SourceAt
	}
	if queue.IntervalsObserved == 0 {
		out.Unavailable = append(out.Unavailable, "provider_capacity_wait")
	}
	return nil
}
