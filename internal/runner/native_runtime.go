package runner

func nativeRuntimePhase(req RunRequest) string {
	switch req.Mode {
	case RunModePlan:
		return "planning"
	case RunModeMerge:
		return "merging"
	}
	if runRole(req.Mode, req.Issue) == RoleRework {
		return "rework"
	}
	return "implementation"
}
