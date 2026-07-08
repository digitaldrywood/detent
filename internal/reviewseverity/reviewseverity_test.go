package reviewseverity

import "testing"

func TestContainsUsesExplicitBadges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		severity string
		want     bool
	}{
		{
			name:     "bracketed p1 anywhere",
			body:     "Automated review found [P1] missing validation.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "bracketed p2 lowercase",
			body:     "Automated review found [p2] naming concerns.",
			severity: "P2",
			want:     true,
		},
		{
			name:     "line anchored colon",
			body:     "P1: Missing rollback path.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "heading prefixed colon",
			body:     "### P1: Missing rollback path.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "list prefixed badge",
			body:     "- P1 BADGE: Missing rollback path.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "ordered list prefixed p2 badge",
			body:     "1. P2 BADGE naming concern.",
			severity: "P2",
			want:     true,
		},
		{
			name:     "narrative p1 negative",
			body:     "No P1 issues found — approved.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "mid sentence p1 fix",
			body:     "The P1 fix from last week is already present.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "mid sentence badge prose",
			body:     "This mentions P1 BADGE in prose.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "p1 finding prose no longer matches",
			body:     "P1 finding count is zero.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "p2 finding prose no longer matches",
			body:     "P2 finding count is zero.",
			severity: "P2",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Contains(tt.body, tt.severity)
			if got != tt.want {
				t.Fatalf("Contains(%q, %q) = %t, want %t", tt.body, tt.severity, got, tt.want)
			}
		})
	}
}

func TestBodySeverityPrioritizesP1(t *testing.T) {
	t.Parallel()

	got := BodySeverity("[P2] Minor concern.\n- P1 BADGE: Blocking concern.")
	if got != "P1" {
		t.Fatalf("BodySeverity() = %q, want P1", got)
	}
}
