package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMergeRequiredCheckStreakLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps []mergeRequiredCheckTestStep
	}{
		{
			name: "increments independent checks",
			steps: []mergeRequiredCheckTestStep{
				{missing: []string{"Test", "Lint"}, want: map[string]int{"Lint": 1, "Test": 1}},
				{missing: []string{"Test"}, want: map[string]int{"Test": 2}},
			},
		},
		{
			name: "appearance resets",
			steps: []mergeRequiredCheckTestStep{
				{missing: []string{"Test"}, want: map[string]int{"Test": 1}},
				{want: map[string]int{}},
				{missing: []string{"Test"}, want: map[string]int{"Test": 1}},
			},
		},
		{
			name: "config change resets",
			steps: []mergeRequiredCheckTestStep{
				{fingerprint: "config-a", missing: []string{"Test"}, want: map[string]int{"Test": 1}},
				{fingerprint: "config-a", missing: []string{"Test"}, want: map[string]int{"Test": 2}},
				{fingerprint: "config-b", missing: []string{"Test"}, want: map[string]int{"Test": 1}},
			},
		},
		{
			name: "pull request change resets",
			steps: []mergeRequiredCheckTestStep{
				{prNumber: 41, missing: []string{"Test"}, want: map[string]int{"Test": 1}},
				{prNumber: 41, missing: []string{"Test"}, want: map[string]int{"Test": 2}},
				{prNumber: 42, missing: []string{"Test"}, want: map[string]int{"Test": 1}},
			},
		},
		{
			name: "head SHA change resets",
			steps: []mergeRequiredCheckTestStep{
				{headSHA: "head-a", missing: []string{"Test"}, want: map[string]int{"Test": 1}},
				{headSHA: "head-a", missing: []string{"Test"}, want: map[string]int{"Test": 2}},
				{headSHA: "head-b", missing: []string{"Test"}, want: map[string]int{"Test": 1}},
			},
		},
		{
			name: "terminal clear resets",
			steps: []mergeRequiredCheckTestStep{
				{missing: []string{"Test"}, want: map[string]int{"Test": 1}},
				{missing: []string{"Test"}, want: map[string]int{"Test": 2}},
				{clear: true},
				{missing: []string{"Test"}, want: map[string]int{"Test": 1}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend, err := Open(t.Context(), Config{Path: filepath.Join(t.TempDir(), "detent.db")})
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := backend.Close(); err != nil {
					t.Fatalf("Close() error = %v", err)
				}
			})

			for index, step := range tt.steps {
				if step.clear {
					if err := backend.ClearMergeRequiredCheckStreaks(t.Context(), "detent", "issue-1634"); err != nil {
						t.Fatalf("step %d ClearMergeRequiredCheckStreaks() error = %v", index, err)
					}
					continue
				}
				fingerprint := step.fingerprint
				if fingerprint == "" {
					fingerprint = "config"
				}
				prNumber := step.prNumber
				if prNumber == 0 {
					prNumber = 41
				}
				headSHA := step.headSHA
				if headSHA == "" {
					headSHA = "head-a"
				}
				streaks, err := backend.EvaluateMergeRequiredChecks(t.Context(), MergeRequiredCheckEvaluation{
					ProjectID:                 "detent",
					IssueID:                   "issue-1634",
					Repository:                "digitaldrywood/detent",
					PRNumber:                  prNumber,
					HeadSHA:                   headSHA,
					RequiredChecksFingerprint: fingerprint,
					MissingChecks:             step.missing,
					EvaluatedAt:               time.Date(2026, 8, 7, 15, index, 0, 0, time.UTC),
				})
				if err != nil {
					t.Fatalf("step %d EvaluateMergeRequiredChecks() error = %v", index, err)
				}
				got := map[string]int{}
				for _, streak := range streaks {
					got[streak.CheckName] = streak.ConsecutiveMissing
				}
				if !reflect.DeepEqual(got, step.want) {
					t.Fatalf("step %d streaks = %#v, want %#v", index, got, step.want)
				}
			}
		})
	}
}

type mergeRequiredCheckTestStep struct {
	prNumber    int
	headSHA     string
	fingerprint string
	missing     []string
	clear       bool
	want        map[string]int
}

func TestMergeRequiredCheckStreakValidation(t *testing.T) {
	t.Parallel()

	backend, err := Open(context.Background(), Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	tests := []struct {
		name       string
		evaluation MergeRequiredCheckEvaluation
		want       string
	}{
		{name: "project", evaluation: MergeRequiredCheckEvaluation{}, want: "project_id"},
		{name: "issue", evaluation: MergeRequiredCheckEvaluation{ProjectID: "detent"}, want: "issue_id"},
		{name: "pull request", evaluation: MergeRequiredCheckEvaluation{ProjectID: "detent", IssueID: "issue"}, want: "pr_number"},
		{name: "head SHA", evaluation: MergeRequiredCheckEvaluation{ProjectID: "detent", IssueID: "issue", PRNumber: 1}, want: "head_sha"},
		{name: "fingerprint", evaluation: MergeRequiredCheckEvaluation{ProjectID: "detent", IssueID: "issue", PRNumber: 1, HeadSHA: "head"}, want: "required_checks_fingerprint"},
		{name: "timestamp", evaluation: MergeRequiredCheckEvaluation{ProjectID: "detent", IssueID: "issue", PRNumber: 1, HeadSHA: "head", RequiredChecksFingerprint: "config"}, want: "evaluated_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := backend.EvaluateMergeRequiredChecks(t.Context(), tt.evaluation)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EvaluateMergeRequiredChecks() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMergeRequiredCheckLegacyStreakDoesNotCarryToCurrentHead(t *testing.T) {
	t.Parallel()

	backend, err := Open(t.Context(), Config{Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	sqliteBackend, ok := backend.(*sqliteStore)
	if !ok {
		t.Fatalf("Open() backend = %T, want *sqliteStore", backend)
	}
	if _, err := sqliteBackend.db.ExecContext(t.Context(), `
INSERT INTO merge_required_check_streaks (
  project_id, issue_id, repository, pr_number, check_name,
  required_checks_fingerprint, consecutive_missing, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"detent", "issue-1634", "digitaldrywood/detent", 41, "Test", "config", 2, "2026-08-07T16:00:00Z",
	); err != nil {
		t.Fatalf("seed legacy streak error = %v", err)
	}

	streaks, err := backend.EvaluateMergeRequiredChecks(t.Context(), MergeRequiredCheckEvaluation{
		ProjectID:                 "detent",
		IssueID:                   "issue-1634",
		Repository:                "digitaldrywood/detent",
		PRNumber:                  41,
		HeadSHA:                   "head-a",
		RequiredChecksFingerprint: "config",
		MissingChecks:             []string{"Test"},
		EvaluatedAt:               time.Date(2026, 8, 7, 16, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("EvaluateMergeRequiredChecks() error = %v", err)
	}
	if len(streaks) != 1 || streaks[0].ConsecutiveMissing != 1 {
		t.Fatalf("streaks = %#v, want fresh current-head count 1", streaks)
	}
}

func TestMergeRequiredCheckStreakPersistsAcrossStoreRestart(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "detent.db")
	backend, err := Open(t.Context(), Config{Path: dbPath})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	evaluation := MergeRequiredCheckEvaluation{
		ProjectID:                 "detent",
		IssueID:                   "issue-1634",
		Repository:                "digitaldrywood/detent",
		PRNumber:                  41,
		HeadSHA:                   "head-a",
		RequiredChecksFingerprint: "config",
		MissingChecks:             []string{"Test"},
		EvaluatedAt:               time.Date(2026, 8, 7, 16, 0, 0, 0, time.UTC),
	}
	for count := 1; count <= 2; count++ {
		streaks, err := backend.EvaluateMergeRequiredChecks(t.Context(), evaluation)
		if err != nil {
			t.Fatalf("EvaluateMergeRequiredChecks() error = %v", err)
		}
		if len(streaks) != 1 || streaks[0].ConsecutiveMissing != count {
			t.Fatalf("streaks = %#v, want count %d", streaks, count)
		}
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	backend, err = Open(t.Context(), Config{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	evaluation.EvaluatedAt = evaluation.EvaluatedAt.Add(time.Minute)
	streaks, err := backend.EvaluateMergeRequiredChecks(t.Context(), evaluation)
	if err != nil {
		t.Fatalf("EvaluateMergeRequiredChecks() after restart error = %v", err)
	}
	if len(streaks) != 1 || streaks[0].ConsecutiveMissing != 3 {
		t.Fatalf("streaks after restart = %#v, want count 3", streaks)
	}
}
