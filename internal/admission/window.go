package admission

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
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
			stale := false
			if previous.SkipReason == "stale_or_ineligible" {
				// A saved verdict describes a past lookup. The candidate may have
				// returned to this eligible snapshot without changing its fingerprint.
				_, _, valid, err := revalidateAdmissionCandidate(ctx, settings, candidate, at)
				if err != nil {
					return nil, err
				}
				stale = !valid
			}
			if stale || previous.SkipReason == "tracking_epic" {
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

// revalidateAdmissionCandidate shares the snapshot check between saved stale
// verdicts and newly evaluated candidates. A transient eligibility change must
// not turn a historical skip into permanent exclusion.
func revalidateAdmissionCandidate(ctx context.Context, settings Settings, original connector.Issue, at time.Time) (connector.Issue, *runner.AdmissionDependencies, bool, error) {
	issueID := strings.TrimSpace(original.ID)
	fresh, err := settings.Issues.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		return connector.Issue{}, nil, false, fmt.Errorf("revalidate backlog admission candidate %s: %w", original.Identifier, err)
	}
	current, found := issueMap(fresh)[issueID]
	dependencies := resolveAdmissionDependencies(ctx, settings, current, at)
	valid := found && issueFingerprint(original, settings.dependencies[issueID]) == issueFingerprint(current, dependencies) &&
		eligibleCandidate(current, settings.Config, settings.TerminalStates)
	return current, dependencies, valid, nil
}
