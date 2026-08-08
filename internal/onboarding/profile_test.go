package onboarding

import (
	"maps"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
)

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
				"AUTO_PROMOTE_GATE_WAIT_STATE":          "source",
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
				"AUTO_PROMOTE_GATE_WAIT_STATE":          "review",
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
				"AUTO_PROMOTE_GATE_WAIT_STATE":          "review",
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

func TestIntakeProfileAnswerExpansion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile string
		want    map[string]string
	}{
		{
			name:    "manual",
			profile: IntakeProfileManual,
			want: map[string]string{
				"FOLLOWUPS_ENABLED":         "true",
				"BACKLOG_ADMISSION_ENABLED": "false",
				"ROUTINES_ENABLED":          "false",
				"STALE_TODOS_ENABLED":       "false",
			},
		},
		{
			name:    "assisted",
			profile: IntakeProfileAssisted,
			want: map[string]string{
				"FOLLOWUPS_ENABLED":                           "true",
				"BACKLOG_ADMISSION_ENABLED":                   "true",
				"BACKLOG_ADMISSION_SCHEDULE":                  "*/15 * * * *",
				"BACKLOG_ADMISSION_SOURCE_STATE":              "Backlog",
				"BACKLOG_ADMISSION_TARGET_STATE":              "Todo",
				"BACKLOG_ADMISSION_CRITERIA_SECTION":          IntakeProfileCriteriaSection,
				"BACKLOG_ADMISSION_MAX_CANDIDATES_PER_RUN":    "50",
				"BACKLOG_ADMISSION_MAX_PROPOSALS_PER_RUN":     "3",
				"BACKLOG_ADMISSION_MAX_OPEN_PROPOSALS":        "10",
				"BACKLOG_ADMISSION_PROPOSAL_EXPIRY_DAYS":      "7",
				"BACKLOG_ADMISSION_AUTO_ADMIT":                "false",
				"BACKLOG_ADMISSION_AUTO_ADMIT_MIN_CONFIDENCE": "0.9",
				"ROUTINES_ENABLED":                            "false",
				"STALE_TODOS_ENABLED":                         "false",
			},
		},
		{
			name:    "autonomous",
			profile: IntakeProfileAutonomous,
			want: map[string]string{
				"FOLLOWUPS_ENABLED":                           "true",
				"BACKLOG_ADMISSION_ENABLED":                   "true",
				"BACKLOG_ADMISSION_SCHEDULE":                  "*/15 * * * *",
				"BACKLOG_ADMISSION_SOURCE_STATE":              "Backlog",
				"BACKLOG_ADMISSION_TARGET_STATE":              "Todo",
				"BACKLOG_ADMISSION_CRITERIA_SECTION":          IntakeProfileCriteriaSection,
				"BACKLOG_ADMISSION_MAX_CANDIDATES_PER_RUN":    "50",
				"BACKLOG_ADMISSION_MAX_PROPOSALS_PER_RUN":     "3",
				"BACKLOG_ADMISSION_MAX_OPEN_PROPOSALS":        "10",
				"BACKLOG_ADMISSION_PROPOSAL_EXPIRY_DAYS":      "7",
				"BACKLOG_ADMISSION_AUTO_ADMIT":                "true",
				"BACKLOG_ADMISSION_AUTO_ADMIT_MIN_CONFIDENCE": "0.9",
				"ROUTINES_ENABLED":                            "true",
				"ROUTINE_NAME":                                IntakeProfileRoutineName,
				"ROUTINE_SCHEDULE":                            IntakeProfileRoutineSchedule,
				"ROUTINE_PROMPT":                              IntakeProfileRoutinePrompt,
				"STALE_TODOS_ENABLED":                         "true",
				"STALE_TODOS_SCHEDULE":                        IntakeProfileStaleTODOsSchedule,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := IntakeProfileAnswerExpansion(tt.profile)
			if !ok {
				t.Fatalf("IntakeProfileAnswerExpansion(%q) ok = false, want true", tt.profile)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("answers = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIntakeProfileRejectsUnknown(t *testing.T) {
	t.Parallel()

	if _, ok := IntakeProfileAnswerExpansion("unbounded_intake"); ok {
		t.Fatal("IntakeProfileAnswerExpansion(unbounded_intake) ok = true, want false")
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

func TestAgentPoolChoiceForProjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		projects    []ProjectWorkload
		wantOffered bool
	}{
		{
			name: "mixed classes",
			projects: []ProjectWorkload{
				{ID: "video", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindArtifact}}},
				{ID: "detent", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"}}},
			},
			wantOffered: true,
		},
		{
			name: "single project",
			projects: []ProjectWorkload{
				{ID: "detent", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"}}},
			},
		},
		{
			name: "all local heavy",
			projects: []ProjectWorkload{
				{ID: "detent", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"}}},
				{ID: "gopher-ai", Workflow: workflowconfig.Config{Gate: gate.Config{CITriggerLabel: "ci:ready"}}},
			},
		},
		{
			name: "all cloud only",
			projects: []ProjectWorkload{
				{ID: "video", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindArtifact}}},
				{ID: "podcast", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindHumanReview}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, offered := AgentPoolChoiceForProjects(tt.projects)
			if offered != tt.wantOffered {
				t.Fatalf("AgentPoolChoiceForProjects() offered = %t, want %t: %#v", offered, tt.wantOffered, got)
			}
			if !offered {
				return
			}
			if len(got.Options) != 2 || got.Options[0].ID != "split" || got.Options[1].ID != "shared" {
				t.Fatalf("options = %#v, want split and shared choices", got.Options)
			}
			if got.Question == "" || strings.Contains(strings.ToLower(got.Question), "warn") {
				t.Fatalf("question = %q, want optional non-warning wording", got.Question)
			}
			if len(got.LocalProjects) != 1 || got.LocalProjects[0] != "detent" ||
				len(got.CloudProjects) != 1 || got.CloudProjects[0] != "video" {
				t.Fatalf("project classes = local %#v cloud %#v", got.LocalProjects, got.CloudProjects)
			}
		})
	}
}

func TestSummarizeDeliveryProfileForProjectsIncludesPoolChoice(t *testing.T) {
	t.Parallel()

	summary, ok := SummarizeDeliveryProfileForProjects("full_autopilot", []ProjectWorkload{
		{ID: "detent", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindCommand, Run: "make check"}}},
		{ID: "video", Workflow: workflowconfig.Config{Gate: gate.Config{Kind: gate.KindArtifact}}},
	})
	if !ok {
		t.Fatal("SummarizeDeliveryProfileForProjects() ok = false, want true")
	}
	if summary.AgentPoolChoice == nil {
		t.Fatal("AgentPoolChoice = nil, want mixed-workload choice")
	}
}
