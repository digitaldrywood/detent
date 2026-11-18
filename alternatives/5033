package staleness_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAcknowledgementsProjectsStoredAndLiveState(t *testing.T) {
	t.Parallel()
	backend := &warningStateStore{}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := backend.AcknowledgeStalenessWarning(t.Context(), "detent", "warning-1", now.Add(-time.Hour)); err != nil {
		t.Fatalf("AcknowledgeStalenessWarning() error = %v", err)
	}
	snapshots := hub.New[telemetry.Snapshot]()
	acknowledgements, err := staleness.NewAcknowledgements(t.Context(), backend, snapshots, []string{"detent", "other"})
	if err != nil {
		t.Fatalf("NewAcknowledgements() error = %v", err)
	}
	raw := telemetry.Snapshot{StalenessWarnings: []telemetry.StalenessWarning{
		{ID: "warning-1", ProjectID: "detent"},
		{ID: "warning-2", ProjectID: "detent"},
		{ID: "warning-1", ProjectID: "other"},
	}}
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	assertWarningIdentities(t, snapshots, []string{"detent/warning-2", "other/warning-1"})

	result, err := acknowledgements.AcknowledgeActive(t.Context(), "detent", []string{"warning-2"}, now)
	if err != nil {
		t.Fatalf("AcknowledgeActive() error = %v", err)
	}
	if !result.SnapshotPublished {
		t.Fatal("AcknowledgeActive() did not publish the effective snapshot")
	}
	assertWarningIdentities(t, snapshots, []string{"other/warning-1"})
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish(unchanged raw snapshot) error = %v", err)
	}
	assertWarningIdentities(t, snapshots, []string{"other/warning-1"})

	for _, tt := range []struct {
		name      string
		projectID string
		warningID string
	}{
		{name: "unknown warning", projectID: "detent", warningID: "unknown"},
		{name: "wrong project", projectID: "other", warningID: "warning-2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := acknowledgements.AcknowledgeActive(t.Context(), tt.projectID, []string{tt.warningID}, now)
			if !errors.Is(err, staleness.ErrWarningNotActive) {
				t.Fatalf("AcknowledgeActive() error = %v, want ErrWarningNotActive", err)
			}
		})
	}
}

func TestAcknowledgementsFollowReconciledStoreState(t *testing.T) {
	t.Parallel()
	backend := &warningStateStore{}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := backend.AcknowledgeStalenessWarning(t.Context(), "detent", "warning-1", now.Add(-31*24*time.Hour)); err != nil {
		t.Fatalf("AcknowledgeStalenessWarning() error = %v", err)
	}
	snapshots := hub.New[telemetry.Snapshot]()
	acknowledgements, err := staleness.NewAcknowledgements(t.Context(), backend, snapshots, []string{"detent"})
	if err != nil {
		t.Fatalf("NewAcknowledgements() error = %v", err)
	}
	raw := telemetry.Snapshot{StalenessWarnings: []telemetry.StalenessWarning{{ID: "warning-1", ProjectID: "detent"}}}
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish(before reconciliation) error = %v", err)
	}
	assertWarningIdentities(t, snapshots, nil)
	if _, err := acknowledgements.ReconcileStalenessWarningStates(
		t.Context(),
		"detent",
		nil,
		now,
		now.Add(-30*24*time.Hour),
	); err != nil {
		t.Fatalf("ReconcileStalenessWarningStates(absent) error = %v", err)
	}
	if _, err := acknowledgements.ReconcileStalenessWarningStates(
		t.Context(),
		"detent",
		[]string{"warning-1"},
		now.Add(time.Minute),
		now.Add(-30*24*time.Hour+time.Minute),
	); err != nil {
		t.Fatalf("ReconcileStalenessWarningStates(recurred) error = %v", err)
	}
	if err := acknowledgements.Publish(raw); err != nil {
		t.Fatalf("Publish(after reconciliation) error = %v", err)
	}
	assertWarningIdentities(t, snapshots, []string{"detent/warning-1"})
}

func assertWarningIdentities(t *testing.T, snapshots *hub.Hub[telemetry.Snapshot], want []string) {
	t.Helper()
	snapshot, ok := snapshots.Latest()
	if !ok {
		t.Fatal("snapshot hub has no latest snapshot")
	}
	got := make([]string, 0, len(snapshot.StalenessWarnings))
	for _, warning := range snapshot.StalenessWarnings {
		got = append(got, warning.ProjectID+"/"+warning.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("warning identities = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("warning identities = %v, want %v", got, want)
		}
	}
}

type warningStateStore struct {
	mu     sync.Mutex
	states []store.StalenessWarningState
}

func (s *warningStateStore) ListStalenessWarningStates(_ context.Context, projectID string) ([]store.StalenessWarningState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projectStates(projectID), nil
}

func (s *warningStateStore) RecordStalenessWarningReminder(context.Context, string, string, time.Time) error {
	return nil
}

func (s *warningStateStore) AcknowledgeStalenessWarning(ctx context.Context, projectID string, warningID string, at time.Time) error {
	return s.AcknowledgeStalenessWarnings(ctx, projectID, []string{warningID}, at)
}

func (s *warningStateStore) AcknowledgeStalenessWarnings(_ context.Context, projectID string, warningIDs []string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, warningID := range warningIDs {
		index := slices.IndexFunc(s.states, func(state store.StalenessWarningState) bool {
			return state.ProjectID == projectID && state.WarningID == warningID
		})
		if index < 0 {
			s.states = append(s.states, store.StalenessWarningState{ProjectID: projectID, WarningID: warningID, AcknowledgedAt: &at})
			continue
		}
		s.states[index].AcknowledgedAt = &at
	}
	return nil
}

func (s *warningStateStore) ReconcileStalenessWarningStates(_ context.Context, projectID string, activeWarningIDs []string, _ time.Time, inactiveBefore time.Time) ([]store.StalenessWarningState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = slices.DeleteFunc(s.states, func(state store.StalenessWarningState) bool {
		return state.ProjectID == projectID && !slices.Contains(activeWarningIDs, state.WarningID) &&
			state.AcknowledgedAt != nil && state.AcknowledgedAt.Before(inactiveBefore)
	})
	return s.projectStates(projectID), nil
}

func (s *warningStateStore) projectStates(projectID string) []store.StalenessWarningState {
	states := []store.StalenessWarningState{}
	for _, state := range s.states {
		if state.ProjectID == projectID {
			states = append(states, state)
		}
	}
	return states
}
