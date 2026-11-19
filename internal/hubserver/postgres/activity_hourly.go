package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

const (
	hourlyAttemptLimit = 10000
	hourlyLandingLimit = 256
	hourlyHistoryLimit = 256
	hourlyGroupLimit   = 512
)

func (s *session) ActivityHourly(ctx context.Context, filter activitystore.HourlyFilter) (activitystore.HourlyFacts, error) {
	var out activitystore.HourlyFacts
	if filter.Project == "" || !filter.From.Before(filter.To) || filter.Limit < 1 || filter.Limit > 200 {
		return out, corestore.ErrInvalidPage
	}
	filter.From, filter.To = hourlyTimeBoundary(filter.From), hourlyTimeBoundary(filter.To)
	population, err := s.queries.ActivityHourlyWindowPopulation(ctx, pgquery.ActivityHourlyWindowPopulationParams{
		OrganizationID: s.organization, ProjectID: filter.Project, WindowFrom: filter.From, WindowTo: filter.To,
		PopulationLimit: hourlyAttemptLimit + 1, LandingLimit: hourlyLandingLimit + 1, Principal: filter.Principal,
	})
	if err != nil {
		return out, fmt.Errorf("read hourly activity population: %w", mapError(ctx, err))
	}
	if population.Attempts > hourlyAttemptLimit || population.Landings > hourlyLandingLimit {
		return out, corestore.ErrInvalidPage
	}
	if population.Undated > hourlyLandingLimit && !population.UndatedLandingHistory {
		return out, corestore.ErrInvalidPage
	}
	out.UndatedLandingHistory = population.UndatedLandingHistory
	if population.Attempts > 0 {
		if err := s.readHourlyOutcomes(ctx, filter, &out); err != nil {
			return out, err
		}
	}
	if population.Landings > 0 {
		if err := s.readHourlyLandings(ctx, filter, &out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *session) readHourlyOutcomes(ctx context.Context, filter activitystore.HourlyFilter, out *activitystore.HourlyFacts) error {
	groups, err := s.queries.ActivityHourlyOutcomes(ctx, pgquery.ActivityHourlyOutcomesParams{
		OrganizationID: s.organization, ProjectID: filter.Project, WindowFrom: filter.From, WindowTo: filter.To, Principal: filter.Principal,
		GroupLimit: hourlyGroupLimit + 1, PopulationLimit: hourlyAttemptLimit,
	})
	if err != nil {
		return fmt.Errorf("aggregate hourly activity outcomes: %w", mapError(ctx, err))
	}
	if len(groups) > hourlyGroupLimit {
		return corestore.ErrInvalidPage
	}
	for _, g := range groups {
		var succeeded *bool
		if g.VerdictSuccess.Valid {
			succeeded = &g.VerdictSuccess.Bool
		}
		out.Outcomes = append(out.Outcomes, activitystore.HourlyOutcome{Hour: g.Hour.UTC(), EndingStage: g.EndingStage, Outcome: g.Outcome,
			FailureReason: g.FailureReason, Verdict: g.Verdict, VerdictSucceeded: succeeded, Reason: g.Reason, NextStep: g.NextStep, Disposition: g.Disposition,
			Attempts: g.Attempts, Seconds: g.Elapsed, AcceptedNoChange: g.AcceptedNoChange, UnknownNoChange: g.UnknownNoChange})
	}
	if len(groups) == 0 {
		return nil
	}
	attempts, err := s.queries.ActivityHourlyAttemptIDs(ctx, pgquery.ActivityHourlyAttemptIDsParams{
		OrganizationID: s.organization, ProjectID: filter.Project, WindowFrom: filter.From, WindowTo: filter.To, Principal: filter.Principal,
		PopulationLimit: hourlyAttemptLimit, ReferenceLimit: int32(filter.Limit + 1),
	})
	if err != nil {
		return fmt.Errorf("read hourly activity attempt IDs: %w", mapError(ctx, err))
	}
	for _, a := range attempts {
		out.Attempts = append(out.Attempts, activitystore.AttemptReference{ID: a.ID, WorkItemID: a.WorkItemID, FinishedAt: a.FinishedAt})
	}
	return nil
}

func (s *session) readHourlyLandings(ctx context.Context, filter activitystore.HourlyFilter, out *activitystore.HourlyFacts) error {
	landings, err := s.queries.ActivityHourlyLandings(ctx, pgquery.ActivityHourlyLandingsParams{
		OrganizationID: s.organization, ProjectID: filter.Project, WindowFrom: filter.From, WindowTo: filter.To, Principal: filter.Principal,
		LandingLimit: hourlyLandingLimit, HistoryLimit: hourlyHistoryLimit + 1,
	})
	if err != nil {
		return fmt.Errorf("read hourly activity landing cohorts: %w", mapError(ctx, err))
	}
	for _, l := range landings {
		out.Landings = append(out.Landings, activitystore.LandingCohort{ID: l.ID, ChangeID: l.ChangeID, AttemptID: l.AttemptID,
			LandedAt: *l.LandedAt, LinkedAttempts: l.LinkedAttempts, HistoryCoverage: l.HistoryCoverage})
	}
	return nil
}

func hourlyTimeBoundary(at time.Time) time.Time {
	micro := at.Truncate(time.Microsecond)
	if at.After(micro) {
		return micro.Add(time.Microsecond)
	}
	return micro
}
