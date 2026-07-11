package retro

import (
	"slices"
	"testing"
	"time"
)

func TestDetectReplaysEfficiencyIncidents(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{}
	snapshot.Attempts = append(snapshot.Attempts,
		Attempt{ID: 1, Identifier: "digitaldrywood/detent#1126", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "success", Phase: "waiting"},
		Attempt{ID: 2, Identifier: "digitaldrywood/detent#1126", StartedAt: base.Add(2 * time.Minute), CompletedAt: base.Add(3 * time.Minute), TerminalState: "success"},
	)
	for index := range 5 {
		id := int64(10 + index)
		startedAt := base.Add(time.Duration(10+index) * time.Minute)
		snapshot.Attempts = append(snapshot.Attempts, Attempt{
			ID:            id,
			Identifier:    "digitaldrywood/detent#200",
			StartedAt:     startedAt,
			CompletedAt:   startedAt.Add(time.Minute),
			TerminalState: "failure",
			ErrorClass:    "runner",
			ErrorMessage:  "session token ceiling exceeded: ceiling_tokens=2000000 source=max_session_context_multiplier",
		})
		snapshot.Sessions = append(snapshot.Sessions, Session{WorkAttemptID: id, Identifier: "digitaldrywood/detent#200", TotalTokens: 2_000_000})
	}
	snapshot.Attempts = append(snapshot.Attempts, Attempt{
		ID: 20, Identifier: "digitaldrywood/detent#200", StartedAt: base.Add(20 * time.Minute), CompletedAt: base.Add(21 * time.Minute), TerminalState: "success",
	})
	for index, issue := range []string{"digitaldrywood/detent#301", "digitaldrywood/detent#302", "digitaldrywood/detent#303"} {
		startedAt := base.Add(time.Duration(30+index) * time.Minute)
		snapshot.Attempts = append(snapshot.Attempts, Attempt{
			ID: int64(30 + index), Identifier: issue, StartedAt: startedAt, CompletedAt: startedAt.Add(time.Minute),
			TerminalState: "capacity", ErrorClass: "backend_capacity", ErrorMessage: "quota exceeded; provider usage limit reached",
		})
	}

	findings := Detect(snapshot, DetectorOptions{})
	patterns := findingPatterns(findings)
	for _, want := range []string{PatternCompletedRedispatch, PatternCeilingThenSuccess, PatternSystemicBreaker + ":quota"} {
		if !slices.Contains(patterns, want) {
			t.Fatalf("patterns = %v, want %s", patterns, want)
		}
	}
	ceiling := findingByPattern(t, findings, PatternCeilingThenSuccess)
	if len(ceiling.Occurrences) != 5 || ceiling.TokenDelta != 10_000_000 {
		t.Fatalf("ceiling finding = %#v, want five incidents and 10000000 token delta", ceiling)
	}
	quota := findingByPattern(t, findings, PatternSystemicBreaker+":quota")
	if quota.Scope != ScopeProduct || quota.Severity != SeverityCritical || len(quota.Occurrences) != 3 {
		t.Fatalf("quota finding = %#v, want critical product cascade with three occurrences", quota)
	}
}

func TestDetectAdditionalPatterns(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	resetAt := base.Add(time.Hour)
	snapshot := Snapshot{
		Attempts: []Attempt{
			{ID: 1, Identifier: "issue-capacity", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "capacity", ErrorClass: "backend_capacity", CapacitySnapshotJSON: `{"outage":{"reset_at":"` + resetAt.Format(time.RFC3339) + `"}}`},
			{ID: 2, Identifier: "issue-capacity-persisted", StartedAt: resetAt.Add(5 * time.Minute), CompletedAt: resetAt.Add(6 * time.Minute), TerminalState: "capacity", ErrorClass: "backend_capacity"},
			{ID: 3, Identifier: "issue-after-capacity", StartedAt: resetAt.Add(10 * time.Minute), CompletedAt: resetAt.Add(11 * time.Minute), TerminalState: "success"},
			{ID: 4, Identifier: "issue-gate", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "timed_out", ErrorClass: "gate_wait_timeout"},
			{ID: 5, Identifier: "issue-gate-2", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "timed_out", WaitReason: "gate wait timeout"},
			{ID: 6, Identifier: "issue-spend", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "no_progress", ErrorClass: "spend_since_progress_circuit_breaker", ErrorMessage: "spent $6.75; configured limit $5.00"},
		},
		Sessions: []Session{
			{ID: 1, Identifier: "issue-orphan-a", StartedAt: base, CompletedAt: base.Add(time.Minute), OrphanRecoveryOutcome: "fresh"},
			{ID: 2, Identifier: "issue-orphan-b", StartedAt: base.Add(time.Minute), CompletedAt: base.Add(2 * time.Minute), OrphanRecoveryOutcome: "fresh"},
			{ID: 3, Identifier: "issue-orphan-c", StartedAt: base.Add(2 * time.Minute), CompletedAt: base.Add(3 * time.Minute), OrphanRecoveryOutcome: "fresh"},
			{ID: 4, WorkAttemptID: 6, Identifier: "issue-spend", StartedAt: base, CompletedAt: base.Add(time.Minute), TotalTokens: 125000},
		},
		UsageEvents: []UsageEvent{
			{Identifier: "issue-small-a", FinishedAt: base, TotalTokens: 100},
			{Identifier: "issue-small-b", FinishedAt: base, TotalTokens: 100},
			{Identifier: "issue-large", FinishedAt: base, TotalTokens: 500},
		},
		PhaseEvents: []PhaseEvent{
			{Identifier: "issue-status-a", Reason: "workpad_status_invalid", StartedAt: base},
			{Identifier: "issue-status-b", Reason: "workpad_status_invalid", StartedAt: base.Add(time.Minute)},
		},
	}

	patterns := findingPatterns(Detect(snapshot, DetectorOptions{}))
	for _, want := range []string{PatternReceiptBaseline, PatternGateWaitTimeout, PatternInvalidWorkpadStatus, PatternSlowCapacityRecovery, PatternFallbackOrphanRecovery, PatternSpendSinceProgressTrip} {
		if !slices.Contains(patterns, want) {
			t.Fatalf("patterns = %v, want %s", patterns, want)
		}
	}
	spend := findingByPattern(t, Detect(snapshot, DetectorOptions{}), PatternSpendSinceProgressTrip)
	if spend.Scope != ScopeProduct || spend.Severity != SeverityCritical || spend.TokenDelta != 125000 || !Qualifies(spend, 2, SeverityCritical) {
		t.Fatalf("spend finding = %#v, want qualifying critical trip", spend)
	}
	orphan := findingByPattern(t, Detect(snapshot, DetectorOptions{}), PatternFallbackOrphanRecovery)
	if len(orphan.Occurrences) != 3 || !Qualifies(orphan, 2, SeverityCritical) {
		t.Fatalf("orphan finding = %#v, want three qualifying occurrences", orphan)
	}
	if got := []string{orphan.Occurrences[0].Issue, orphan.Occurrences[1].Issue, orphan.Occurrences[2].Issue}; !slices.Equal(got, []string{"issue-orphan-a", "issue-orphan-b", "issue-orphan-c"}) {
		t.Fatalf("orphan issues = %v, want project-wide consecutive fallbacks", got)
	}
}

func TestDetectSlowCapacityRecoveryIgnoresIdleTime(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	resetAt := base.Add(time.Hour)
	findings := Detect(Snapshot{Attempts: []Attempt{
		{ID: 1, Identifier: "issue-capacity", StartedAt: base, CompletedAt: base.Add(time.Minute), TerminalState: "capacity", ErrorClass: "backend_capacity", CapacitySnapshotJSON: `{"reset_at":"` + resetAt.Format(time.RFC3339) + `"}`},
		{ID: 2, Identifier: "issue-after-idle", StartedAt: resetAt.Add(time.Hour), CompletedAt: resetAt.Add(time.Hour + time.Minute), TerminalState: "success"},
	}}, DetectorOptions{})
	if slices.Contains(findingPatterns(findings), PatternSlowCapacityRecovery) {
		t.Fatalf("patterns = %v, want idle time ignored", findingPatterns(findings))
	}
}

func TestQualifiesRequiresRecurrenceOrCriticalSeverity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		finding Finding
		want    bool
	}{
		{name: "two warnings", finding: Finding{Severity: SeverityWarning, Occurrences: []Occurrence{{}, {}}}, want: true},
		{name: "one high", finding: Finding{Severity: SeverityHigh, Occurrences: []Occurrence{{}}}, want: false},
		{name: "one critical", finding: Finding{Severity: SeverityCritical, Occurrences: []Occurrence{{}}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Qualifies(tt.finding, 2, SeverityCritical); got != tt.want {
				t.Fatalf("Qualifies() = %t, want %t", got, tt.want)
			}
		})
	}
}

func findingPatterns(findings []Finding) []string {
	patterns := make([]string, 0, len(findings))
	for _, finding := range findings {
		patterns = append(patterns, finding.Pattern)
	}
	return patterns
}

func findingByPattern(t *testing.T, findings []Finding, pattern string) Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Pattern == pattern {
			return finding
		}
	}
	t.Fatalf("finding %s not found in %#v", pattern, findings)
	return Finding{}
}
