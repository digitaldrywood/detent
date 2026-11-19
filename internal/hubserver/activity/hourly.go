package activity

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHourly(ctx context.Context, caller Caller, request operatortool.ActivityRequest, window operatortool.AnalyticsWindow) (operatortool.ActivityHourlyReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	organization := tracker.OrganizationID(s.deps.Organization())
	project := tracker.ProjectID(request.ProjectID)
	filter := activitystore.HourlyFilter{Project: project, From: window.From, To: window.To, Limit: request.Limit}
	if caller.Credential.HostedRole == "reporter" {
		filter.Principal = caller.Scope.Actor.PrincipalID
		if filter.Principal == "" {
			return operatortool.ActivityHourlyReport{}, operatortool.ErrAccessDenied
		}
	}
	var facts activitystore.HourlyFacts
	err := s.readSnapshot(ctx, organization, func(r tenantstore.Reader) error {
		if _, _, err := s.readableProjects(ctx, caller, r, request.ProjectID); err != nil {
			return err
		}
		reader, ok := r.(activitystore.HourlyReader)
		if !ok {
			return s.deps.Unavailable
		}
		var err error
		facts, err = reader.ActivityHourly(ctx, filter)
		return err
	})
	if errors.Is(err, corestore.ErrInvalidPage) {
		err = hubapi.PublicInvalid("Hourly activity population exceeds the read limit. Use a shorter window.")
	}
	if err != nil {
		return operatortool.ActivityHourlyReport{}, err
	}
	return hourlyReport(organization, project, s.deps.Now().UTC(), window, request.Limit, facts), nil
}

func hourlyReport(organization tracker.OrganizationID, project tracker.ProjectID, observed time.Time, window operatortool.AnalyticsWindow, limit int, facts activitystore.HourlyFacts) operatortool.ActivityHourlyReport {
	out := operatortool.ActivityHourlyReport{OrganizationID: string(organization), ProjectID: string(project), ObservedAt: observed, Window: window,
		Hours: []operatortool.ActivityHour{}, Attempts: []operatortool.ActivityAttemptReference{}, Landings: []operatortool.ActivityLandingReference{},
		MoreAttempts: len(facts.Attempts) > limit, MoreLandings: len(facts.Landings) > limit}
	first := window.From.Truncate(time.Hour)
	for hour := first; hour.Before(window.To); hour = hour.Add(time.Hour) {
		out.Hours = append(out.Hours, operatortool.ActivityHour{Hour: hour, Outcomes: []operatortool.ActivityHourlyOutcome{}, LandingHistory: []operatortool.ActivityLandingHistory{}})
	}
	var attempts int64
	for _, group := range facts.Outcomes {
		hour := &out.Hours[int(group.Hour.Sub(first)/time.Hour)]
		hour.Outcomes = append(hour.Outcomes, operatortool.ActivityHourlyOutcome{EndingStage: group.EndingStage, Outcome: group.Outcome, FailureReason: group.FailureReason, Verdict: group.Verdict, VerdictSucceeded: group.VerdictSucceeded, Reason: group.Reason, NextStep: group.NextStep, Disposition: group.Disposition, Attempts: group.Attempts, ElapsedAttemptSeconds: group.Seconds})
		hour.TerminalAttempts += group.Attempts
		hour.ElapsedAttemptSeconds += group.Seconds
		hour.AcceptedNoChange += group.AcceptedNoChange
		hour.UnknownNoChange += group.UnknownNoChange
		attempts += group.Attempts
		switch group.Verdict {
		case "pending":
			hour.PendingVerdicts += group.Attempts
		case "unavailable":
			hour.UnavailableVerdicts += group.Attempts
		}
	}
	histories := make(map[time.Time]map[operatortool.ActivityLandingHistory]int64)
	for _, landing := range facts.Landings {
		bucket := landing.LandedAt.UTC().Truncate(time.Hour)
		out.Hours[int(bucket.Sub(first)/time.Hour)].VerifiedLandings++
		if histories[bucket] == nil {
			histories[bucket] = make(map[operatortool.ActivityLandingHistory]int64)
		}
		histories[bucket][operatortool.ActivityLandingHistory{LinkedAttempts: landing.LinkedAttempts, HistoryCoverage: landing.HistoryCoverage}]++
	}
	for i := range out.Hours {
		hour := &out.Hours[i]
		for history, count := range histories[hour.Hour] {
			history.Landings = count
			hour.LandingHistory = append(hour.LandingHistory, history)
		}
		slices.SortFunc(hour.LandingHistory, func(a, b operatortool.ActivityLandingHistory) int {
			return cmp.Or(cmp.Compare(a.LinkedAttempts, b.LinkedAttempts), cmp.Compare(a.HistoryCoverage, b.HistoryCoverage))
		})
	}
	if len(facts.Landings) > 0 {
		ratio := float64(attempts) / float64(len(facts.Landings))
		out.AttemptsPerLanding = &ratio
	}
	for _, a := range facts.Attempts[:min(limit, len(facts.Attempts))] {
		out.Attempts = append(out.Attempts, operatortool.ActivityAttemptReference{AttemptID: a.ID, WorkItemID: a.WorkItemID, FinishedAt: a.FinishedAt.UTC()})
	}
	for _, landing := range facts.Landings[:min(limit, len(facts.Landings))] {
		out.Landings = append(out.Landings, operatortool.ActivityLandingReference{LandingID: landing.ID, ChangeID: landing.ChangeID, LandingAttemptID: landing.AttemptID, LandedAt: landing.LandedAt.UTC(), LinkedAttempts: landing.LinkedAttempts, HistoryCoverage: landing.HistoryCoverage})
	}
	return out
}
