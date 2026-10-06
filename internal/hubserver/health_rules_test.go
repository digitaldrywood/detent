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
		{"scheduler stale", "scheduler_loop_behind", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now.Add(-5*time.Minute - time.Nanosecond)}}}, healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now}}}},
		{"scheduler slow", "scheduler_loop_behind", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now, RefreshDuration: 5*time.Minute + time.Nanosecond}}}, healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now, RefreshDuration: time.Second}}}},
		{"all refused", "every_candidate_refused", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch", "policy_mismatch"}}}}, healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch", "capacity_full"}}}}},
		{"human question", "unanswered_human_wait", healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Since: now.Add(-30*time.Minute - time.Nanosecond), Active: true}}}, healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Since: now.Add(-time.Hour)}}}},
		{"permission wait", "unanswered_human_wait", healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Attempt: "attempt", Since: now.Add(-time.Hour), Active: true}}}, healthSnapshot{Waits: []healthWait{{Item: "item", Project: "project", Attempt: "attempt", Since: now, Active: true}}}},
		{"lifetime limit", "lifetime_limit_hit", healthSnapshot{Limits: []healthLimit{{Item: "item", Project: "project", Event: "event", Active: true}}}, healthSnapshot{Limits: []healthLimit{{Item: "item", Project: "project", Event: "recovered"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateHealth(now, test.snapshot)
			if len(got) != 1 || got[0].Signal != test.signal || got[0].Severity != "attention" || got[0].Subject.ID == "" || got[0].NextAction == "" {
				t.Fatalf("findings = %+v", got)
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
		{"dead man before threshold", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, FreeSlots: 1, LastLanding: now.Add(-30*time.Minute + time.Nanosecond)}}}},
		{"refresh exact threshold", healthSnapshot{Projects: []healthProject{{ID: "project", RefreshAt: now.Add(-5 * time.Minute), RefreshDuration: 5 * time.Minute}}}},
		{"claim in cycle", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, Claims: 1, RefusalReasons: []string{"policy_mismatch"}}}}},
		{"incomplete cycle", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 2, RefusalReasons: []string{"policy_mismatch"}}}}},
		{"missing refusal reason", healthSnapshot{Projects: []healthProject{{ID: "project", Candidates: 1, RefusalReasons: []string{""}}}}},
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
