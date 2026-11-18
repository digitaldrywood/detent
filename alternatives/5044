package statuspage

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	defaultBaselineInterval = 30 * time.Minute
	defaultActiveInterval   = time.Minute
	defaultScanInterval     = 15 * time.Second
)

type Fetcher interface {
	Fetch(context.Context, Source) (Report, error)
}

type ManagerConfig struct {
	BaselineInterval time.Duration
	ActiveInterval   time.Duration
	ScanInterval     time.Duration
}

type ManagerDependencies struct {
	Fetcher Fetcher
	Logger  *slog.Logger
}

type cacheEntry struct {
	Report      Report
	AttemptedAt time.Time
	Failed      bool
}

type Manager struct {
	config  ManagerConfig
	fetcher Fetcher
	logger  *slog.Logger
	pollMu  sync.Mutex
	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
}

func NewManager(cfg ManagerConfig, deps ManagerDependencies) *Manager {
	if cfg.BaselineInterval <= 0 {
		cfg.BaselineInterval = defaultBaselineInterval
	}
	if cfg.ActiveInterval <= 0 {
		cfg.ActiveInterval = defaultActiveInterval
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = defaultScanInterval
	}
	if deps.Fetcher == nil {
		deps.Fetcher = NewClient(ClientConfig{})
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Manager{
		config:  cfg,
		fetcher: deps.Fetcher,
		logger:  deps.Logger,
		cache:   map[string]cacheEntry{},
	}
}

func (m *Manager) Run(ctx context.Context, sources func() []Source, conditions func() []telemetry.TrackerCondition, now func() time.Time) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sources == nil {
		return
	}
	if conditions == nil {
		conditions = func() []telemetry.TrackerCondition { return nil }
	}
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(m.config.ScanInterval)
	defer ticker.Stop()
	for {
		m.Poll(ctx, sources(), conditions(), now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) Poll(ctx context.Context, sources []Source, conditions []telemetry.TrackerCondition, now time.Time) {
	if m == nil {
		return
	}
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	normalized := normalizeSources(sources)
	unique := uniqueSources(normalized)
	active := activeSourceURLs(normalized, conditions)
	for _, source := range unique {
		interval := m.config.BaselineInterval
		if _, ok := active[source.BaseURL]; ok {
			interval = m.config.ActiveInterval
		}
		entry, ok := m.cached(source.BaseURL)
		if ok && now.Sub(entry.AttemptedAt) < interval {
			continue
		}
		report, err := m.fetcher.Fetch(ctx, source)
		m.cacheMu.Lock()
		m.cache[source.BaseURL] = cacheEntry{Report: report, AttemptedAt: now, Failed: err != nil}
		m.cacheMu.Unlock()
		if err != nil && ctx.Err() == nil {
			m.logger.Warn("provider status poll failed", "provider", source.Provider, "source_url", source.BaseURL, "error", err)
		}
	}
}

func (m *Manager) Enrich(snapshot telemetry.Snapshot, sources []Source) telemetry.Snapshot {
	if m == nil || len(snapshot.TrackerUnavailable) == 0 {
		return snapshot
	}
	normalized := normalizeSources(sources)
	snapshot.TrackerUnavailable = append([]telemetry.TrackerCondition(nil), snapshot.TrackerUnavailable...)
	for index := range snapshot.TrackerUnavailable {
		condition := &snapshot.TrackerUnavailable[index]
		source, ok := sourceForCondition(normalized, *condition)
		if !ok {
			continue
		}
		status := telemetry.ProviderStatus{
			Provider:  source.Provider,
			SourceURL: source.BaseURL,
			State:     telemetry.ProviderStatusPending,
		}
		entry, ok := m.cached(source.BaseURL)
		if !ok {
			condition.ProviderStatus = &status
			continue
		}
		status.CheckedAt = entry.AttemptedAt
		if entry.Failed {
			status.State = telemetry.ProviderStatusUnavailable
			condition.ProviderStatus = &status
			continue
		}
		incident, ok := corroboratingIncident(entry.Report, source)
		if !ok {
			status.State = telemetry.ProviderStatusNoMatch
			condition.ProviderStatus = &status
			continue
		}
		status.State = telemetry.ProviderStatusCorroborated
		status.Incident = &incident
		condition.ProviderStatus = &status
	}
	return snapshot
}

func (m *Manager) cached(baseURL string) (cacheEntry, bool) {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	entry, ok := m.cache[baseURL]
	return entry, ok
}

func uniqueSources(sources []Source) []Source {
	byURL := map[string]Source{}
	order := make([]string, 0, len(sources))
	for _, source := range sources {
		normalized, ok := source.normalize()
		if !ok {
			continue
		}
		if existing, found := byURL[normalized.BaseURL]; found {
			existing.RelevantComponents = append(existing.RelevantComponents, normalized.RelevantComponents...)
			existing, _ = existing.normalize()
			byURL[normalized.BaseURL] = existing
			continue
		}
		byURL[normalized.BaseURL] = normalized
		order = append(order, normalized.BaseURL)
	}
	result := make([]Source, 0, len(order))
	for _, baseURL := range order {
		result = append(result, byURL[baseURL])
	}
	return result
}

func normalizeSources(sources []Source) []Source {
	result := make([]Source, 0, len(sources))
	for _, source := range sources {
		if normalized, ok := source.normalize(); ok {
			result = append(result, normalized)
		}
	}
	return result
}

func activeSourceURLs(sources []Source, conditions []telemetry.TrackerCondition) map[string]struct{} {
	active := map[string]struct{}{}
	for _, condition := range conditions {
		if source, ok := sourceForCondition(sources, condition); ok {
			active[source.BaseURL] = struct{}{}
		}
	}
	return active
}

func sourceForCondition(sources []Source, condition telemetry.TrackerCondition) (Source, bool) {
	projectID := strings.TrimSpace(condition.ProjectID)
	connector := strings.ToLower(strings.TrimSpace(condition.Connector))
	for _, source := range sources {
		if projectID != "" && source.ProjectID == projectID {
			return source, true
		}
	}
	for _, source := range sources {
		if connector != "" && source.Connector == connector {
			return source, true
		}
	}
	return Source{}, false
}

func corroboratingIncident(report Report, source Source) (telemetry.ProviderIncident, bool) {
	relevant := make(map[string]struct{}, len(source.RelevantComponents))
	for _, component := range source.RelevantComponents {
		relevant[strings.ToLower(strings.TrimSpace(component))] = struct{}{}
	}
	var selected incidentPayload
	var selectedComponents []string
	for _, incident := range report.Incidents {
		components := matchingComponents(incident, report.Components, relevant)
		if len(relevant) > 0 && len(components) == 0 {
			continue
		}
		if selected.ID == "" || incidentImpactRank(incident.Impact) > incidentImpactRank(selected.Impact) || (incident.Impact == selected.Impact && incident.UpdatedAt.After(selected.UpdatedAt)) {
			selected = incident
			selectedComponents = components
		}
	}
	if selected.ID == "" {
		return telemetry.ProviderIncident{}, false
	}
	return telemetry.ProviderIncident{
		Name:       selected.Name,
		URL:        selected.Shortlink,
		Status:     operatorIncidentStatus(selected.Status),
		Impact:     selected.Impact,
		Components: selectedComponents,
		UpdatedAt:  selected.UpdatedAt,
	}, true
}

func matchingComponents(incident incidentPayload, summary []componentPayload, relevant map[string]struct{}) []string {
	names := make([]string, 0, len(incident.Components))
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || slices.Contains(names, name) {
			return
		}
		if len(relevant) > 0 {
			if _, ok := relevant[strings.ToLower(name)]; !ok {
				return
			}
		}
		names = append(names, name)
	}
	for _, component := range incident.Components {
		appendName(component.Name)
	}
	for _, update := range incident.IncidentUpdates {
		for _, component := range update.AffectedComponents {
			appendName(component.Name)
		}
	}
	if len(names) > 0 || len(incident.Components) > 0 {
		return names
	}
	for _, component := range summary {
		if component.Status != "operational" {
			appendName(component.Name)
		}
	}
	return names
}

func incidentImpactRank(impact string) int {
	switch strings.TrimSpace(impact) {
	case "critical":
		return 4
	case "major":
		return 3
	case "minor":
		return 2
	case "maintenance":
		return 1
	default:
		return 0
	}
}

func operatorIncidentStatus(status string) string {
	switch strings.TrimSpace(status) {
	case "identified":
		return "mitigating"
	case "in_progress":
		return "in progress"
	default:
		return strings.ReplaceAll(strings.TrimSpace(status), "_", " ")
	}
}

var _ Fetcher = (*Client)(nil)
