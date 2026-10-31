package cli

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorGitHubKeyring(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, via, status string
		want              doctorStatus
	}{
		{"keyring", "gh", "Logged in to github.com account worker (keyring)", doctorWarn},
		{"file", "gh", "Logged in to github.com account worker (/config/gh/hosts.yml)", doctorOK},
		{"environment", "", "", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := checkDoctorGitHub(context.Background(), nil, RuntimeSecret{Value: "test-token", ResolvedVia: tt.via}, doctorDeps{
				githubScopes: func(context.Context, string) ([]string, error) { return nil, nil },
				ghAuthStatus: func(context.Context) (string, error) { return tt.status, nil },
			})
			if check.Status != tt.want {
				t.Fatalf("check = %#v", check)
			}
			if tt.want == doctorWarn && !strings.Contains(check.Hint, "GH_TOKEN") {
				t.Fatalf("missing service remedy: %#v", check)
			}
		})
	}
}
