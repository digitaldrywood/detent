package scheduler_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/scheduler"
	"github.com/digitaldrywood/symphony-go/internal/store"
)

func TestWeightedFairSelectProjectConvergesToWeights(t *testing.T) {
	t.Parallel()

	global := newGlobalScheduler(t, scheduler.Config{Kind: "weighted_fair", Capacity: 1})
	request := scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "alpha", Weight: 3},
			{ID: "beta", Weight: 1},
		},
	}

	counts := map[string]int{}
	for range 8 {
		selection, err := global.SelectProject(context.Background(), request)
		if err != nil {
			t.Fatalf("SelectProject() error = %v", err)
		}
		counts[selection.Project.ID]++
	}

	want := map[string]int{"alpha": 6, "beta": 2}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("selection counts = %#v, want %#v", counts, want)
	}
}

func TestWeightedFairAppliesDecay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	global := newGlobalScheduler(t, scheduler.Config{
		Kind:          "weighted_fair",
		Capacity:      1,
		DecayHalfLife: time.Second,
	})
	request := scheduler.ProjectSelectionRequest{
		Now: now,
		Projects: []scheduler.ProjectCandidate{
			{ID: "alpha", Weight: 1},
			{ID: "beta", Weight: 1},
		},
	}

	first, err := global.SelectProject(context.Background(), request)
	if err != nil {
		t.Fatalf("SelectProject() first error = %v", err)
	}
	if first.Project.ID != "alpha" {
		t.Fatalf("first project = %q, want alpha", first.Project.ID)
	}

	request.Now = now.Add(10 * time.Second)
	second, err := global.SelectProject(context.Background(), request)
	if err != nil {
		t.Fatalf("SelectProject() second error = %v", err)
	}
	if second.Project.ID != "alpha" {
		t.Fatalf("second project = %q, want alpha after decay", second.Project.ID)
	}
}

func TestStrictPriorityReportsPreemption(t *testing.T) {
	t.Parallel()

	global := newGlobalScheduler(t, scheduler.Config{Kind: "strict", Capacity: 1})
	request := scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "urgent", Priority: 1},
		},
		Running: []scheduler.RunningProject{
			{ProjectID: "background", Priority: 4},
		},
	}

	selection, err := global.SelectProject(context.Background(), request)
	if err != nil {
		t.Fatalf("SelectProject() error = %v", err)
	}
	if selection.Project.ID != "urgent" {
		t.Fatalf("selected project = %q, want urgent", selection.Project.ID)
	}
	if len(selection.Preemptions) != 1 || selection.Preemptions[0].ProjectID != "background" {
		t.Fatalf("preemptions = %#v, want background", selection.Preemptions)
	}
}

func TestStrictPriorityRejectsCandidateThatCannotPreempt(t *testing.T) {
	t.Parallel()

	global := newGlobalScheduler(t, scheduler.Config{Kind: "strict", Capacity: 1})
	_, err := global.SelectProject(context.Background(), scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "low", Priority: 4},
		},
		Running: []scheduler.RunningProject{
			{ProjectID: "urgent", Priority: 1},
		},
	})
	if !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("SelectProject() error = %v, want ErrNoSlots", err)
	}
}

func TestRoundRobinRotatesProjects(t *testing.T) {
	t.Parallel()

	global := newGlobalScheduler(t, scheduler.Config{Kind: "round_robin", Capacity: 1})
	request := scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "alpha"},
			{ID: "beta"},
			{ID: "gamma"},
		},
	}

	var got []string
	for range 5 {
		selection, err := global.SelectProject(context.Background(), request)
		if err != nil {
			t.Fatalf("SelectProject() error = %v", err)
		}
		got = append(got, selection.Project.ID)
	}

	want := []string{"alpha", "beta", "gamma", "alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected projects = %#v, want %#v", got, want)
	}
}

func TestFairShareUsesPersistedUsage(t *testing.T) {
	t.Parallel()

	store := &fairShareStore{
		usage: []store.FairShareUsage{
			{ProjectID: "alpha", Dispatches: 10, RuntimeSeconds: 100},
			{ProjectID: "beta", Dispatches: 1, RuntimeSeconds: 10},
		},
	}
	global := newGlobalScheduler(t, scheduler.Config{
		Kind:           "fair_share",
		Capacity:       1,
		FairShareStore: store,
	})

	selection, err := global.SelectProject(context.Background(), scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "alpha", Weight: 1},
			{ID: "beta", Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("SelectProject() error = %v", err)
	}
	if selection.Project.ID != "beta" {
		t.Fatalf("selected project = %q, want beta", selection.Project.ID)
	}

	if err := global.RecordProjectDispatch(context.Background(), scheduler.ProjectDispatch{
		ProjectID:      selection.Project.ID,
		Weight:         2,
		RuntimeSeconds: 30,
		DispatchedAt:   time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordProjectDispatch() error = %v", err)
	}
	if len(store.records) != 1 || store.records[0].ProjectID != "beta" {
		t.Fatalf("recorded dispatches = %#v, want beta", store.records)
	}
}

func TestFairShareRequiresStore(t *testing.T) {
	t.Parallel()

	global := scheduler.NewFairShare(scheduler.Config{Capacity: 1})
	_, err := global.SelectProject(context.Background(), scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "alpha"},
			{ID: "beta"},
		},
	})
	if !errors.Is(err, scheduler.ErrFairShareStoreRequired) {
		t.Fatalf("SelectProject() error = %v, want ErrFairShareStoreRequired", err)
	}

	err = global.RecordProjectDispatch(context.Background(), scheduler.ProjectDispatch{
		ProjectID: "alpha",
	})
	if !errors.Is(err, scheduler.ErrFairShareStoreRequired) {
		t.Fatalf("RecordProjectDispatch() error = %v, want ErrFairShareStoreRequired", err)
	}
}

func TestSelectProjectRejectsEmptyCandidateSet(t *testing.T) {
	t.Parallel()

	global := newGlobalScheduler(t, scheduler.Config{Kind: "weighted_fair", Capacity: 1})
	_, err := global.SelectProject(context.Background(), scheduler.ProjectSelectionRequest{
		Projects: []scheduler.ProjectCandidate{
			{ID: "paused", Paused: true},
			{ID: " "},
		},
	})
	if !errors.Is(err, scheduler.ErrNoCandidates) {
		t.Fatalf("SelectProject() error = %v, want ErrNoCandidates", err)
	}
}

func newGlobalScheduler(t *testing.T, cfg scheduler.Config) scheduler.GlobalScheduler {
	t.Helper()

	sched, err := scheduler.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig() error = %v", err)
	}
	global, ok := sched.(scheduler.GlobalScheduler)
	if !ok {
		t.Fatalf("NewFromConfig() returned %T, want GlobalScheduler", sched)
	}
	return global
}

type fairShareStore struct {
	usage   []store.FairShareUsage
	records []store.FairShareDispatch
}

func (s *fairShareStore) ListFairShareUsage(context.Context) ([]store.FairShareUsage, error) {
	return append([]store.FairShareUsage(nil), s.usage...), nil
}

func (s *fairShareStore) RecordFairShareDispatch(_ context.Context, record store.FairShareDispatch) error {
	s.records = append(s.records, record)
	return nil
}
