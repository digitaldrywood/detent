package hubserver

import (
	"strings"
	"testing"
	"time"
)

func TestHealthRules(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		signal    string
		snapshot  healthSnapshot
		recovered healthSnapshot
	}{
		{"heartbeat with ranks", "runner_heartbeat_gap", healthSnapshot{Runners: []healthRunner{{ID: "runner", Projects: []string{"project"}, Heartbeat: now.Add(-5 * time.Minute)}}}, healthSnapshot{Runners: []healthRunner{{ID: "runner", Projects: []string{"project"}, Heartbeat: now}}}},
		{"heartbeat with leases", "runner_heartbeat_gap", healthSnapshot{Runners: []healthRunner{{ID: "runner", Leases: 1, Heartbeat: now.Add(-5 * time.Minute)}}}, healthSnapshot{Runners: []healthRunner{{ID: "runner", Heartbeat: now.Add(-5 * time.Minute)}}}},
		{"dead man", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, LastLanding: now.Add(-30 * time.Minute)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, LastLanding: now}}}},
		{"dead man without landing history", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, CandidateSince: now.Add(-30 * time.Minute)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 0, FreeSlots: 1, CandidateSince: now.Add(-30 * time.Minute)}}}},
		{"scheduled silent minimum window", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now.Add(-6 * time.Hour), Cadence: time.Hour}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now, LastGreen: now}}}}},
		{"scheduled silent observed cadence", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now.Add(-12 * time.Hour), Cadence: 6 * time.Hour}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now, LastGreen: now, Cadence: 6 * time.Hour}}}}},
		{"scheduled red without a green", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{FirstReport: now.Add(-24 * time.Hour), LastReport: now, LastFailed: true, Failures: 2}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{FirstReport: now.Add(-24 * time.Hour), LastReport: now, LastGreen: now, Failures: 2}}}}},
		{"scheduled red since last green", "dead_man", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastGreen: now.Add(-24 * time.Hour), LastReport: now, LastFailed: true, Failures: 2}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastGreen: now, LastReport: now, Failures: 2}}}}},
		{"scheduler stale", "scheduler_loop_behind", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now.Add(-5*time.Minute - time.Nanosecond)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now}}}},
		{"scheduler slow", "scheduler_loop_behind", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now, RefreshDuration: 5*time.Minute + time.Nanosecond}}}, healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now, RefreshDuration: time.Second}}}},
		{"all refused", "every_candidate_refused", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch", "policy_mismatch"}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch", "capacity_full"}}}}},
		{"unavailable runner", "every_candidate_refused", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"runner_heartbeat_stale", "runner_heartbeat_stale"}}}}, healthSnapshot{}},
		{"human question", "unanswered_human_wait", healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Since: now.Add(-30*time.Minute - time.Nanosecond), Active: true}}}, healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Since: now.Add(-time.Hour)}}}},
		{"permission wait", "unanswered_human_wait", healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Attempt: "attempt", Since: now.Add(-time.Hour), Active: true}}}, healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Attempt: "attempt", Since: now, Active: true}}}},
		{"dependency hold", "dependency_hold", healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 3, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-time.Hour), LastLanding: now.Add(-30 * time.Minute)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 3, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-time.Hour), LastLanding: now}}}},
		{"dependency hold without landing history", "dependency_hold", healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 1, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-30 * time.Minute)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 1, HoldSince: now.Add(-30 * time.Minute)}}}},
		{"lifetime limit", "lifetime_limit_hit", healthSnapshot{Limits: []healthLimit{{Item: "item", Project: "project", Event: "event", Active: true}}}, healthSnapshot{Limits: []healthLimit{{Item: "item", Project: "project", Event: "recovered"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateHealth(now, test.snapshot)
			if len(got) != 1 || got[0].Signal != test.signal || got[0].Severity != "attention" || got[0].Subject.ID == "" || got[0].NextAction == "" {
				t.Fatalf("findings = %+v", got)
			}
			if test.name == "all refused" {
				if got[0].Evidence.Reason != "policy_mismatch" || got[0].Summary != "Candidates cannot start work because runners refused every claim." || got[0].NextAction != "The runner operator checks the claim refusal in the runner's logs." {
					t.Fatalf("policy refusal guidance = %+v", got[0])
				}
			}
			if repeat := evaluateHealth(now.Add(time.Second), test.snapshot); len(repeat) != 1 || repeat[0].Fingerprint != got[0].Fingerprint {
				t.Fatalf("unstable fingerprint: %+v", repeat)
			}
			if recovered := evaluateHealth(now, test.recovered); len(recovered) != 0 {
				t.Fatalf("recovered findings = %+v", recovered)
			}
		})
	}
	for _, test := range []struct {
		name     string
		snapshot healthSnapshot
	}{
		{"empty tenant", healthSnapshot{}},
		{"idle runner", healthSnapshot{Runners: []healthRunner{{ID: "runner", Heartbeat: now.Add(-time.Hour)}}}},
		{"heartbeat before threshold", healthSnapshot{Runners: []healthRunner{{ID: "runner", Leases: 1, Heartbeat: now.Add(-5*time.Minute + time.Nanosecond)}}}},
		{"no free slot", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, LastLanding: now.Add(-time.Hour)}}}},
		{"no candidates", healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, LastLanding: now.Add(-time.Hour)}}}},
		{"scheduled never reported", healthSnapshot{Projects: []healthProject{{ID: "project"}}}},
		{"scheduled before minimum window", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now.Add(-6*time.Hour + time.Nanosecond), Cadence: time.Hour}}}}},
		{"scheduled before observed cadence window", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastReport: now.Add(-12*time.Hour + time.Nanosecond), Cadence: 6 * time.Hour}}}}},
		{"scheduled recently green", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{LastGreen: now.Add(-24*time.Hour + time.Nanosecond), LastReport: now, LastFailed: true, Failures: 2}}}}},
		{"scheduled only one failure", healthSnapshot{Projects: []healthProject{{ID: "project", Scheduled: healthScheduledValidation{FirstReport: now.Add(-24 * time.Hour), LastReport: now, LastFailed: true, Failures: 1}}}}},
		{"dead man before threshold", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, LastLanding: now.Add(-30*time.Minute + time.Nanosecond)}}}},
		{"refresh exact threshold", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now.Add(-5 * time.Minute), RefreshDuration: 5 * time.Minute}}}},
		{"claim in cycle", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, Claims: 1, RefusalReasons: []string{"policy_mismatch"}}}}},
		{"incomplete cycle", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch"}}}}},
		{"active work fills every runner", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"runner_capacity_full", "runner_capacity_full"}}}}},
		{"saturated project runners with organization room", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"runner_capacity_full", "runner_capacity_full"}}, {ID: "other-project", FreeSlots: 1}}}},
		{"recorded capacity backpressure", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"capacity_full", "capacity_full"}}}}},
		{"missing refusal reason", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, RefusalReasons: []string{""}}}}},
		{"dependency hold with a claimable candidate", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, DependencyGated: 1, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-time.Hour)}}}},
		{"new dependency hold without landing history", healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 1, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-30*time.Minute + time.Nanosecond)}}}},
		{"new dependency hold after an old landing", healthSnapshot{Projects: []healthProject{{ID: "project", FreeSlots: 1, DependencyGated: 1, HeldCount: 1, HeldBlockers: []string{"held"}, HoldSince: now.Add(-time.Minute), LastLanding: now.Add(-24 * time.Hour)}}}},
		{"dependency hold without free slots", healthSnapshot{Projects: []healthProject{{ID: "project", DependencyGated: 1, HeldCount: 1, HeldBlockers: []string{"held"}}}}},
		{"wait exact threshold", healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Since: now.Add(-30 * time.Minute), Active: true}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := evaluateHealth(now, test.snapshot); len(got) != 0 {
				t.Fatalf("findings = %+v", got)
			}
		})
	}
}

func TestHealthEvidenceBound(t *testing.T) {
	t.Parallel()
	events := make([]string, healthEvidenceLimit+5)
	for i := range events {
		events[i] = strings.Repeat("e", 10)
	}
	got := newHealthFinding("signal", "instance", "runner", "runner", "summary", "action", []string{"project"}, healthEvidence{EventIDs: events, AttemptIDs: events})
	if len(got.Evidence.EventIDs) != healthEvidenceLimit || len(got.Evidence.AttemptIDs) != healthEvidenceLimit {
		t.Fatalf("unbounded evidence: %+v", got.Evidence)
	}
	events[0] = "changed"
	if got.Evidence.EventIDs[0] == "changed" {
		t.Fatal("finding aliases snapshot evidence")
	}
}
