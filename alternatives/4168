package workspacehub

import (
	"slices"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func Claimable(record Record, capabilities workspacesession.Capabilities, fresh bool, runnerID, retainedRunner, runnerIsolation, configuredIsolation string) (bool, string) {
	if !fresh {
		return false, "the runner has no fresh workspace capability report"
	}
	if !capabilities.Satisfies(record.Requires) {
		return false, "the runner does not serve every required surface"
	}
	if !TerminalIsolationAllowed(record.Requires, configuredIsolation, runnerIsolation) {
		return false, "the runner's terminal isolation is not allowed by the organization"
	}
	if retainedRunner != "" && retainedRunner != runnerID {
		return false, "the attempt's retained worktree belongs to another runner"
	}
	return true, ""
}
func TerminalIsolationAllowed(requires []string, configured, reported string) bool {
	if !slices.Contains(requires, workspacesession.CapabilityTerminal) {
		return true
	}
	switch reported {
	case workspacesession.IsolationSandbox:
		return configured == workspacesession.IsolationSandbox || configured == workspacesession.IsolationUser
	case workspacesession.IsolationContainer:
		return true
	case workspacesession.IsolationUser:
		return configured == workspacesession.IsolationUser
	default:
		return false
	}
}
