package hubhealth

import (
	"fmt"
	"testing"
	"time"
)

// TestHealthRunnerSetupFindings is #1408. Repeated pre-provider failures that
// the runner attributes to the instance surface as a Diagnostics finding on
// that runner, with no baseline required, while issue-attributed, older and
// other runners' failures do not.
func TestHealthRunnerSetupFindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	setup := func(runner string, age time.Duration, n int, instance bool) []RateAttempt {
		out := []RateAttempt{}
		for i := range n {
			out = append(out, RateAttempt{ID: fmt.Sprint(runner, "-", age, "-", i), Item: fmt.Sprint("item-", i), Status: "failed", Runner: runner, Signature: "workspace hook after_create failed (exit status <n>)", InstanceSetupFailure: instance, At: now.Add(-age)})
		}
		return out
	}
	for _, test := range []struct {
		name     string
		attempts []RateAttempt
		want     map[string]int
	}{
		{name: "repeated instance setup failures on one runner", attempts: setup("runner-air", 10*time.Minute, 3, true), want: map[string]int{"runner-air": 3}},
		{name: "each failing runner is reported", attempts: append(setup("runner-air", 5*time.Minute, 4, true), setup("runner-mini", 20*time.Minute, 3, true)...), want: map[string]int{"runner-air": 4, "runner-mini": 3}},
		{name: "below threshold is quiet", attempts: setup("runner-air", 10*time.Minute, 2, true), want: map[string]int{}},
		{name: "older failures are quiet", attempts: setup("runner-air", 2*time.Hour, 6, true), want: map[string]int{}},
		{name: "issue attributed failures are quiet", attempts: setup("runner-air", 10*time.Minute, 6, false), want: map[string]int{}},
		{name: "unattributed runner is quiet", attempts: setup("", 10*time.Minute, 6, true), want: map[string]int{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := map[string]int{}
			for _, f := range evaluateHealthRates(now, []RateProject{{ID: "project", Attempts: test.attempts}}) {
				if f.Signal != "runner_setup_failures" {
					continue
				}
				if f.Class != "instance" || f.Subject.Kind != "runner" || len(f.Evidence.AttemptIDs) == 0 || len(f.Evidence.Signatures) != 1 {
					t.Fatalf("finding = %#v, want an instance runner finding with attempt and signature evidence", f)
				}
				got[f.Subject.ID] = f.Evidence.Counts["failures_1h"]
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("runner setup findings = %v, want %v", got, test.want)
			}
		})
	}

	t.Run("failures below threshold in each project aggregate per runner", func(t *testing.T) {
		t.Parallel()
		findings := evaluateHealthRates(now, []RateProject{{ID: "first", Attempts: setup("runner-air", 10*time.Minute, 2, true)}, {ID: "second", Attempts: setup("runner-air", 20*time.Minute, 2, true)}, {ID: "third", Attempts: setup("runner-mini", 10*time.Minute, 2, true)}})
		got := map[string]string{}
		for _, f := range findings {
			if f.Signal == "runner_setup_failures" {
				got[f.Subject.ID] = fmt.Sprint(f.Projects, f.Evidence.Counts["failures_1h"])
			}
		}
		if fmt.Sprint(got) != "map[runner-air:[first second] 4]" {
			t.Fatalf("runner setup findings = %v, want only runner-air across first and second with 4 failures", got)
		}
	})

	t.Run("one finding per runner across projects", func(t *testing.T) {
		t.Parallel()
		findings := evaluateHealthRates(now, []RateProject{{ID: "first", Attempts: setup("runner-air", 10*time.Minute, 3, true)}, {ID: "second", Attempts: setup("runner-air", 10*time.Minute, 4, true)}})
		var matched []Finding
		for _, f := range findings {
			if f.Signal == "runner_setup_failures" {
				matched = append(matched, f)
			}
		}
		if len(matched) != 1 || fmt.Sprint(matched[0].Projects) != "[first second]" || matched[0].Evidence.Counts["failures_1h"] != 7 {
			t.Fatalf("findings = %#v, want one runner finding covering both projects", matched)
		}
	})
}
