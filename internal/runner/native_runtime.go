package runner

func nativeRuntimePhase(req RunRequest) string {
	switch req.Mode {
	case RunModePlan:
		return "planning"
	case RunModeMerge:
		return "merging"
	}
	if req.executionRole() == RoleRework {
		return "rework"
	}
	return "implementation"
}
