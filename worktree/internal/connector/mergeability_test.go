package connector

import "testing"

func TestPullRequestConflicts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		state string
		want  bool
	}{
		{"dirty", true}, {"conflicting", true}, {" DIRTY ", true}, {" ConFLicting ", true},
		{"", false}, {"unknown", false}, {"draft", false}, {"clean", false}, {"blocked", false},
	} {
		t.Run(tt.state, func(t *testing.T) {
			if got := PullRequestConflicts(tt.state); got != tt.want {
				t.Fatalf("conflicts = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPullRequestConflictCleared(t *testing.T) {
	t.Parallel()
	for _, before := range []string{"dirty", " CONFLICTING ", "clean", "unknown", ""} {
		for _, after := range []string{"dirty", " conflicting ", "clean", "draft", "unknown", "unstable", "has_hooks", "behind", "blocked", "mergeable", "unrecognized", "", " "} {
			t.Run(before+"/"+after, func(t *testing.T) {
				want := (before == "dirty" || before == " CONFLICTING ") && after != "dirty" && after != " conflicting " && after != "" && after != " " && after != "unrecognized"
				if got := PullRequestConflictCleared(before, after); got != want {
					t.Fatalf("cleared = %t, want %t", got, want)
				}
			})
		}
	}
}
