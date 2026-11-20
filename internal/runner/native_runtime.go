package runner

import "github.com/digitaldrywood/detent/internal/runner/runworkspace"

func nativeRuntimePhase(req RunRequest) string {
	switch req.Mode {
	case RunModePlan:
		return "planning"
	case RunModeMerge:
		return "merging"
	}
	if runworkspace.RunRole(req.Mode, req.Issue) == RoleRework {
		return "rework"
	}
	return "implementation"
}
