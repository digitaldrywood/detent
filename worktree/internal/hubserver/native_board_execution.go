package hubserver

func nativeBoardExecutionLabel(stage string) string {
	switch stage {
	case "implementation", "implementing", "code":
		return "Implementing"
	case "rework", "reworking":
		return "Reworking"
	case "planning", "plan":
		return "Planning"
	case "validation", "validating", "review", "security_audit":
		return "Validating"
	case "merging", "merge":
		return "Merging"
	case "finalizing":
		return "Finalizing"
	default:
		return ""
	}
}
