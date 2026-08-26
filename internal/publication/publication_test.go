package publication

import "testing"

func TestProtect(t *testing.T) {
	t.Parallel()

	const original = "Private acme/secret#42 and https://github.com/acme/secret/pull/99 in acme/secret and secret from feature/quiet-fix at /srv/acme/secret/worktrees/42 by @private-user. Public public/destination#7 stays. Repeat acme/secret#42."
	privateSource := Source{
		Repository: "acme/secret",
		Workspaces: []string{"/srv/acme/secret/worktrees"},
		Branches:   []string{"feature/quiet-fix"},
		Logins:     []string{"private-user"},
	}
	tests := []struct {
		name   string
		policy Policy
		want   string
	}{
		{
			name: "public destination redacts cross project references consistently",
			policy: Policy{
				DestinationRepository: "public/destination",
				Sources:               []Source{privateSource},
				Visibility:            VisibilityPublic,
			},
			want: "Private project-A#1 and project-A#2 in project-A and project-A from branch-A at <workspace> by @contributor-A. Public public/destination#7 stays. Repeat project-A#1.",
		},
		{
			name: "private destination preserves bytes",
			policy: Policy{
				DestinationRepository: "private/destination",
				Sources:               []Source{privateSource},
				Visibility:            VisibilityPrivate,
			},
			want: original,
		},
		{
			name: "unknown visibility fails closed",
			policy: Policy{
				DestinationRepository: "public/destination",
				Sources:               []Source{privateSource},
				Visibility:            VisibilityUnknown,
			},
			want: "Private project-A#1 and project-A#2 in project-A and project-A from branch-A at <workspace> by @contributor-A. Public public/destination#7 stays. Repeat project-A#1.",
		},
		{
			name: "same repository survives public destination",
			policy: Policy{
				DestinationRepository: "acme/secret",
				Sources:               []Source{privateSource},
				Visibility:            VisibilityPublic,
			},
			want: original,
		},
		{
			name: "opt in preserves bytes",
			policy: Policy{
				DestinationRepository:          "public/destination",
				Sources:                        []Source{privateSource},
				Visibility:                     VisibilityPublic,
				AllowPublicCrossProjectDetails: true,
			},
			want: original,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Protect(original, test.policy); got != test.want {
				t.Fatalf("Protect() = %q, want %q", got, test.want)
			}
		})
	}
}
