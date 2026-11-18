package labels

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestIsManagedLabel(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		label string
		want  bool
	}{
		{name: "plain", label: "bug"},
		{name: "namespaced but unmanaged", label: "goal:reliability"},
		{name: "priority", label: "priority:high", want: true},
		{name: "priority in another case", label: "Priority:High", want: true},
		{name: "effort", label: "effort:low", want: true},
		{name: "reserved detent", label: "detent:coordinator", want: true},
		{name: "blank", label: "   ", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsManaged(test.label); got != test.want {
				t.Fatalf("isManagedLabel(%q) = %v, want %v", test.label, got, test.want)
			}
		})
	}
}

func TestLabelColorIsStableAndInThePalette(t *testing.T) {
	t.Parallel()
	palette := map[string]bool{}
	for _, colour := range labelPalette {
		palette[colour] = true
	}
	for _, test := range []struct{ name, label, same string }{
		{name: "bug", label: "bug", same: "BUG"},
		{name: "chore", label: "chore", same: " chore "},
		{name: "long name", label: "needs-product-decision", same: "Needs-Product-Decision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			colour := Color(test.label)
			if !palette[colour] {
				t.Fatalf("labelColor(%q) = %q, which is not in the palette", test.label, colour)
			}
			if other := Color(test.same); other != colour {
				t.Fatalf("labelColor(%q) = %q but labelColor(%q) = %q", test.label, colour, test.same, other)
			}
		})
	}
}
