package web

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/projectcolor"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const projectSmallMultipleSampleLimit = 12

type projectSmallMultipleRecorder struct {
	mu      sync.Mutex
	samples map[string][]templates.ProjectSmallMultipleSample
}

func newProjectSmallMultipleRecorder() *projectSmallMultipleRecorder {
	return &projectSmallMultipleRecorder{
		samples: map[string][]templates.ProjectSmallMultipleSample{},
	}
}

func (s *Server) projectSmallMultiples(ctx context.Context, snapshot telemetry.Snapshot) []templates.ProjectSmallMultiple {
	projects := projectSmallMultiplesFromSnapshot(snapshot)
	projects = s.addConfiguredProjectMultiples(projects)
	if len(projects) == 0 {
		return nil
	}

	now := snapshot.GeneratedAt
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Second)
	}
	now = now.UTC()
	overrideNow := time.Now().UTC().Truncate(time.Second)

	spend := s.projectSpend(ctx, projectSmallMultipleIDs(projects), now)
	for i := range projects {
		if value, ok := spend[projects[i].ID]; ok {
			projects[i].CurrentSpendUSD = value
		}
		projects[i].BudgetResetAt = dailyBudgetReset(now)
		projects[i].BudgetObservedAt = overrideNow
	}
	s.applyBudgetOverrides(ctx, projects, overrideNow)
	return s.projects.record(now, projects)
}

func (s *Server) cachedProjectSmallMultiples(snapshot telemetry.Snapshot) []templates.ProjectSmallMultiple {
	projects := projectSmallMultiplesFromSnapshot(snapshot)
	projects = s.addConfiguredProjectMultiples(projects)
	if len(projects) == 0 {
		return nil
	}

	now := snapshot.GeneratedAt
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Second)
	}
	now = now.UTC()
	for i := range projects {
		projects[i].BudgetResetAt = dailyBudgetReset(now)
	}
	return s.projects.record(now, projects)
}

func projectSmallMultiplesFromSnapshot(snapshot telemetry.Snapshot) []templates.ProjectSmallMultiple {
	if len(snapshot.Projects) > 0 {
		projects := make([]templates.ProjectSmallMultiple, 0, len(snapshot.Projects))
		for _, project := range snapshot.Projects {
			projects = append(projects, projectSmallMultipleFromSnapshot(project, telemetry.BoardWorkloadForProject(snapshot, projectID(project.Project))))
		}
		return projects
	}
	if snapshot.Project == (telemetry.Project{}) {
		return nil
	}
	workload := telemetry.BoardWorkload(snapshot)
	return []templates.ProjectSmallMultiple{
		{
			ID:           projectID(snapshot.Project),
			Name:         strings.TrimSpace(snapshot.Project.DisplayName),
			URL:          strings.TrimSpace(snapshot.Project.URL),
			Color:        snapshotProjectColor(snapshot.Project.Color),
			Pool:         strings.TrimSpace(snapshot.Project.Pool),
			Running:      snapshot.Counts.Running,
			QueueCount:   snapshot.Counts.Queue,
			Blocked:      snapshot.Counts.Blocked,
			BoardLoad:    workload.Load,
			BoardTodo:    workload.Todo,
			BoardActive:  workload.Active,
			BoardWaiting: workload.Waiting,
			BoardBlocked: workload.Blocked,
			Completed:    snapshot.Counts.Completed,
			TotalTokens:  snapshot.Tokens.Total,
		},
	}
}

func projectSmallMultipleFromSnapshot(project telemetry.ProjectSnapshot, workload telemetry.BoardWorkloadCounts) templates.ProjectSmallMultiple {
	return templates.ProjectSmallMultiple{
		ID:                        projectID(project.Project),
		Name:                      strings.TrimSpace(project.Project.DisplayName),
		URL:                       strings.TrimSpace(project.Project.URL),
		Color:                     snapshotProjectColor(project.Project.Color),
		Pool:                      strings.TrimSpace(project.Project.Pool),
		Running:                   project.Counts.Running,
		QueueCount:                project.Counts.Queue,
		Blocked:                   project.Counts.Blocked,
		BoardLoad:                 workload.Load,
		BoardTodo:                 workload.Todo,
		BoardActive:               workload.Active,
		BoardWaiting:              workload.Waiting,
		BoardBlocked:              workload.Blocked,
		Completed:                 project.Counts.Completed,
		TotalTokens:               project.Tokens.Total,
		ThroughputTokensPerSecond: project.Throughput.TokensPerSecond,
	}
}

func projectID(project telemetry.Project) string {
	if id := strings.TrimSpace(project.ID); id != "" {
		return id
	}
	if name := strings.TrimSpace(project.DisplayName); name != "" {
		return name
	}
	return ""
}

func (s *Server) addConfiguredProjectMultiples(projects []templates.ProjectSmallMultiple) []templates.ProjectSmallMultiple {
	if s.registry == nil {
		return projects
	}

	configured := map[string]templates.ProjectSmallMultiple{}
	for _, trackedProject := range s.registry.List() {
		if trackedProject == nil {
			continue
		}
		id := strings.TrimSpace(string(trackedProject.ID()))
		if id == "" {
			continue
		}
		projectConfig := trackedProject.Config()
		configured[id] = templates.ProjectSmallMultiple{
			ID:             id,
			Name:           id,
			URL:            trackerProjectURL(trackedProject),
			Color:          configuredProjectColor(projectConfig.Color),
			Pool:           strings.TrimSpace(projectConfig.Pool),
			Paused:         trackedProject.Paused(),
			PauseReason:    projectConfig.PausedReason,
			PauseIssue:     projectConfig.PausedUntilIssue,
			PauseUntil:     projectConfig.PausedUntil,
			BudgetEnabled:  trackedProject.Workflow().Config.Budget.Enabled,
			PerDayMaxUSD:   trackedProject.Workflow().Config.Budget.PerDayMaxUSD,
			PerIssueMaxUSD: trackedProject.Workflow().Config.Budget.PerIssueMaxUSD,
		}
	}

	seen := map[string]struct{}{}
	for _, project := range projects {
		if id := strings.TrimSpace(project.ID); id != "" {
			seen[id] = struct{}{}
		}
	}
	for i := range projects {
		id := strings.TrimSpace(projects[i].ID)
		if configuredProject, ok := configured[id]; ok {
			projects[i].Pool = configuredProject.Pool
			projects[i].Paused = configuredProject.Paused
			projects[i].PauseReason = configuredProject.PauseReason
			projects[i].PauseIssue = configuredProject.PauseIssue
			projects[i].PauseUntil = configuredProject.PauseUntil
			projects[i].BudgetEnabled = configuredProject.BudgetEnabled
			projects[i].PerDayMaxUSD = configuredProject.PerDayMaxUSD
			projects[i].PerIssueMaxUSD = configuredProject.PerIssueMaxUSD
			if strings.TrimSpace(projects[i].URL) == "" {
				projects[i].URL = configuredProject.URL
			}
			if strings.TrimSpace(configuredProject.Color) != "" {
				projects[i].Color = configuredProject.Color
			}
		}
	}
	for id, configuredProject := range configured {
		if _, ok := seen[id]; ok {
			continue
		}
		projects = append(projects, configuredProject)
		seen[id] = struct{}{}
	}
	return projects
}

func (s *Server) applyBudgetOverrides(ctx context.Context, projects []templates.ProjectSmallMultiple, now time.Time) {
	overrides, ok := s.store.(store.BudgetOverrideStore)
	if !ok {
		return
	}
	active, err := overrides.ListActiveBudgetOverrides(ctx, now)
	if err != nil {
		s.logger.Warn("budget override query failed", slog.Any("error", err))
		return
	}
	byProject := make(map[string]store.BudgetOverride, len(active))
	for _, override := range active {
		byProject[override.ProjectID] = override
	}
	for i := range projects {
		override, ok := byProject[projects[i].ID]
		if !ok {
			continue
		}
		projects[i].BudgetOverride = &telemetry.BudgetOverride{
			ProjectID:      override.ProjectID,
			PerDayMaxUSD:   override.PerDayMaxUSD,
			PerIssueMaxUSD: override.PerIssueMaxUSD,
			ExpiresAt:      override.ExpiresAt,
			CreatedAt:      override.CreatedAt,
			Reason:         override.Reason,
		}
		if override.PerDayMaxUSD != nil {
			projects[i].PerDayMaxUSD = *override.PerDayMaxUSD
		}
		if override.PerIssueMaxUSD != nil {
			projects[i].PerIssueMaxUSD = *override.PerIssueMaxUSD
		}
	}
}

func dailyBudgetReset(now time.Time) time.Time {
	_, end := dailyBudgetPeriod(now)
	return end
}

func configuredProjectColor(value string) string {
	color, ok := projectcolor.Normalize(value)
	if !ok {
		return ""
	}
	return color
}

func snapshotProjectColor(value string) string {
	color, ok := projectcolor.Normalize(value)
	if !ok {
		return ""
	}
	return color
}

func trackerProjectURL(trackedProject *projectpkg.Project) string {
	if trackedProject == nil {
		return ""
	}
	tracker := trackedProject.Workflow().Config.Tracker
	slug := strings.TrimSpace(tracker.ProjectSlug)
	if strings.HasPrefix(slug, "http://") || strings.HasPrefix(slug, "https://") {
		return slug
	}
	if repoIssuesURL := githubRepositoryIssuesURL(tracker.Repository); repoIssuesURL != "" {
		return repoIssuesURL
	}
	return ""
}

func githubRepositoryIssuesURL(repository string) string {
	owner, name, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return "https://github.com/" + owner + "/" + name + "/issues"
}

func projectSmallMultipleIDs(projects []templates.ProjectSmallMultiple) []string {
	ids := make([]string, 0, len(projects))
	seen := map[string]struct{}{}
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		ids = append(ids, id)
		seen[id] = struct{}{}
	}
	return ids
}

func (s *Server) projectSpend(ctx context.Context, projectIDs []string, now time.Time) map[string]float64 {
	if s.store == nil || len(projectIDs) == 0 {
		return nil
	}
	periodStart, periodEnd := dailyBudgetPeriod(now)
	events, err := s.store.BudgetCostEvents(ctx, store.BudgetCostQuery{
		ProjectIDs: projectIDs,
		From:       periodStart,
		To:         periodEnd,
	})
	if err != nil {
		s.logger.Warn("project spend query failed", slog.Any("error", err))
		return nil
	}

	spend := map[string]float64{}
	for _, event := range events {
		projectID := strings.TrimSpace(event.ProjectID)
		at := event.At.UTC()
		if projectID == "" || event.CostUSD <= 0 || at.IsZero() || at.Before(periodStart) || !at.Before(periodEnd) {
			continue
		}
		spend[projectID] += event.CostUSD
	}
	return spend
}

func (r *projectSmallMultipleRecorder) record(now time.Time, projects []templates.ProjectSmallMultiple) []templates.ProjectSmallMultiple {
	if r == nil {
		return projects
	}
	now = now.UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]templates.ProjectSmallMultiple, len(projects))
	copy(out, projects)
	seen := map[string]struct{}{}
	for i := range out {
		id := strings.TrimSpace(out[i].ID)
		if id == "" {
			id = strings.TrimSpace(out[i].Name)
		}
		if id == "" {
			continue
		}
		seen[id] = struct{}{}

		samples := r.samples[id]
		throughput := out[i].ThroughputTokensPerSecond
		if len(samples) > 0 {
			previous := samples[len(samples)-1]
			if out[i].TotalTokens < previous.TotalTokens {
				samples = nil
				throughput = 0
			} else if now.After(previous.At) {
				elapsed := now.Sub(previous.At).Seconds()
				if elapsed > 0 {
					throughput = float64(out[i].TotalTokens-previous.TotalTokens) / elapsed
				}
			}
		}

		out[i].ThroughputTokensPerSecond = throughput
		sample := templates.ProjectSmallMultipleSample{
			At:                        now,
			Running:                   out[i].Running,
			TotalTokens:               out[i].TotalTokens,
			ThroughputTokensPerSecond: throughput,
			SpendUSD:                  out[i].CurrentSpendUSD,
			QueueDepth:                out[i].QueueCount,
			Blocked:                   out[i].Blocked,
			Completed:                 out[i].Completed,
		}
		if len(samples) > 0 && !now.After(samples[len(samples)-1].At) {
			samples[len(samples)-1] = sample
		} else {
			samples = append(samples, sample)
		}
		if len(samples) > projectSmallMultipleSampleLimit {
			samples = append([]templates.ProjectSmallMultipleSample(nil), samples[len(samples)-projectSmallMultipleSampleLimit:]...)
		}
		r.samples[id] = samples
		out[i].Samples = append([]templates.ProjectSmallMultipleSample(nil), samples...)
	}
	for id := range r.samples {
		if _, ok := seen[id]; !ok {
			delete(r.samples, id)
		}
	}
	return out
}
