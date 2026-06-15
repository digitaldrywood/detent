package web

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
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

	spend := s.projectSpend(ctx, projectSmallMultipleIDs(projects), now)
	for i := range projects {
		if value, ok := spend[projects[i].ID]; ok {
			projects[i].CurrentSpendUSD = value
		}
	}
	return s.projects.record(now, projects)
}

func projectSmallMultiplesFromSnapshot(snapshot telemetry.Snapshot) []templates.ProjectSmallMultiple {
	if len(snapshot.Projects) > 0 {
		projects := make([]templates.ProjectSmallMultiple, 0, len(snapshot.Projects))
		for _, project := range snapshot.Projects {
			projects = append(projects, projectSmallMultipleFromSnapshot(project))
		}
		return projects
	}
	if snapshot.Project == (telemetry.Project{}) {
		return nil
	}
	return []templates.ProjectSmallMultiple{
		{
			ID:          projectID(snapshot.Project),
			Name:        strings.TrimSpace(snapshot.Project.DisplayName),
			URL:         strings.TrimSpace(snapshot.Project.URL),
			Running:     snapshot.Counts.Running,
			QueueCount:  snapshot.Counts.Queue,
			Blocked:     snapshot.Counts.Blocked,
			Completed:   snapshot.Counts.Completed,
			TotalTokens: snapshot.Tokens.Total,
		},
	}
}

func projectSmallMultipleFromSnapshot(project telemetry.ProjectSnapshot) templates.ProjectSmallMultiple {
	return templates.ProjectSmallMultiple{
		ID:                        projectID(project.Project),
		Name:                      strings.TrimSpace(project.Project.DisplayName),
		URL:                       strings.TrimSpace(project.Project.URL),
		Running:                   project.Counts.Running,
		QueueCount:                project.Counts.Queue,
		Blocked:                   project.Counts.Blocked,
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
		configured[id] = templates.ProjectSmallMultiple{
			ID:     id,
			Name:   id,
			URL:    trackerProjectURL(trackedProject),
			Paused: trackedProject.Paused(),
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
			projects[i].Paused = configuredProject.Paused
			if strings.TrimSpace(projects[i].URL) == "" {
				projects[i].URL = configuredProject.URL
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

func trackerProjectURL(trackedProject *projectpkg.Project) string {
	if trackedProject == nil {
		return ""
	}
	slug := strings.TrimSpace(trackedProject.Workflow().Config.Tracker.ProjectSlug)
	if strings.HasPrefix(slug, "http://") || strings.HasPrefix(slug, "https://") {
		return slug
	}
	return ""
}

func (s *Server) projectWorkflowStates(projectID string) []string {
	if s.registry == nil {
		return nil
	}
	trackedProject, ok := s.registry.Get(projectpkg.ID(strings.TrimSpace(projectID)))
	if !ok || trackedProject == nil {
		return nil
	}
	return configuredWorkflowStates(trackedProject.Workflow().Config)
}

func configuredWorkflowStates(cfg workflowconfig.Config) []string {
	configured := make([]string, 0, len(cfg.Tracker.ActiveStates)+len(cfg.Tracker.ObservedStates)+len(cfg.Tracker.TerminalStates))
	configured = append(configured, cfg.Tracker.ActiveStates...)
	configured = append(configured, cfg.Tracker.ObservedStates...)
	configured = append(configured, cfg.Tracker.TerminalStates...)
	seen := map[string]string{}
	for _, state := range configured {
		display := strings.Join(strings.Fields(strings.TrimSpace(state)), " ")
		if display == "" {
			continue
		}
		key := workflowStateKey(display)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = display
	}

	states := make([]string, 0, len(seen))
	for _, state := range detentWorkflowStateOrder() {
		key := workflowStateKey(state)
		display, ok := seen[key]
		if !ok {
			continue
		}
		states = append(states, display)
		delete(seen, key)
	}
	for _, state := range configured {
		display := strings.Join(strings.Fields(strings.TrimSpace(state)), " ")
		key := workflowStateKey(display)
		if display == "" {
			continue
		}
		if _, ok := seen[key]; !ok {
			continue
		}
		states = append(states, display)
		delete(seen, key)
	}
	return states
}

func detentWorkflowStateOrder() []string {
	return []string{
		"Backlog",
		"Todo",
		"In Progress",
		"Blocked",
		"Human Review",
		"Rework",
		"Merging",
		"Done",
		"Cancelled",
		"Canceled",
		"Closed",
		"Duplicate",
	}
}

func workflowStateKey(state string) string {
	state = strings.ToLower(strings.TrimSpace(state))
	replacer := strings.NewReplacer(" ", "", "-", "", "_", "")
	return replacer.Replace(state)
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
