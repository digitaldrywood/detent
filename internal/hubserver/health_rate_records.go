package hubserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHealthRateRecords(ctx context.Context, organization tracker.OrganizationID, now time.Time, runners []healthRunnerState, snapshot *healthSnapshot) error {
	for index := range snapshot.Projects {
		project := &snapshot.Projects[index]
		p := healthRateProject{ID: project.ID, Candidates: project.Candidates, Costs: map[string]healthItemCost{}, BaselineCosts: map[string]healthItemCost{}}
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{tracker.ProjectID(p.ID)}, From: now.Add(-healthBaselineWindow - healthLongWindow), To: now, Limit: 5000}
		var history []reportstore.ProjectHistory
		var attempts []reportstore.AttemptFact
		var attemptRuns []tenantAttemptSnapshot
		var landings []reportstore.LandingFact
		var usage []healthstore.UsageObservation
		archived := map[tracker.NativeWorkItemID]bool{}
		repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
		if !ok {
			return workItemStorageUnavailable()
		}
		err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			h, ok := r.(reportstore.HistoryReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			a, ok := r.(reportstore.AttemptReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			l, ok := r.(reportstore.LandingReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			u, ok := r.(healthstore.ObservationReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			var err error
			history, err = h.ReadHistory(ctx, query)
			if err != nil {
				return fmt.Errorf("read health residence: %w", err)
			}
			var ids []tracker.NativeWorkItemID
			for _, history := range history {
				for _, issue := range history.Issues {
					ids = append(ids, issue.WorkItemID)
				}
			}
			archived, err = u.HealthArchivedWorkItems(ctx, tracker.ProjectID(p.ID), ids)
			if err != nil {
				return err
			}
			attempts, err = a.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: query.Projects, From: query.From, To: query.To, Limit: query.Limit})
			if err != nil {
				return fmt.Errorf("read health attempts: %w", err)
			}
			for _, attempt := range attempts {
				var input tenantAttemptSnapshot
				input.attempt, err = r.Attempt(ctx, attempt.AttemptID)
				if err != nil {
					return err
				}
				input.lease, err = r.Lease(ctx, input.attempt.LeaseID)
				if err != nil {
					return err
				}
				input.event, err = r.LatestRunEvent(ctx, attempt.ProjectID, attempt.AttemptID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if input.event.ID == "" {
					input.event = eventstore.Event{OrganizationID: organization, ProjectID: attempt.ProjectID, WorkItemID: attempt.WorkItemID}
				}
				if err := readTenantAttemptRunPayload(ctx, r, &input); err != nil {
					return err
				}
				attemptRuns = append(attemptRuns, input)
			}
			landings, err = l.ReadLandingFacts(ctx, query)
			if err != nil {
				return err
			}
			usage, err = u.HealthUsage(ctx, tracker.ProjectID(p.ID), now.Add(-healthLongWindow), now, healthReadLimit+1)
			return err
		})
		if err != nil {
			return err
		}
		if len(usage) > healthReadLimit {
			return errHealthReadLimit
		}
		for _, history := range history {
			states := make(map[string]tracker.NativeState, len(history.States))
			for _, state := range history.States {
				states[state.Name] = state
			}
			for _, item := range history.Issues {
				if archived[item.WorkItemID] {
					continue
				}
				issue := analyticsResidenceIssue{id: string(item.WorkItemID), created: item.CreatedAt, initial: item.InitialState}
				for _, event := range item.Events {
					issue.events = append(issue.events, analyticsResidenceEvent{at: event.At, kind: event.Type, data: tracker.CollaborationData{FromState: event.FromState, ToState: event.ToState}})
				}
				p.Timelines = append(p.Timelines, buildAnalyticsResidenceTimeline(issue, states, now))
			}
		}
		attemptCosts := map[string]float64{}
		for _, usage := range usage {
			current := p.Costs[usage.Item]
			current.Known = true
			current.Cost += usage.Cost
			p.Costs[usage.Item] = current
			baseline := p.BaselineCosts[usage.Item]
			baseline.Known = baseline.Known || usage.BaselineKnown
			baseline.Cost += usage.BaselineCost
			p.BaselineCosts[usage.Item] = baseline
			attemptCosts[usage.AttemptID] = usage.Cost
		}
		for i, attempt := range attempts {
			input := attemptRuns[i]
			a := healthRateAttempt{ID: attempt.AttemptID, Item: string(attempt.WorkItemID), Status: input.attempt.Status, Runner: attempt.RunnerID, At: input.attempt.UpdatedAt, Started: input.attempt.StartedAt, Cost: attemptCosts[attempt.AttemptID]}
			run, err := s.readNativeAttemptRunTenant(ctx, input)
			if err != nil {
				return err
			}
			if err := nativeAttemptRunMismatch(input, run); err != nil {
				return err
			}
			applyHealthAttemptData(&a, run)
			p.Attempts = append(p.Attempts, a)
		}
		canonical := map[string]bool{}
		for _, landing := range landings {
			if !landing.Complete || landing.VersionID == "" || landing.HeadSHA == "" || landing.MergeSHA == "" {
				continue
			}
			canonical[landing.ChangeID+"\x00"+landing.VersionID+"\x00"+landing.MergeSHA] = true
			p.Attempts = append(p.Attempts, healthRateAttempt{ID: landing.ChangeID, Item: string(landing.WorkItemID), Landing: true, Landed: true, LandingAt: landing.At})
			if landing.At.After(project.LastLanding) {
				project.LastLanding = landing.At
			}
		}
		for i := range p.Attempts {
			if canonical[p.Attempts[i].LandingKey] {
				p.Attempts[i].Landing, p.Attempts[i].Landed = false, false
			}
		}
		for _, runner := range runners {
			if runner.Routing.State == "active" && slices.Contains(runner.Projects, p.ID) {
				p.Runners = append(p.Runners, runner.healthRunner)
			}
		}
		snapshot.BaselineUnavailable = append(snapshot.BaselineUnavailable, healthBaselineGaps(p, buildHealthRateBaseline(now, p))...)
		snapshot.Rates = append(snapshot.Rates, p)
	}
	return nil
}
