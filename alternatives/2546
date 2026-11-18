package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectNativeAnalyticsResidenceTenant(ctx context.Context, scope hubscope.NativeScope, r operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	var live bool
	if value, ok := ctx.Value(analyticsLiveIssuesKey{}).(bool); ok {
		live = value
	}
	after, cursor, key, err := s.analyticsResidenceSeek(ctx, scope, out.Window, live)
	if err != nil {
		return err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	var page eventstore.AnalyticsResidencePage
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, reader, scope)
		if err != nil {
			return err
		}
		owner, ok := reader.(eventstore.AnalyticsResidenceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsResidencePage(ctx, eventstore.AnalyticsResidenceQuery{ProjectID: scope.Project, Principal: principal, From: out.Window.From, To: out.Window.To, Limit: maxAnalyticsPopulation, Live: live, IssuesAfter: after.IssuesAfter, EventsAfterItem: after.EventItem, EventsAfterSequence: after.EventSequence})
		return err
	})
	if err != nil {
		return err
	}
	page.Issues = slices.DeleteFunc(page.Issues, func(row eventstore.AnalyticsResidenceIssue) bool { return !analyticsCohortAllows(ctx, row.WorkItemID) })
	states := map[string]tracker.NativeState{}
	for _, state := range page.States {
		states[state.Name] = state
	}
	issues := make([]analyticsResidenceIssue, len(page.Issues))
	byID := map[tracker.NativeWorkItemID]int{}
	for i, issue := range page.Issues {
		issues[i] = analyticsResidenceIssue{Id: string(issue.WorkItemID), Created: issue.CreatedAt, Initial: issue.InitialState, Partial: after.EventItem != ""}
		byID[issue.WorkItemID] = i
		if after.EventItem != "" {
			issues[i].Initial = ""
		}
		if page.EventsMore && issue.WorkItemID >= page.EventsAfterItem {
			issues[i].Partial = true
		}
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, row := range page.Events {
		if row.OrganizationID != scope.Organization || row.ProjectID != scope.Project {
			return artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		i, ok := byID[row.WorkItemID]
		if !ok {
			return artifact.ErrIntegrity
		}
		value, err := content.Event(ctx, row)
		if err != nil {
			return err
		}
		if value.Type == "scheduler.decision" {
			if value.Data.Decision == nil {
				return artifact.ErrIntegrity
			}
			if value.Data.Decision.Source != "native_claim" || value.Data.Decision.Outcome != "claimed" {
				continue
			}
		}
		issues[i].Events = append(issues[i].Events, analyticsResidenceEvent{At: value.RecordedAt, Kind: value.Type, Data: value.Data})
	}
	paging := analyticsPagingFromContext(ctx)
	if paging == nil {
		return errors.New("analytics residence paging context unavailable")
	}
	if page.IssuesMore && len(key) > 0 {
		token, err := s.encodeAnalyticsResidenceCursor(cursor, key, analyticsResidenceCursor{IssuesAfter: page.IssuesAfter}, "residence_issues")
		if err != nil {
			return err
		}
		paging.Next["residence_issues"] = token
	}
	if page.EventsMore && len(key) > 0 {
		token, err := s.encodeAnalyticsResidenceCursor(cursor, key, analyticsResidenceCursor{IssuesAfter: after.IssuesAfter, EventItem: page.EventsAfterItem, EventSequence: page.EventsAfterSequence}, "residence_events")
		if err != nil {
			return err
		}
		paging.Next["residence_events"] = token
	}
	clipped := page.IssuesMore || page.EventsMore || after.IssuesAfter != "" || after.EventItem != ""
	if (page.IssuesMore || page.EventsMore) && len(key) == 0 {
		out.Unavailable = append(out.Unavailable, "residence_continuation_requires_secret_keys")
	}
	return projectAnalyticsResidencePopulation(states, issues, clipped, r, out)
}
