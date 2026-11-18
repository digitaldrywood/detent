package remoterunner

import (
	"crypto/rand"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"strings"
	"testing"
)

func TestWorkspaceLaneEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		capabilities workspacesession.Capabilities
		want         bool
	}{
		{"files reported", workspacesession.Capabilities{Files: true}, true},
		{"files with diff reported", workspacesession.Capabilities{Files: true, Diff: true}, true},
		{"nothing reported", workspacesession.Capabilities{}, false},
		// A request without `requires` gets files and diff, so a runner that
		// reports only a terminal would still be refused every claim.
		{"terminal only", workspacesession.Capabilities{Terminal: true}, false},
		{"diff only", workspacesession.Capabilities{Diff: true}, false},
		{"files with exec reported", workspacesession.Capabilities{Files: true, Exec: true}, true},
		{"exec only", workspacesession.Capabilities{Exec: true}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := workspaceLaneEnabled(test.capabilities); got != test.want {
				t.Errorf("workspaceLaneEnabled(%#v) = %t, want %t", test.capabilities, got, test.want)
			}
		})
	}
}

func TestWorkspaceSessionIDsAreUniqueAndDistinct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		generations []string
	}{
		{name: "one generation", generations: []string{"1"}},
		{name: "overlapping generations", generations: []string{"1", "2"}},
		{name: "process generations", generations: []string{rand.Text(), rand.Text(), rand.Text()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seen := map[string]bool{}
			for _, generation := range test.generations {
				next := workspaceSessionIDs("machine_1", generation)
				for range 3 {
					id, err := next()
					if err != nil {
						t.Fatalf("workspaceSessionIDs() error = %v", err)
					}
					if seen[id] {
						t.Fatalf("session id %q was handed out twice", id)
					}
					// The hub pins a policy per lease session, so a workspace claim must
					// never reuse the issue lane's session.
					if !strings.HasPrefix(id, "machine_1-workspace-") {
						t.Fatalf("session id %q does not name the workspace lane", id)
					}
					seen[id] = true
				}
			}
		})
	}
}
