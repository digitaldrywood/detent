package admission

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

// orderCandidateWindow reuses persisted run evidence so unchanged stale
// snapshots cannot consume the next window, including after a restart.
func (m *Manager) orderCandidateWindow(ctx context.Context, settings Settings, candidates []connector.Issue, skipped map[string]int, at time.Time) ([]connector.Issue, error) {
	history, err := m.store.AdmissionCandidateHistory(ctx, settings.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("read backlog admission candidate history: %w", err)
	}
	lastEvaluated := make(map[string]time.Time, len(candidates))
	out := make([]connector.Issue, 0, len(candidates))
	for _, candidate := range candidates {
		if len(admissionDependencyReferences(candidate)) > 0 {
			settings.dependencies[candidate.ID] = resolveAdmissionDependencies(ctx, settings, candidate, at)
		}
		previous := history[candidate.ID]
		if previous.Fingerprint == admissionEvaluationFingerprints(settings, candidate).proposal {
			if previous.SkipReason == "stale_or_ineligible" || previous.SkipReason == "tracking_epic" {
				skipped[previous.SkipReason]++
				continue
			}
			lastEvaluated[candidate.ID] = previous.EvaluatedAt
		}
		out = append(out, candidate)
	}
	// Existing dispatch ordering breaks ties within the same evaluation time.
	// Unseen or changed candidates sort first, followed by least recently evaluated.
	sortCandidates(out, settings)
	sort.SliceStable(out, func(i, j int) bool {
		return lastEvaluated[out[i].ID].Before(lastEvaluated[out[j].ID])
	})
	return out, nil
}
