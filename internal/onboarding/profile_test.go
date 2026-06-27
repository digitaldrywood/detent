package onboarding

import "testing"

func TestDeliveryProfileAnswerExpansion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile string
		want    map[string]string
	}{
		{
			name:    "full autopilot",
			profile: "full_autopilot",
			want: map[string]string{
				"KANBAN_MODE":                           "integration",
				"AUTO_PROMOTE_ENABLED":                  "true",
				"AUTO_PROMOTE_QUIET_SECONDS":            "0",
				"GATE_REQUIRE_AUTOMATED_REVIEW":         "false",
				"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": "false",
				"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       "true",
				"MERGING_CONCURRENCY":                   "1",
			},
		},
		{
			name:    "review gate",
			profile: "review_gate",
			want: map[string]string{
				"KANBAN_MODE":                           "integration",
				"AUTO_PROMOTE_ENABLED":                  "false",
				"AUTO_PROMOTE_QUIET_SECONDS":            "600",
				"GATE_REQUIRE_AUTOMATED_REVIEW":         "false",
				"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": "false",
				"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       "false",
				"MERGING_CONCURRENCY":                   "1",
			},
		},
		{
			name:    "conservative manual",
			profile: "conservative_manual",
			want: map[string]string{
				"KANBAN_MODE":                           "read_only",
				"AUTO_PROMOTE_ENABLED":                  "false",
				"AUTO_PROMOTE_QUIET_SECONDS":            "600",
				"GATE_REQUIRE_AUTOMATED_REVIEW":         "true",
				"AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW": "true",
				"DEPENDENCY_AUTO_UNBLOCK_ENABLED":       "false",
				"MERGING_CONCURRENCY":                   "1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := DeliveryProfileAnswerExpansion(tt.profile)
			if !ok {
				t.Fatalf("DeliveryProfileAnswerExpansion(%q) ok = false, want true", tt.profile)
			}
			for key, value := range tt.want {
				if got[key] != value {
					t.Fatalf("answer %s = %q, want %q; all answers = %#v", key, got[key], value, got)
				}
			}
		})
	}
}

func TestDeliveryProfileRejectsUnknown(t *testing.T) {
	t.Parallel()

	if _, ok := DeliveryProfileAnswerExpansion("safe_start"); ok {
		t.Fatal("DeliveryProfileAnswerExpansion(safe_start) ok = true, want false")
	}
}

func TestNormalizeDeliveryProfilePreservesLegacyReviewAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "explicit review gate", value: "review_gate", want: "review_gate"},
		{name: "hyphenated review gate", value: "review-gate", want: "review_gate"},
		{name: "legacy review", value: "review", want: "conservative_manual"},
		{name: "legacy human review", value: "human_review", want: "conservative_manual"},
		{name: "legacy hyphenated human review", value: "human-review", want: "conservative_manual"},
		{name: "legacy conservative review", value: "conservative_review", want: "conservative_manual"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := NormalizeDeliveryProfile(tt.value); got != tt.want {
				t.Fatalf("NormalizeDeliveryProfile(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestSummarizeDeliveryProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                        string
		profile                     string
		wantEffectiveProfile        string
		wantKanbanMode              string
		wantGateRequiresReview      bool
		wantAutoPromoteEnabled      bool
		wantQuietWindow             string
		wantDependencyAutoUnblock   bool
		wantDependencyBehavior      string
		wantMergeConcurrency        int
		wantMergeConcurrencySummary string
	}{
		{
			name:                        "full autopilot",
			profile:                     "full_autopilot",
			wantEffectiveProfile:        "full_autopilot",
			wantKanbanMode:              "integration",
			wantGateRequiresReview:      false,
			wantAutoPromoteEnabled:      true,
			wantQuietWindow:             "There is no quiet-window delay before promotion.",
			wantDependencyAutoUnblock:   true,
			wantDependencyBehavior:      "Dependency-waiting `Blocked` issues can move back to `Todo` when declared blockers are terminal or merged.",
			wantMergeConcurrency:        1,
			wantMergeConcurrencySummary: "`Merging` remains serialized for this project.",
		},
		{
			name:                        "review gate",
			profile:                     "review_gate",
			wantEffectiveProfile:        "review_gate",
			wantKanbanMode:              "integration",
			wantGateRequiresReview:      false,
			wantAutoPromoteEnabled:      false,
			wantQuietWindow:             "Auto-promotion is disabled; the 600-second quiet window only matters if auto-promotion is enabled later.",
			wantDependencyAutoUnblock:   false,
			wantDependencyBehavior:      "Dependency-waiting `Blocked` issues remain `Blocked` until a human or workflow moves them.",
			wantMergeConcurrency:        1,
			wantMergeConcurrencySummary: "`Merging` remains serialized for this project.",
		},
		{
			name:                        "conservative manual",
			profile:                     "conservative_manual",
			wantEffectiveProfile:        "conservative_manual",
			wantKanbanMode:              "read_only",
			wantGateRequiresReview:      true,
			wantAutoPromoteEnabled:      false,
			wantQuietWindow:             "Auto-promotion is disabled; the 600-second quiet window only matters if auto-promotion is enabled later.",
			wantDependencyAutoUnblock:   false,
			wantDependencyBehavior:      "Dependency-waiting `Blocked` issues remain `Blocked` until a human or workflow moves them.",
			wantMergeConcurrency:        1,
			wantMergeConcurrencySummary: "`Merging` remains serialized for this project.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := SummarizeDeliveryProfile(tt.profile)
			if !ok {
				t.Fatalf("SummarizeDeliveryProfile(%q) ok = false, want true", tt.profile)
			}
			if got.EffectiveDeliveryProfile != tt.wantEffectiveProfile {
				t.Fatalf("EffectiveDeliveryProfile = %q, want %q", got.EffectiveDeliveryProfile, tt.wantEffectiveProfile)
			}
			if got.KanbanMode != tt.wantKanbanMode {
				t.Fatalf("KanbanMode = %q, want %q", got.KanbanMode, tt.wantKanbanMode)
			}
			if got.GateRequiresAutomatedReview != tt.wantGateRequiresReview {
				t.Fatalf("GateRequiresAutomatedReview = %t, want %t", got.GateRequiresAutomatedReview, tt.wantGateRequiresReview)
			}
			if got.AutoPromoteEnabled != tt.wantAutoPromoteEnabled {
				t.Fatalf("AutoPromoteEnabled = %t, want %t", got.AutoPromoteEnabled, tt.wantAutoPromoteEnabled)
			}
			if got.QuietWindowBehavior != tt.wantQuietWindow {
				t.Fatalf("QuietWindowBehavior = %q, want %q", got.QuietWindowBehavior, tt.wantQuietWindow)
			}
			if got.DependencyAutoUnblockEnabled != tt.wantDependencyAutoUnblock {
				t.Fatalf("DependencyAutoUnblockEnabled = %t, want %t", got.DependencyAutoUnblockEnabled, tt.wantDependencyAutoUnblock)
			}
			if got.DependencyAutoUnblockBehavior != tt.wantDependencyBehavior {
				t.Fatalf("DependencyAutoUnblockBehavior = %q, want %q", got.DependencyAutoUnblockBehavior, tt.wantDependencyBehavior)
			}
			if got.MergingConcurrency != tt.wantMergeConcurrency {
				t.Fatalf("MergingConcurrency = %d, want %d", got.MergingConcurrency, tt.wantMergeConcurrency)
			}
			if got.MergeConcurrencyBehavior != tt.wantMergeConcurrencySummary {
				t.Fatalf("MergeConcurrencyBehavior = %q, want %q", got.MergeConcurrencyBehavior, tt.wantMergeConcurrencySummary)
			}
			if len(got.StopConditions) == 0 || got.StopBehavior == "" {
				t.Fatalf("stop summary = conditions %#v behavior %q, want populated stop behavior", got.StopConditions, got.StopBehavior)
			}
		})
	}
}
