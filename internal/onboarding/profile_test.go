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
			name:    "autonomous delivery",
			profile: "autonomous_delivery",
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
			name:    "conservative review alias",
			profile: "conservative",
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

	if _, ok := DeliveryProfileAnswerExpansion("manual"); ok {
		t.Fatal("DeliveryProfileAnswerExpansion(manual) ok = true, want false")
	}
}
