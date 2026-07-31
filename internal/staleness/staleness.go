package staleness

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

const (
	KindLaneAging          = "lane_aging"
	KindProjectLiveness    = "project_liveness"
	KindMergeLiveness      = "merge_liveness"
	KindRepeatedDecision   = "repeated_decision"
	defaultWarningIDLength = 16
)

type Config struct {
	Enabled                       bool
	Lanes                         []LaneThreshold
	NoCompletionThreshold         time.Duration
	NoMergeThreshold              time.Duration
	RepeatedDecisionCount         int
	RepeatedDecisionWindow        time.Duration
	RepeatedDecisionBenignReasons []string
	TerminalStates                []string
}

type LaneThreshold struct {
	State     string
	Threshold time.Duration
	HumanGate bool
}

type Input struct {
	ProjectID    string
	Items        []Item
	Dispatchable []Item
	MergeQueue   []Item
	Completions  []Completion
	Decisions    []Decision
}

type Item struct {
	ID                   string
	Identifier           string
	URL                  string
	Title                string
	State                string
	EnteredAt            time.Time
	WaitingOnHuman       bool
	HasRecoveryPredicate bool
}

type Completion struct {
	At     time.Time
	Merged bool
}

type Decision struct {
	IssueID      string
	Identifier   string
	IssueURL     string
	CurrentState string
	Closed       bool
	Merged       bool
	Result       string
	Reason       string
	At           time.Time
}

type Warning struct {
	ID                   string    `json:"id"`
	ProjectID            string    `json:"project_id,omitempty"`
	Kind                 string    `json:"kind"`
	IssueID              string    `json:"issue_id,omitempty"`
	Identifier           string    `json:"identifier,omitempty"`
	IssueURL             string    `json:"issue_url,omitempty"`
	Title                string    `json:"title,omitempty"`
	Lane                 string    `json:"lane,omitempty"`
	Reason               string    `json:"reason"`
	Detail               string    `json:"detail"`
	Since                time.Time `json:"since"`
	AgeSeconds           int64     `json:"age_seconds"`
	ThresholdSeconds     int64     `json:"threshold_seconds"`
	Count                int       `json:"count,omitempty"`
	WaitingOnHuman       bool      `json:"waiting_on_human,omitempty"`
	HasRecoveryPredicate bool      `json:"has_recovery_predicate,omitempty"`
}

func Evaluate(cfg Config, input Input, now time.Time) []Warning {
	if !cfg.Enabled || now.IsZero() {
		return nil
	}
	now = now.UTC()
	warnings := laneWarnings(cfg, input, now)
	if warning, ok := projectLivenessWarning(cfg, input, now); ok {
		warnings = append(warnings, warning)
	}
	if warning, ok := mergeLivenessWarning(cfg, input, now); ok {
		warnings = append(warnings, warning)
	}
	warnings = append(warnings, repeatedDecisionWarnings(cfg, input, now)...)
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].Since.Equal(warnings[j].Since) {
			return warnings[i].ID < warnings[j].ID
		}
		return warnings[i].Since.Before(warnings[j].Since)
	})
	return warnings
}

func laneWarnings(cfg Config, input Input, now time.Time) []Warning {
	thresholds := make(map[string]LaneThreshold, len(cfg.Lanes))
	for _, threshold := range cfg.Lanes {
		state := normalize(threshold.State)
		if state == "" || threshold.Threshold <= 0 {
			continue
		}
		thresholds[state] = threshold
	}
	warnings := make([]Warning, 0)
	seen := make(map[string]struct{})
	for _, item := range input.Items {
		threshold, ok := thresholds[normalize(item.State)]
		if !ok || item.EnteredAt.IsZero() || item.EnteredAt.After(now) {
			continue
		}
		age := now.Sub(item.EnteredAt)
		if age < threshold.Threshold {
			continue
		}
		identity := itemIdentity(item)
		if identity == "" {
			continue
		}
		key := KindLaneAging + "\x00" + identity + "\x00" + normalize(item.State)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		waitingOnHuman := threshold.HumanGate || item.WaitingOnHuman
		warnings = append(warnings, Warning{
			ID:                   warningID(key),
			ProjectID:            strings.TrimSpace(input.ProjectID),
			Kind:                 KindLaneAging,
			IssueID:              strings.TrimSpace(item.ID),
			Identifier:           strings.TrimSpace(item.Identifier),
			IssueURL:             strings.TrimSpace(item.URL),
			Title:                strings.TrimSpace(item.Title),
			Lane:                 strings.TrimSpace(item.State),
			Reason:               "lane threshold exceeded",
			Detail:               laneWarningDetail(item, waitingOnHuman),
			Since:                item.EnteredAt.UTC(),
			AgeSeconds:           int64(age / time.Second),
			ThresholdSeconds:     int64(threshold.Threshold / time.Second),
			WaitingOnHuman:       waitingOnHuman,
			HasRecoveryPredicate: item.HasRecoveryPredicate,
		})
	}
	return warnings
}

func projectLivenessWarning(cfg Config, input Input, now time.Time) (Warning, bool) {
	if cfg.NoCompletionThreshold <= 0 || len(input.Dispatchable) == 0 {
		return Warning{}, false
	}
	baseline := latestCompletion(input.Completions, false, now)
	queueSince := earliestItemTime(input.Dispatchable, now)
	baseline = laterTime(baseline, queueSince)
	if baseline.IsZero() || now.Sub(baseline) < cfg.NoCompletionThreshold {
		return Warning{}, false
	}
	projectID := strings.TrimSpace(input.ProjectID)
	key := KindProjectLiveness + "\x00" + projectID
	return Warning{
		ID:               warningID(key),
		ProjectID:        projectID,
		Kind:             KindProjectLiveness,
		Reason:           "dispatchable work has no recent completion",
		Detail:           "dispatchable work is queued but the project has not completed work",
		Since:            baseline.UTC(),
		AgeSeconds:       int64(now.Sub(baseline) / time.Second),
		ThresholdSeconds: int64(cfg.NoCompletionThreshold / time.Second),
		Count:            len(input.Dispatchable),
	}, true
}

func mergeLivenessWarning(cfg Config, input Input, now time.Time) (Warning, bool) {
	if cfg.NoMergeThreshold <= 0 || len(input.MergeQueue) == 0 {
		return Warning{}, false
	}
	baseline := latestCompletion(input.Completions, true, now)
	queueSince := earliestItemTime(input.MergeQueue, now)
	baseline = laterTime(baseline, queueSince)
	if baseline.IsZero() || now.Sub(baseline) < cfg.NoMergeThreshold {
		return Warning{}, false
	}
	projectID := strings.TrimSpace(input.ProjectID)
	key := KindMergeLiveness + "\x00" + projectID
	return Warning{
		ID:               warningID(key),
		ProjectID:        projectID,
		Kind:             KindMergeLiveness,
		Lane:             "Merging",
		Reason:           "merge queue has no recent success",
		Detail:           "the merge queue is non-empty but the project has no recent successful merge",
		Since:            baseline.UTC(),
		AgeSeconds:       int64(now.Sub(baseline) / time.Second),
		ThresholdSeconds: int64(cfg.NoMergeThreshold / time.Second),
		Count:            len(input.MergeQueue),
	}, true
}

func repeatedDecisionWarnings(cfg Config, input Input, now time.Time) []Warning {
	if cfg.RepeatedDecisionCount <= 0 || cfg.RepeatedDecisionWindow <= 0 {
		return nil
	}
	type group struct {
		decision Decision
		count    int
		first    time.Time
		last     time.Time
	}
	cutoff := now.Add(-cfg.RepeatedDecisionWindow)
	benignReasons := normalizedSet(cfg.RepeatedDecisionBenignReasons)
	terminalStates := normalizedSet(cfg.TerminalStates)
	groups := make(map[string]group)
	for _, decision := range input.Decisions {
		identity := decisionIdentity(decision)
		result := normalize(decision.Result)
		reason := strings.TrimSpace(decision.Reason)
		currentState := normalize(decision.CurrentState)
		if identity == "" ||
			result != "skipped" ||
			reason == "" ||
			currentState == "" ||
			decision.Closed ||
			decision.Merged ||
			contains(terminalStates, currentState) ||
			contains(benignReasons, normalize(reason)) ||
			decision.At.IsZero() ||
			decision.At.Before(cutoff) ||
			decision.At.After(now) {
			continue
		}
		key := identity + "\x00" + reason
		current := groups[key]
		if current.count == 0 {
			current.decision = decision
			current.first = decision.At.UTC()
		}
		current.count++
		if current.last.IsZero() || decision.At.After(current.last) {
			current.last = decision.At.UTC()
		}
		if decision.At.Before(current.first) {
			current.first = decision.At.UTC()
		}
		groups[key] = current
	}
	warnings := make([]Warning, 0)
	for key, current := range groups {
		if current.count < cfg.RepeatedDecisionCount {
			continue
		}
		warnings = append(warnings, Warning{
			ID:               warningID(KindRepeatedDecision + "\x00" + key),
			ProjectID:        strings.TrimSpace(input.ProjectID),
			Kind:             KindRepeatedDecision,
			IssueID:          strings.TrimSpace(current.decision.IssueID),
			Identifier:       strings.TrimSpace(current.decision.Identifier),
			IssueURL:         strings.TrimSpace(current.decision.IssueURL),
			Reason:           strings.TrimSpace(current.decision.Reason),
			Detail:           "the same scheduler decision has recurred beyond its configured threshold",
			Since:            current.first,
			AgeSeconds:       int64(now.Sub(current.first) / time.Second),
			ThresholdSeconds: int64(cfg.RepeatedDecisionWindow / time.Second),
			Count:            current.count,
		})
	}
	return warnings
}

func normalizedSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = normalize(value); value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func contains(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}

func laneWarningDetail(item Item, waitingOnHuman bool) string {
	if waitingOnHuman {
		return "the item is waiting on a person by design and has exceeded its reminder threshold"
	}
	if normalize(item.State) == "blocked" && !item.HasRecoveryPredicate {
		return "the item is blocked beyond its threshold without a recovery predicate"
	}
	return "the item has remained in the same lane beyond its configured threshold"
}

func latestCompletion(completions []Completion, mergedOnly bool, now time.Time) time.Time {
	var latest time.Time
	for _, completion := range completions {
		if completion.At.IsZero() || completion.At.After(now) || mergedOnly && !completion.Merged {
			continue
		}
		if latest.IsZero() || completion.At.After(latest) {
			latest = completion.At.UTC()
		}
	}
	return latest
}

func earliestItemTime(items []Item, now time.Time) time.Time {
	var earliest time.Time
	for _, item := range items {
		if item.EnteredAt.IsZero() || item.EnteredAt.After(now) {
			continue
		}
		if earliest.IsZero() || item.EnteredAt.Before(earliest) {
			earliest = item.EnteredAt.UTC()
		}
	}
	return earliest
}

func laterTime(left time.Time, right time.Time) time.Time {
	if left.IsZero() || right.After(left) {
		return right
	}
	return left
}

func itemIdentity(item Item) string {
	for _, value := range []string{item.ID, item.Identifier, item.URL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func decisionIdentity(decision Decision) string {
	for _, value := range []string{decision.IssueID, decision.Identifier, decision.IssueURL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func warningID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:defaultWarningIDLength]
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
