package auth_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
)

func TestAllowlistRequiresVerifiedAllowedEmail(t *testing.T) {
	t.Parallel()

	allowlist, err := auth.NewAllowlist([]string{"operator@example.com"}, []string{"example.org"})
	if err != nil {
		t.Fatalf("NewAllowlist() error = %v", err)
	}
	tests := []struct {
		name     string
		email    string
		verified bool
		want     bool
	}{
		{name: "exact email", email: "Operator@Example.com", verified: true, want: true},
		{name: "allowed domain", email: "someone@example.org", verified: true, want: true},
		{name: "domain suffix is not exact", email: "someone@sub.example.org", verified: true},
		{name: "other email", email: "other@example.net", verified: true},
		{name: "unverified exact email", email: "operator@example.com"},
		{name: "invalid email", email: "not-an-email", verified: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := allowlist.Allows(tt.email, tt.verified); got != tt.want {
				t.Fatalf("Allows(%q, %t) = %t, want %t", tt.email, tt.verified, got, tt.want)
			}
		})
	}
}

func TestNewAllowlistValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		emails  []string
		domains []string
	}{
		{name: "empty"},
		{name: "invalid email", emails: []string{"invalid"}},
		{name: "invalid domain", domains: []string{"@example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := auth.NewAllowlist(tt.emails, tt.domains); err == nil {
				t.Fatal("NewAllowlist() error = nil, want validation error")
			}
		})
	}
}
