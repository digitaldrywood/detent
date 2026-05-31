package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/store"
)

var ErrNoCandidates = errors.New("scheduler has no project candidates")

type FairShareStore interface {
	ListFairShareUsage(context.Context) ([]store.FairShareUsage, error)
	RecordFairShareDispatch(context.Context, store.FairShareDispatch) error
}

type GlobalScheduler interface {
	Scheduler
	SelectProject(context.Context, ProjectSelectionRequest) (ProjectSelection, error)
	RecordProjectDispatch(context.Context, ProjectDispatch) error
}

type ProjectCandidate struct {
	ID       string
	Weight   int
	Priority int
	Paused   bool
}

type RunningProject struct {
	ProjectID string
	Priority  int
}

type ProjectSelectionRequest struct {
	Projects []ProjectCandidate
	Running  []RunningProject
	Now      time.Time
}

type ProjectSelection struct {
	Project     ProjectCandidate
	Preemptions []RunningProject
}

type ProjectDispatch struct {
	ProjectID      string
	Weight         int
	RuntimeSeconds int64
	DispatchedAt   time.Time
}

type globalScheduler struct {
	*CountingSemaphore

	mode           Mode
	decayHalfLife  time.Duration
	fairShareStore FairShareStore

	mu                 sync.Mutex
	weightedCurrent    map[string]float64
	weightedLastUpdate time.Time
	roundRobinLastID   string
}

var _ GlobalScheduler = (*globalScheduler)(nil)

func NewWeightedFair(cfg Config) GlobalScheduler {
	return newGlobalScheduler(ModeWeightedFair, cfg)
}

func NewStrictPriority(cfg Config) GlobalScheduler {
	return newGlobalScheduler(ModeStrictPriority, cfg)
}

func NewRoundRobin(cfg Config) GlobalScheduler {
	return newGlobalScheduler(ModeRoundRobin, cfg)
}

func NewFairShare(cfg Config) GlobalScheduler {
	return newGlobalScheduler(ModeFairShare, cfg)
}

func newGlobalScheduler(mode Mode, cfg Config) *globalScheduler {
	return &globalScheduler{
		CountingSemaphore: NewCountingSemaphore(cfg),
		mode:              mode,
		decayHalfLife:     cfg.DecayHalfLife,
		fairShareStore:    cfg.FairShareStore,
		weightedCurrent:   map[string]float64{},
	}
}

func (s *globalScheduler) Mode() Mode {
	return s.mode
}

func (s *globalScheduler) SelectProject(ctx context.Context, req ProjectSelectionRequest) (ProjectSelection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ProjectSelection{}, ctx.Err()
	default:
	}

	candidates := normalizeProjectCandidates(req.Projects)
	if len(candidates) == 0 {
		return ProjectSelection{}, ErrNoCandidates
	}

	switch s.mode {
	case ModeStrictPriority:
		return s.selectStrictPriority(candidates, req.Running)
	case ModeRoundRobin:
		if !s.projectSlotAvailable(req.Running) {
			return ProjectSelection{}, ErrNoSlots
		}
		return s.selectRoundRobin(candidates), nil
	case ModeFairShare:
		if !s.projectSlotAvailable(req.Running) {
			return ProjectSelection{}, ErrNoSlots
		}
		return s.selectFairShare(ctx, candidates)
	default:
		if !s.projectSlotAvailable(req.Running) {
			return ProjectSelection{}, ErrNoSlots
		}
		return s.selectWeightedFair(candidates, req.Now), nil
	}
}

func (s *globalScheduler) RecordProjectDispatch(ctx context.Context, dispatch ProjectDispatch) error {
	if s.mode != ModeFairShare || s.fairShareStore == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	projectID := normalizeProjectID(dispatch.ProjectID)
	if projectID == "" {
		return fmt.Errorf("%w: project id is required", ErrNoCandidates)
	}

	dispatchedAt := dispatch.DispatchedAt
	if dispatchedAt.IsZero() {
		dispatchedAt = time.Now()
	}

	return s.fairShareStore.RecordFairShareDispatch(ctx, store.FairShareDispatch{
		ProjectID:      projectID,
		Weight:         normalizeProjectWeight(dispatch.Weight),
		RuntimeSeconds: nonNegative(dispatch.RuntimeSeconds),
		DispatchedAt:   dispatchedAt,
	})
}

func (s *globalScheduler) projectSlotAvailable(running []RunningProject) bool {
	return len(running) < s.capacity
}

func (s *globalScheduler) selectWeightedFair(candidates []ProjectCandidate, now time.Time) ProjectSelection {
	s.mu.Lock()
	defer s.mu.Unlock()

	if now.IsZero() {
		now = time.Now()
	}
	s.applyWeightedDecayLocked(now)

	totalWeight := 0
	for _, candidate := range candidates {
		totalWeight += candidate.Weight
		s.weightedCurrent[candidate.ID] += float64(candidate.Weight)
	}

	selected := candidates[0]
	best := s.weightedCurrent[selected.ID]
	for _, candidate := range candidates[1:] {
		if current := s.weightedCurrent[candidate.ID]; current > best {
			selected = candidate
			best = current
		}
	}
	s.weightedCurrent[selected.ID] -= float64(totalWeight)
	s.weightedLastUpdate = now

	return ProjectSelection{Project: selected}
}

func (s *globalScheduler) applyWeightedDecayLocked(now time.Time) {
	if s.decayHalfLife <= 0 || s.weightedLastUpdate.IsZero() || !now.After(s.weightedLastUpdate) {
		return
	}

	factor := math.Pow(0.5, now.Sub(s.weightedLastUpdate).Seconds()/s.decayHalfLife.Seconds())
	if factor < 0.01 {
		clear(s.weightedCurrent)
		return
	}

	for projectID, current := range s.weightedCurrent {
		s.weightedCurrent[projectID] = current * factor
	}
}

func (s *globalScheduler) selectStrictPriority(candidates []ProjectCandidate, running []RunningProject) (ProjectSelection, error) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left := priorityRank(candidates[i].Priority)
		right := priorityRank(candidates[j].Priority)
		if left != right {
			return left < right
		}
		return candidates[i].ID < candidates[j].ID
	})

	selected := candidates[0]
	if s.projectSlotAvailable(running) {
		return ProjectSelection{Project: selected}, nil
	}

	preempt, ok := preemptableRunningProject(selected.Priority, running)
	if !ok {
		return ProjectSelection{}, ErrNoSlots
	}

	return ProjectSelection{
		Project:     selected,
		Preemptions: []RunningProject{preempt},
	}, nil
}

func preemptableRunningProject(priority int, running []RunningProject) (RunningProject, bool) {
	selectedRank := priorityRank(priority)
	var preempt RunningProject
	found := false
	worstRank := 0
	for _, candidate := range running {
		rank := priorityRank(candidate.Priority)
		if rank <= selectedRank {
			continue
		}
		if !found || rank > worstRank {
			preempt = candidate
			worstRank = rank
			found = true
		}
	}
	return preempt, found
}

func (s *globalScheduler) selectRoundRobin(candidates []ProjectCandidate) ProjectSelection {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := 0
	if s.roundRobinLastID != "" {
		for i, candidate := range candidates {
			if candidate.ID == s.roundRobinLastID {
				index = (i + 1) % len(candidates)
				break
			}
		}
	}

	selected := candidates[index]
	s.roundRobinLastID = selected.ID
	return ProjectSelection{Project: selected}
}

func (s *globalScheduler) selectFairShare(ctx context.Context, candidates []ProjectCandidate) (ProjectSelection, error) {
	usageByProject := map[string]store.FairShareUsage{}
	if s.fairShareStore != nil {
		usage, err := s.fairShareStore.ListFairShareUsage(ctx)
		if err != nil {
			return ProjectSelection{}, fmt.Errorf("read fair-share usage: %w", err)
		}
		for _, item := range usage {
			projectID := normalizeProjectID(item.ProjectID)
			if projectID == "" {
				continue
			}
			usageByProject[projectID] = item
		}
	}

	selected := candidates[0]
	best := fairShareScore(selected, usageByProject[selected.ID])
	for _, candidate := range candidates[1:] {
		if score := fairShareScore(candidate, usageByProject[candidate.ID]); score < best {
			selected = candidate
			best = score
		}
	}

	return ProjectSelection{Project: selected}, nil
}

func fairShareScore(candidate ProjectCandidate, usage store.FairShareUsage) float64 {
	cost := float64(nonNegative(usage.Dispatches)) + float64(nonNegative(usage.RuntimeSeconds))/60
	return cost / float64(candidate.Weight)
}

func priorityRank(priority int) int {
	if priority < 1 || priority > 4 {
		return 5
	}
	return priority
}

func normalizeProjectCandidates(projects []ProjectCandidate) []ProjectCandidate {
	candidates := make([]ProjectCandidate, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if project.Paused {
			continue
		}
		project.ID = normalizeProjectID(project.ID)
		if project.ID == "" {
			continue
		}
		if _, ok := seen[project.ID]; ok {
			continue
		}
		seen[project.ID] = struct{}{}
		project.Weight = normalizeProjectWeight(project.Weight)
		candidates = append(candidates, project)
	}
	return candidates
}

func normalizeProjectID(projectID string) string {
	return strings.TrimSpace(projectID)
}

func normalizeProjectWeight(weight int) int {
	if weight <= 0 {
		return 1
	}
	return weight
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
