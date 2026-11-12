package hubapi_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestLesserHostedRoleFromEntryAssertion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		asserted, local, want string
	}{
		{"owner", "owner", "owner"},
		{"owner", "viewer", "viewer"},
		{"admin", "member", "member"},
		{"member", "admin", "member"},
		{"viewer", "owner", "viewer"},
	}
	for _, test := range tests {
		if got := hubapi.LesserHostedRole(test.asserted, test.local); got != test.want {
			t.Errorf("lesserHostedRole(%q, %q) = %q, want %q", test.asserted, test.local, got, test.want)
		}
	}
}
