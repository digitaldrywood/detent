package accessstore_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
)

func TestDemoted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		from, to string
		want     bool
	}{
		{"owner", "member", true},
		{"admin", "member", true},
		{"admin", "viewer", true},
		{"owner", "reporter", true},
		{"owner", "admin", false},
		{"admin", "owner", false},
		{"member", "admin", false},
		{"member", "viewer", false},
		{"admin", "admin", false},
	} {
		if got := accessstore.Demoted(test.from, test.to); got != test.want {
			t.Errorf("Demoted(%q, %q) = %t, want %t", test.from, test.to, got, test.want)
		}
	}
}
