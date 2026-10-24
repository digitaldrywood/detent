package scheduler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
)

var ErrNoCandidates = errors.New("scheduler has no project candidates")

type GlobalScheduler interface {
	Scheduler
	Reconfigurable
}

type ProjectCandidate struct {
	ID                       string
	Pool                     string
	Rank                     int
	Paused                   bool
	ActiveHours              activehours.Config
	ActiveHoursOverrideUntil time.Time
}

func (p ProjectCandidate) ActiveHoursStatus(now time.Time) (activehours.Status, error) {
	return activehours.Evaluate(p.ActiveHours, now, p.ActiveHoursOverrideUntil)
}

type globalScheduler struct {
	sem *CountingSemaphore
}

var (
	_ Scheduler       = (*globalScheduler)(nil)
	_ GlobalScheduler = (*globalScheduler)(nil)
)

func NewStrictPriority(cfg Config) GlobalScheduler {
	return &globalScheduler{sem: NewCountingSemaphore(cfg)}
}

func (s *globalScheduler) Mode() Mode { return ModeStrictPriority }

func (s *globalScheduler) Reconfigure(cfg Config) error {
	if _, err := globalModeFromConfig(cfg); err != nil {
		return err
	}
	s.sem.reconfigure(cfg)
	return nil
}

func (s *globalScheduler) RequestSlot(ctx context.Context, req SlotRequest) (Slot, error) {
	return s.sem.RequestSlot(ctx, req)
}

func (s *globalScheduler) ReleaseSlot(slot Slot) error {
	return s.sem.ReleaseSlot(slot)
}

func (s *globalScheduler) capacitySnapshot(state string) capacitySnapshot {
	return s.sem.capacitySnapshot(state)
}

func normalizeProjectCandidates(projects []ProjectCandidate) []ProjectCandidate {
	configured := normalizeConfiguredProjectCandidates(projects)
	candidates := make([]ProjectCandidate, 0, len(configured))
	for _, project := range configured {
		if !project.Paused {
			candidates = append(candidates, project)
		}
	}
	return candidates
}

func normalizeConfiguredProjectCandidates(projects []ProjectCandidate) []ProjectCandidate {
	candidates := make([]ProjectCandidate, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		project.ID = normalizeProjectID(project.ID)
		if project.ID == "" {
			continue
		}
		if _, ok := seen[project.ID]; ok {
			continue
		}
		seen[project.ID] = struct{}{}
		project.Pool = normalizePoolName(project.Pool)
		candidates = append(candidates, project)
	}
	return candidates
}

func normalizeProjectID(projectID string) string {
	return strings.TrimSpace(projectID)
}

func normalizePoolName(pool string) string {
	pool = strings.TrimSpace(pool)
	if pool == "" {
		return DefaultPoolName
	}
	return pool
}

func globalModeFromConfig(cfg Config) (Mode, error) {
	switch normalizeKind(cfg.Kind) {
	case "", "strict", "strict_priority", "strictpriority", "weighted", "weighted_fair", "weightedfair", "round_robin", "roundrobin", "fair_share", "fairshare":
		return ModeStrictPriority, nil
	default:
		return "", ErrUnsupportedBackend
	}
}
