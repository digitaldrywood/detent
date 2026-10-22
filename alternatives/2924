package staleness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrWarningNotActive = errors.New("staleness warning is not active")

type WarningAcknowledgementResult struct {
	WarningIDs        []string
	SnapshotPublished bool
}

type warningIdentity struct {
	projectID string
	warningID string
}

type Acknowledgements struct {
	mu           sync.Mutex
	store        store.StalenessWarningStore
	hub          *hub.Hub[telemetry.Snapshot]
	acknowledged map[warningIdentity]struct{}
	active       map[warningIdentity]struct{}
	latestRaw    telemetry.Snapshot
	hasLatestRaw bool
}

func NewAcknowledgements(
	ctx context.Context,
	stateStore store.StalenessWarningStore,
	snapshotHub *hub.Hub[telemetry.Snapshot],
	projectIDs []string,
) (*Acknowledgements, error) {
	if stateStore == nil {
		return nil, errors.New("staleness acknowledgements require store")
	}
	if snapshotHub == nil {
		return nil, errors.New("staleness acknowledgements require snapshot hub")
	}
	acknowledgements := &Acknowledgements{
		store:        stateStore,
		hub:          snapshotHub,
		acknowledged: make(map[warningIdentity]struct{}),
		active:       make(map[warningIdentity]struct{}),
	}
	seenProjects := make(map[string]struct{}, len(projectIDs))
	for _, projectID := range projectIDs {
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			continue
		}
		if _, exists := seenProjects[projectID]; exists {
			continue
		}
		seenProjects[projectID] = struct{}{}
		states, err := stateStore.ListStalenessWarningStates(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("load staleness warning acknowledgements for project %q: %w", projectID, err)
		}
		acknowledgements.replaceProjectAcknowledgements(projectID, states)
	}
	if latest, ok := snapshotHub.Latest(); ok {
		acknowledgements.latestRaw = cloneSnapshotWarnings(latest)
		acknowledgements.hasLatestRaw = true
		acknowledgements.active = activeWarningIdentities(latest)
	}
	return acknowledgements, nil
}

func (a *Acknowledgements) Publish(snapshot telemetry.Snapshot) error {
	if a == nil {
		return errors.New("staleness acknowledgements are unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.latestRaw = cloneSnapshotWarnings(snapshot)
	a.hasLatestRaw = true
	a.active = activeWarningIdentities(snapshot)
	return a.hub.Publish(a.effectiveSnapshot(snapshot))
}

func (a *Acknowledgements) Latest() (telemetry.Snapshot, bool) {
	if a == nil || a.hub == nil {
		return telemetry.Snapshot{}, false
	}
	return a.hub.Latest()
}

func (a *Acknowledgements) AcknowledgeActive(
	ctx context.Context,
	projectID string,
	warningIDs []string,
	at time.Time,
) (WarningAcknowledgementResult, error) {
	if a == nil {
		return WarningAcknowledgementResult{}, errors.New("staleness acknowledgements are unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	warningIDs, err := normalizeWarningIDs(warningIDs)
	if err != nil {
		return WarningAcknowledgementResult{}, err
	}
	if projectID == "" {
		return WarningAcknowledgementResult{}, store.ErrProjectRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	for _, warningID := range warningIDs {
		identity := warningIdentity{projectID: projectID, warningID: warningID}
		_, active := a.active[identity]
		_, alreadyAcknowledged := a.acknowledged[identity]
		if !active && !alreadyAcknowledged {
			return WarningAcknowledgementResult{}, fmt.Errorf("%w: project %q warning %q", ErrWarningNotActive, projectID, warningID)
		}
	}
	return a.acknowledgeLocked(ctx, projectID, warningIDs, at)
}

func (a *Acknowledgements) ListStalenessWarningStates(ctx context.Context, projectID string) ([]store.StalenessWarningState, error) {
	return a.store.ListStalenessWarningStates(ctx, projectID)
}

func (a *Acknowledgements) RecordStalenessWarningReminder(ctx context.Context, projectID string, warningID string, at time.Time) error {
	return a.store.RecordStalenessWarningReminder(ctx, projectID, warningID, at)
}

func (a *Acknowledgements) AcknowledgeStalenessWarning(ctx context.Context, projectID string, warningID string, at time.Time) error {
	return a.AcknowledgeStalenessWarnings(ctx, projectID, []string{warningID}, at)
}

func (a *Acknowledgements) AcknowledgeStalenessWarnings(ctx context.Context, projectID string, warningIDs []string, at time.Time) error {
	if a == nil {
		return errors.New("staleness acknowledgements are unavailable")
	}
	projectID = strings.TrimSpace(projectID)
	warningIDs, err := normalizeWarningIDs(warningIDs)
	if err != nil {
		return err
	}
	if projectID == "" {
		return store.ErrProjectRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.acknowledgeLocked(ctx, projectID, warningIDs, at)
	return err
}

func (a *Acknowledgements) ReconcileStalenessWarningStates(
	ctx context.Context,
	projectID string,
	activeWarningIDs []string,
	observedAt time.Time,
	inactiveBefore time.Time,
) ([]store.StalenessWarningState, error) {
	if a == nil {
		return nil, errors.New("staleness acknowledgements are unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	states, err := a.store.ReconcileStalenessWarningStates(ctx, projectID, activeWarningIDs, observedAt, inactiveBefore)
	if err != nil {
		return nil, err
	}
	a.replaceProjectAcknowledgements(projectID, states)
	return states, nil
}

func (a *Acknowledgements) acknowledgeLocked(
	ctx context.Context,
	projectID string,
	warningIDs []string,
	at time.Time,
) (WarningAcknowledgementResult, error) {
	if err := a.store.AcknowledgeStalenessWarnings(ctx, projectID, warningIDs, at); err != nil {
		return WarningAcknowledgementResult{}, err
	}
	for _, warningID := range warningIDs {
		a.acknowledged[warningIdentity{projectID: projectID, warningID: warningID}] = struct{}{}
	}
	result := WarningAcknowledgementResult{WarningIDs: append([]string(nil), warningIDs...)}
	if !a.hasLatestRaw {
		return result, nil
	}
	if err := a.hub.Publish(a.effectiveSnapshot(a.latestRaw)); err != nil {
		return result, fmt.Errorf("publish acknowledged staleness warnings: %w", err)
	}
	result.SnapshotPublished = true
	return result, nil
}

func (a *Acknowledgements) replaceProjectAcknowledgements(projectID string, states []store.StalenessWarningState) {
	projectID = strings.TrimSpace(projectID)
	for identity := range a.acknowledged {
		if identity.projectID == projectID {
			delete(a.acknowledged, identity)
		}
	}
	for _, state := range states {
		warningID := strings.TrimSpace(state.WarningID)
		if state.AcknowledgedAt == nil || warningID == "" {
			continue
		}
		a.acknowledged[warningIdentity{projectID: projectID, warningID: warningID}] = struct{}{}
	}
}

func (a *Acknowledgements) effectiveSnapshot(snapshot telemetry.Snapshot) telemetry.Snapshot {
	filtered := make([]telemetry.StalenessWarning, 0, len(snapshot.StalenessWarnings))
	for _, warning := range snapshot.StalenessWarnings {
		identity := warningIdentity{
			projectID: strings.TrimSpace(warning.ProjectID),
			warningID: strings.TrimSpace(warning.ID),
		}
		if _, acknowledged := a.acknowledged[identity]; acknowledged {
			continue
		}
		filtered = append(filtered, warning)
	}
	snapshot.StalenessWarnings = filtered
	return snapshot
}

func activeWarningIdentities(snapshot telemetry.Snapshot) map[warningIdentity]struct{} {
	active := make(map[warningIdentity]struct{}, len(snapshot.StalenessWarnings))
	for _, warning := range snapshot.StalenessWarnings {
		identity := warningIdentity{
			projectID: strings.TrimSpace(warning.ProjectID),
			warningID: strings.TrimSpace(warning.ID),
		}
		if identity.projectID == "" || identity.warningID == "" {
			continue
		}
		active[identity] = struct{}{}
	}
	return active
}

func cloneSnapshotWarnings(snapshot telemetry.Snapshot) telemetry.Snapshot {
	snapshot.StalenessWarnings = append([]telemetry.StalenessWarning(nil), snapshot.StalenessWarnings...)
	return snapshot
}

func normalizeWarningIDs(warningIDs []string) ([]string, error) {
	if len(warningIDs) == 0 {
		return nil, errors.New("warning_ids are required")
	}
	seen := make(map[string]struct{}, len(warningIDs))
	normalized := make([]string, 0, len(warningIDs))
	for _, warningID := range warningIDs {
		warningID = strings.TrimSpace(warningID)
		if warningID == "" {
			return nil, errors.New("warning_id is required")
		}
		if _, exists := seen[warningID]; exists {
			continue
		}
		seen[warningID] = struct{}{}
		normalized = append(normalized, warningID)
	}
	return normalized, nil
}
