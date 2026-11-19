package workspacehub

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestClaimableChecksRunnerFacts(t *testing.T) {
	t.Parallel()
	filesAndDiff := workspacesession.Capabilities{Files: true, Diff: true}
	terminal := workspacesession.Capabilities{Files: true, Diff: true, Terminal: true}
	for _, test := range []struct {
		name                            string
		requires                        []string
		capabilities                    workspacesession.Capabilities
		fresh                           bool
		retained, isolation, configured string
		want                            bool
		reason                          string
	}{
		{name: "stale report", capabilities: filesAndDiff, reason: "the runner has no fresh workspace capability report"},
		{name: "missing diff", requires: []string{"files", "diff"}, capabilities: workspacesession.Capabilities{Files: true}, fresh: true, reason: "the runner does not serve every required surface"},
		{name: "foreign retained checkout", capabilities: filesAndDiff, fresh: true, retained: "runner_two", reason: "the attempt's retained worktree belongs to another runner"},
		{name: "same retained checkout", capabilities: filesAndDiff, fresh: true, retained: "runner_one", want: true},
		{name: "terminal isolation mismatch", requires: []string{"terminal"}, capabilities: terminal, fresh: true, isolation: workspacesession.IsolationUser, configured: workspacesession.IsolationContainer, reason: "the runner's terminal isolation is not allowed by the organization"},
		{name: "container terminal", requires: []string{"terminal"}, capabilities: terminal, fresh: true, isolation: workspacesession.IsolationContainer, configured: workspacesession.IsolationContainer, want: true},
		{name: "user terminal", requires: []string{"terminal"}, capabilities: terminal, fresh: true, isolation: workspacesession.IsolationUser, configured: workspacesession.IsolationUser, want: true},
		{name: "no terminal isolation requirement", requires: []string{"files", "diff"}, capabilities: filesAndDiff, fresh: true, isolation: workspacesession.IsolationUser, configured: workspacesession.IsolationContainer, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, reason := Claimable(Record{Requires: test.requires}, test.capabilities, test.fresh, "runner_one", test.retained, test.isolation, test.configured)
			if got != test.want || reason != test.reason {
				t.Fatalf("claimable=%t reason=%q, want %t %q", got, reason, test.want, test.reason)
			}
		})
	}
}
