package cli

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDoctorDependencyDeclarationBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		body, label string
		wantRefs    int
	}{
		{body: "Depends on: none. Order: #2, #3"},
		{body: "Blocked by: n/a, see #9"},
		{body: "Depends on: - #9"},
		{body: "Depends on: release train", label: "release train"},
		{body: "Depends on: release train #9", label: "release train #9"},
		{body: "Depends on: #2 and #3. See #9", label: "#2 and #3", wantRefs: 2},
	} {
		t.Run(tt.body, func(t *testing.T) {
			t.Parallel()
			labels := doctorDependencyLineReferences(tt.body)
			if tt.label == "" {
				if len(labels) != 0 {
					t.Fatalf("labels = %v, want none", labels)
				}
			} else if len(labels) != 1 || labels[0] != tt.label {
				t.Fatalf("labels = %v, want %q", labels, tt.label)
			}
			refs := doctorDependencyTextBlockedRefs(connector.Issue{Identifier: "owner/repo#1", Description: tt.body})
			if len(refs) != tt.wantRefs {
				t.Fatalf("refs = %v, want %d", refs, tt.wantRefs)
			}
		})
	}
}
