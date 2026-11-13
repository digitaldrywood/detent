package hubserver

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsResidenceIssue struct {
	id      string
	created time.Time
	initial string
	events  []analyticsResidenceEvent
	partial bool
}

type analyticsResidenceEvent struct {
	at   time.Time
	kind string
	data tracker.CollaborationData
}

type analyticsResidenceSpan struct {
	lane    string
	from    time.Time
	to      time.Time
	queueTo time.Time
}

type analyticsResidenceTimeline struct {
	issue   analyticsResidenceIssue
	started *time.Time
	spans   []analyticsResidenceSpan
	state   string
	entered time.Time
	done    []time.Time
	rework  []analyticsResidenceEvent
}

func projectAnalyticsResidencePopulation(states map[string]tracker.NativeState, issues []analyticsResidenceIssue, clipped bool, r operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	residence := operatortool.AnalyticsResidence{
		Source:         "native_workflow_transitions_and_issue_creation",
		Coverage:       "per_issue; residence_clipped_to_window; system_excludes_intake_terminal_and_held; held_after_first_dispatch; lead_created_to_done_at_completion; aging_at_window_end; quantiles_linear_interpolation",
		IssuesObserved: len(issues),
	}
	timelines := make([]analyticsResidenceTimeline, 0, len(issues))
	available := false
	for _, issue := range issues {
		available = available || issue.initial != ""
		residence.EventsObserved += len(issue.events)
		for _, event := range issue.events {
			if event.at.After(out.SourceAt) {
				out.SourceAt = event.at
			}
		}
		if issue.created.After(out.SourceAt) {
			out.SourceAt = issue.created
		}
		timeline := buildAnalyticsResidenceTimeline(issue, states, out.Window.To)
		clipped = clipped || timeline.issue.partial
		timelines = append(timelines, timeline)
		out.pipelineTimings = append(out.pipelineTimings, residencePipelineTimings(timeline, out.allAttempts)...)
	}
	var items []operatortool.AnalyticsIssueResidence
	residence.AnalyticsResidenceSummary, items = summarizeAnalyticsResidence(timelines, states, out.Window.From, out.Window.To)
	residence.Partial = clipped
	residence.Issues = operatortool.OffsetPage(items, r.RowOffset, r.Limit)
	aging := []operatortool.AnalyticsAging{}
	for _, timeline := range timelines {
		if timeline.issue.partial || states[timeline.state].Terminal || !timeline.entered.Before(out.Window.To) {
			continue
		}
		item := operatortool.AnalyticsAging{WorkItemID: timeline.issue.id, Lane: timeline.state, EnteredAt: timeline.entered, Hours: out.Window.To.Sub(timeline.entered).Hours()}
		if timeline.started != nil {
			since := out.Window.To.Sub(*timeline.started).Hours()
			item.SinceTodoHours = &since
		}
		for _, lane := range residence.Lanes {
			if lane.Lane == item.Lane {
				hours := lane.P90Seconds / 3600
				item.LaneP90Hours = &hours
				break
			}
		}
		aging = append(aging, item)
	}
	residence.Aging = operatortool.OffsetPage(aging, r.RowOffset, r.Limit)
	out.LaneResidence = residence
	out.Partial = out.Partial || clipped
	for i := range out.Digest {
		bucket := &out.Digest[i]
		bucket.LaneResidence, _ = summarizeAnalyticsResidence(timelines, states, bucket.From, bucket.To)
		bucket.LaneResidence.Partial = clipped
		bucket.QueueSeconds = bucket.LaneResidence.QueueTotal.Seconds
	}
	out.QueueTime.Source = residence.Source
	out.QueueTime.Coverage = "todo_residence_and_rework_before_claim; includes_unclaimed_waits; clipped_to_window"
	out.QueueTime.Seconds = residence.QueueTotal.Seconds
	out.QueueTime.Duration = residence.QueueTotal
	out.QueueTime.Partial = clipped
	if !available {
		out.QueueTime.Partial = true
		out.Unavailable = append(out.Unavailable, "lane_residence_and_unclaimed_waits_unavailable", "queue_time")
	} else {
		if clipped {
			out.Unavailable = append(out.Unavailable, "lane_residence_complete_population", "queue_time_complete_population")
		}
	}
	return nil
}

type analyticsLiveIssuesKey struct{}

// withAnalyticsLiveIssues leaves archived nonterminal issues out of residence
// history so they neither appear as current work nor consume the population cap.
func withAnalyticsLiveIssues(ctx context.Context) context.Context {
	return context.WithValue(ctx, analyticsLiveIssuesKey{}, true)
}

type analyticsResidenceFold struct {
	issue   analyticsResidenceIssue
	states  map[string]tracker.NativeState
	to      time.Time
	queueTo time.Time
	last    time.Time
	started *time.Time
	state   string
	entered time.Time
}

type analyticsResidenceDelta struct {
	span    analyticsResidenceSpan
	hasSpan bool
	done    bool
	rework  bool
}

func newAnalyticsResidenceFold(issue analyticsResidenceIssue, states map[string]tracker.NativeState, to time.Time) analyticsResidenceFold {
	fold := analyticsResidenceFold{issue: issue, states: states, to: to, queueTo: to, last: issue.created, state: issue.initial, entered: issue.created}
	if issue.initial == "" {
		fold.issue.partial = true
	}
	if states[issue.initial].Dispatchable && analyticsResidenceGroup(issue.initial, states) == "system" {
		at := issue.created
		fold.started = &at
	}
	return fold
}

func (fold *analyticsResidenceFold) append(event analyticsResidenceEvent) analyticsResidenceDelta {
	delta := analyticsResidenceDelta{}
	if event.at.Before(fold.last) {
		fold.issue.partial = true
		return delta
	}
	fold.last = event.at
	if event.kind == "scheduler.decision" {
		if fold.state == "Rework" && fold.queueTo.After(event.at) {
			fold.queueTo = event.at
		}
		return delta
	}
	if event.data.FromState != fold.state || event.data.ToState == "" {
		fold.issue.partial = true
	} else if event.data.ToState != fold.state && fold.started != nil {
		delta.span = analyticsResidenceSpan{lane: fold.state, from: maxTime(fold.entered, *fold.started), to: event.at, queueTo: minTime(fold.queueTo, event.at)}
		delta.hasSpan = true
	}
	if event.data.ToState == "" || event.data.ToState == fold.state {
		return delta
	}
	fold.state, fold.entered = event.data.ToState, event.at
	fold.queueTo = fold.to
	if fold.started == nil && fold.states[fold.state].Dispatchable && analyticsResidenceGroup(fold.state, fold.states) == "system" {
		at := event.at
		fold.started = &at
	}
	delta.done = fold.state == "Done"
	delta.rework = fold.state == "Rework"
	return delta
}

func (fold *analyticsResidenceFold) finish() (analyticsResidenceSpan, bool) {
	if fold.started == nil || fold.issue.partial {
		return analyticsResidenceSpan{}, false
	}
	return analyticsResidenceSpan{lane: fold.state, from: maxTime(fold.entered, *fold.started), to: fold.to, queueTo: fold.queueTo}, true
}

func buildAnalyticsResidenceTimeline(issue analyticsResidenceIssue, states map[string]tracker.NativeState, to time.Time) analyticsResidenceTimeline {
	fold := newAnalyticsResidenceFold(issue, states, to)
	timeline := analyticsResidenceTimeline{}
	for _, event := range issue.events {
		delta := fold.append(event)
		if delta.hasSpan {
			timeline.spans = append(timeline.spans, delta.span)
		}
		if delta.done {
			timeline.done = append(timeline.done, event.at)
		}
		if delta.rework {
			timeline.rework = append(timeline.rework, event)
		}
	}
	if span, ok := fold.finish(); ok {
		timeline.spans = append(timeline.spans, span)
	}
	timeline.issue, timeline.started, timeline.state, timeline.entered = fold.issue, fold.started, fold.state, fold.entered
	return timeline
}

func analyticsResidenceGroup(lane string, states map[string]tracker.NativeState) string {
	if lane == "Backlog" || lane == "Triage" || lane == "Done" || lane == "Cancelled" || states[lane].Terminal {
		return "excluded"
	}
	if lane == "Blocked" || lane == "Human Review" {
		return "held"
	}
	return "system"
}

func summarizeAnalyticsResidence(timelines []analyticsResidenceTimeline, states map[string]tracker.NativeState, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	var system, held, lead, queue []float64
	causes := map[[2]string]operatortool.AnalyticsReworkCause{}
	for _, timeline := range timelines {
		item := operatortool.AnalyticsIssueResidence{WorkItemID: timeline.issue.id, SystemStartedAt: timeline.started, LaneSeconds: map[string]float64{}, Partial: timeline.issue.partial}
		for _, span := range timeline.spans {
			group := analyticsResidenceGroup(span.lane, states)
			seconds := minTime(span.to, to).Sub(maxTime(span.from, from)).Seconds()
			if seconds <= 0 || group == "excluded" {
				continue
			}
			item.LaneSeconds[span.lane] += seconds
			if group == "held" {
				item.HeldSeconds += seconds
			} else {
				item.SystemSeconds += seconds
			}
			if span.lane == "Todo" || span.lane == "Rework" {
				end := span.to
				if span.lane == "Rework" {
					end = span.queueTo
				}
				item.QueueSeconds += max(0, minTime(end, to).Sub(maxTime(span.from, from)).Seconds())
			}
		}
		for _, done := range timeline.done {
			if !done.Before(from) && done.Before(to) {
				seconds := done.Sub(timeline.issue.created).Seconds()
				item.LeadSeconds = &seconds
			}
		}
		for lane, seconds := range item.LaneSeconds {
			lanes[lane] = append(lanes[lane], seconds)
		}
		if item.SystemSeconds > 0 {
			system = append(system, item.SystemSeconds)
		}
		if item.HeldSeconds > 0 {
			held = append(held, item.HeldSeconds)
		}
		if item.QueueSeconds > 0 {
			queue = append(queue, item.QueueSeconds)
		}
		if item.LeadSeconds != nil {
			lead = append(lead, *item.LeadSeconds)
		}
		for _, event := range timeline.rework {
			if event.at.Before(from) || !event.at.Before(to) {
				continue
			}
			key := [2]string{event.data.FromState, event.data.ReasonDetail}
			cause := causes[key]
			cause.FromState, cause.ReasonDetail = event.data.FromState, event.data.ReasonDetail
			cause.Count++
			causes[key] = cause
		}
		items = append(items, item)
	}
	for lane, values := range lanes {
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, states), AnalyticsDuration: sampledDuration(values)})
	}
	sort.Slice(summary.Lanes, func(i, j int) bool { return summary.Lanes[i].Lane < summary.Lanes[j].Lane })
	for _, cause := range causes {
		summary.ReworkCauses = append(summary.ReworkCauses, cause)
	}
	sort.Slice(summary.ReworkCauses, func(i, j int) bool {
		a, b := summary.ReworkCauses[i], summary.ReworkCauses[j]
		if a.FromState != b.FromState {
			return a.FromState < b.FromState
		}
		return a.ReasonDetail < b.ReasonDetail
	})
	summary.SystemTotal, summary.HeldTotal = sampledDuration(system), sampledDuration(held)
	summary.LeadTotal, summary.QueueTotal = sampledDuration(lead), sampledDuration(queue)
	return summary, items
}

// sampledDuration is analyticsDuration keeping its sorted samples, so that
// durations read for several projects combine exactly.

func (s *Service) projectNativeAnalyticsResidenceTenant(ctx context.Context, scope nativeScope, r operatortool.AnalyticsRequest, out *nativeAnalyticsProject) error {
	live, _ := ctx.Value(analyticsLiveIssuesKey{}).(bool)
	after, cursor, key, err := s.analyticsResidenceSeek(ctx, scope, out.Window, live)
	if err != nil {
		return err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return workItemStorageUnavailable()
	}
	var page eventstore.AnalyticsResidencePage
	err = repository.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, reader, scope)
		if err != nil {
			return err
		}
		owner, ok := reader.(eventstore.AnalyticsResidenceReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsResidencePage(ctx, eventstore.AnalyticsResidenceQuery{ProjectID: scope.project, Principal: principal, From: out.Window.From, To: out.Window.To, Limit: maxAnalyticsPopulation, Live: live, IssuesAfter: after.IssuesAfter, EventsAfterItem: after.EventItem, EventsAfterSequence: after.EventSequence})
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
		issues[i] = analyticsResidenceIssue{id: string(issue.WorkItemID), created: issue.CreatedAt, initial: issue.InitialState, partial: after.EventItem != ""}
		byID[issue.WorkItemID] = i
		if after.EventItem != "" {
			issues[i].initial = ""
		}
		if page.EventsMore && issue.WorkItemID >= page.EventsAfterItem {
			issues[i].partial = true
		}
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, row := range page.Events {
		if row.OrganizationID != scope.organization || row.ProjectID != scope.project {
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
		issues[i].events = append(issues[i].events, analyticsResidenceEvent{at: value.RecordedAt, kind: value.Type, data: value.Data})
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
		paging.next["residence_issues"] = token
	}
	if page.EventsMore && len(key) > 0 {
		token, err := s.encodeAnalyticsResidenceCursor(cursor, key, analyticsResidenceCursor{IssuesAfter: after.IssuesAfter, EventItem: page.EventsAfterItem, EventSequence: page.EventsAfterSequence}, "residence_events")
		if err != nil {
			return err
		}
		paging.next["residence_events"] = token
	}
	clipped := page.IssuesMore || page.EventsMore || after.IssuesAfter != "" || after.EventItem != ""
	if (page.IssuesMore || page.EventsMore) && len(key) == 0 {
		out.Unavailable = append(out.Unavailable, "residence_continuation_requires_secret_keys")
	}
	return projectAnalyticsResidencePopulation(states, issues, clipped, r, out)
}
