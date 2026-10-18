package github

import (
	"slices"
	"testing"
)

func TestDependencyDeclarationSentenceBoundary(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"Depends on: ", "Blocked by: "} {
		for _, tt := range []struct {
			text string
			want []string
		}{
			{text: "none. Order: #2, #3 (#2 → #3)."},
			{text: "N/A, see #9"},
			{text: "- #9"},
			{text: "#2, #3.", want: []string{"owner/repo#2", "owner/repo#3"}},
			{text: "#2 and #3 — see #9 for context", want: []string{"owner/repo#2", "owner/repo#3"}},
			{text: "#2\nOrder: #9", want: []string{"owner/repo#2"}},
			{text: "https://github.com/owner/repo.name/issues/2. See #9", want: []string{"owner/repo.name#2"}},
			{text: "[#2](https://github.com/owner/repo/issues/2), #3. See #9", want: []string{"owner/repo#2", "owner/repo#3"}},
		} {
			t.Run(label+tt.text, func(t *testing.T) {
				t.Parallel()
				var got []string
				for _, ref := range parseBlockedBy(label+tt.text, "owner/repo") {
					got = append(got, ref.Identifier)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("dependencies = %v, want %v", got, tt.want)
				}
			})
		}
	}
}
