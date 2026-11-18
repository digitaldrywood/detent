package workspacehub

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestTerminalIsolationAllowed(t *testing.T) {
	t.Parallel()
	terminal := []string{workspacesession.CapabilityFiles, workspacesession.CapabilityTerminal}
	files := []string{workspacesession.CapabilityFiles}
	const (
		user      = workspacesession.IsolationUser
		container = workspacesession.IsolationContainer
	)
	for _, test := range []struct {
		name       string
		requires   []string
		configured string
		reported   string
		want       bool
	}{
		{name: "no terminal, no isolation", requires: files, want: true},
		{name: "no terminal, user runner in container organization", requires: files, configured: container, reported: user, want: true},
		{name: "container runner, container organization", requires: terminal, configured: container, reported: container, want: true},
		{name: "container runner, user organization", requires: terminal, configured: user, reported: container, want: true},
		{name: "user runner, user organization", requires: terminal, configured: user, reported: user, want: true},
		{name: "user runner, container organization", requires: terminal, configured: container, reported: user},
		{name: "user runner, unset organization", requires: terminal, reported: user},
		{name: "runner reporting no isolation", requires: terminal, configured: user},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := TerminalIsolationAllowed(test.requires, test.configured, test.reported); got != test.want {
				t.Fatalf("TerminalIsolationAllowed(%v, %q, %q) = %v, want %v", test.requires, test.configured, test.reported, got, test.want)
			}
		})
	}
}
