package web

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	dailyDigestVisibleDays  = 7
	dailyDigestBaselineDays = 7
)

func (s *Server) dailyDigestData(ctx context.Context, snapshot telemetry.Snapshot, projects []templates.ProjectSmallMultiple, location *time.Location, now time.Time) (templates.DailyDigestData, error) {
	if location == nil {
		location = time.UTC
	}
	windows := dailyDigestWindows(now, location, dailyDigestVisibleDays+dailyDigestBaselineDays)
	runtimeDays, err := s.store.DailyDigest(ctx, windows)
	if err != nil {
		return templates.DailyDigestData{}, err
	}

	projectNames := make(map[string]string, len(projects))
	for _, project := range projects {
		projectNames[strings.TrimSpace(project.ID)] = strings.TrimSpace(project.Name)
	}
	days := make([]templates.DailyDigestDayData, 0, len(runtimeDays))
	for index, runtimeDay := range runtimeDays {
		window := windows[index]
		efficiencyRollup, err := s.store.EfficiencyRollup(ctx, efficiency.Query{From: window.From, To: window.To})
		if err != nil {
			return templates.DailyDigestData{}, err
		}
		day := templates.DailyDigestDayData{
			Date:                 runtimeDay.Date,
			From:                 window.From,
			To:                   window.To,
			Sessions:             runtimeDay.Sessions,
			InputTokens:          runtimeDay.InputTokens,
			CachedInputTokens:    runtimeDay.CachedInputTokens,
			OutputTokens:         runtimeDay.OutputTokens,
			TotalTokens:          runtimeDay.TotalTokens,
			SpendUSD:             usageSpendUSD(runtimeDay.Models, s.pricing),
			OrphanResumed:        runtimeDay.OrphanResumed,
			OrphanFresh:          runtimeDay.OrphanFresh,
			CapacityOutages:      runtimeDay.CapacityOutages,
			CapacitySeconds:      runtimeDay.CapacitySeconds,
			CapacityRecoveryMode: runtimeDay.CapacityRecoveryMode,
			BreakerTrips:         runtimeDay.BreakerTrips,
			FailedSessions:       runtimeDay.FailedSessions,
			DominantErrorClass:   runtimeDay.DominantErrorClass,
			Efficiency:           efficiencyRollup.Current,
		}
		populateDailyDigestTracker(&day, snapshot, projectNames)
		days = append(days, day)
	}
	return templates.DailyDigestData{Timezone: location.String(), Days: days}, nil
}

func dailyDigestWindows(now time.Time, location *time.Location, count int) []store.DailyDigestWindow {
	localNow := now.In(location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	windows := make([]store.DailyDigestWindow, 0, count)
	for offset := 1 - count; offset <= 0; offset++ {
		from := today.AddDate(0, 0, offset)
		to := from.AddDate(0, 0, 1)
		windows = append(windows, store.DailyDigestWindow{
			Date: from.Format(time.DateOnly),
			From: from,
			To:   to,
		})
	}
	return windows
}

func populateDailyDigestTracker(day *templates.DailyDigestDayData, snapshot telemetry.Snapshot, projectNames map[string]string) {
	projects := map[string]*templates.DailyDigestProjectData{}
	project := func(id string) *templates.DailyDigestProjectData {
		id = strings.TrimSpace(id)
		if existing := projects[id]; existing != nil {
			return existing
		}
		name := strings.TrimSpace(projectNames[id])
		if name == "" {
			name = id
		}
		if name == "" {
			name = "Unassigned"
		}
		value := &templates.DailyDigestProjectData{ID: id, Name: name}
		projects[id] = value
		return value
	}

	for _, issue := range dailyDigestSnapshotIssues(snapshot) {
		projectID := strings.TrimSpace(issue.ProjectID)
		if projectID == "" {
			projectID = strings.TrimSpace(snapshot.Project.ID)
		}
		if timeInDigestWindow(issue.CreatedAt, day.From, day.To) {
			day.IssuesFiled++
			project(projectID).Filed++
		}
		if dailyDigestShippedState(issue.State) && timeInDigestWindow(issue.StageUpdatedAt, day.From, day.To) {
			day.IssuesShipped++
			project(projectID).Shipped++
		}
	}
	for _, release := range dailyDigestReleases(snapshot) {
		if timeInDigestWindow(release.LastReleaseAt, day.From, day.To) {
			projectID := strings.TrimSpace(release.ProjectID)
			if projectID == "" {
				projectID = strings.TrimSpace(snapshot.Project.ID)
			}
			day.ReleasesTagged++
			project(projectID).Releases++
		}
	}

	day.Projects = make([]templates.DailyDigestProjectData, 0, len(projects))
	for _, value := range projects {
		day.Projects = append(day.Projects, *value)
	}
	sort.Slice(day.Projects, func(i, j int) bool {
		return strings.ToLower(day.Projects[i].Name) < strings.ToLower(day.Projects[j].Name)
	})
}

func dailyDigestSnapshotIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := append([]telemetry.Issue(nil), snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	issues = append(issues, snapshot.TrackerDrift.UntrackedOpen...)
	issues = append(issues, snapshot.TrackerDrift.OpenTerminal...)
	for _, row := range snapshot.Running {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Queue {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Blocked {
		issues = append(issues, row.Issue)
	}
	for _, row := range snapshot.Completed {
		issues = append(issues, row.Issue)
	}

	unique := make([]telemetry.Issue, 0, len(issues))
	seen := map[string]struct{}{}
	for _, issue := range issues {
		key := strings.TrimSpace(issue.ProjectID) + "\x00" + strings.TrimSpace(issue.ID)
		if strings.TrimSpace(issue.ID) == "" {
			key = ""
		}
		if key == "" {
			key = strings.TrimSpace(issue.ProjectID) + "\x00" + strings.TrimSpace(issue.Identifier)
			if strings.TrimSpace(issue.Identifier) == "" {
				key = ""
			}
		}
		if key == "" {
			key = strings.TrimSpace(issue.URL)
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, issue)
	}
	return unique
}

func dailyDigestReleases(snapshot telemetry.Snapshot) []telemetry.Release {
	releases := append([]telemetry.Release(nil), snapshot.Releases...)
	if !snapshot.Release.IsZero() {
		releases = append(releases, snapshot.Release)
	}
	unique := make([]telemetry.Release, 0, len(releases))
	seen := map[string]struct{}{}
	for _, release := range releases {
		if release.LastReleaseAt == nil {
			continue
		}
		key := strings.TrimSpace(release.ProjectID) + "\x00" + strings.TrimSpace(release.LastRelease) + "\x00" + release.LastReleaseAt.UTC().Format(time.RFC3339Nano)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, release)
	}
	return unique
}

func dailyDigestShippedState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "done", "completed", "closed", "merged", "shipped":
		return true
	default:
		return false
	}
}

func timeInDigestWindow(value *time.Time, from time.Time, to time.Time) bool {
	return value != nil && !value.Before(from) && value.Before(to)
}
