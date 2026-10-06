package runnerauth

import (
	"errors"
	"slices"
	"strings"
	"time"
)

type Problem struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	FixHint   string    `json:"fix_hint"`
	FirstSeen time.Time `json:"first_seen"`
}

func NewProblem(code string) Problem {
	p := Problem{Code: code}
	switch code {
	case "tier_unavailable":
		p.Message = "The configured isolation tier is unavailable."
		p.FixHint = "Install or repair the sandbox tooling and check the backend's sandbox support."
	case "backend_missing":
		p.Message = "A configured agent backend is unavailable."
		p.FixHint = "Install the configured backend and verify its command can run on the runner."
	case "host_service_unreachable":
		p.Message = "A configured host service cannot be reached."
		p.FixHint = "Start the host service and check its configured socket or port."
	case "settings_invalid":
		p.Message = "The runner's local settings are invalid."
		p.FixHint = "Check the local workflow and runner routing settings."
	case "keep_awake_failed":
		p.Message = "The runner could not inhibit host sleep."
		p.FixHint = "Install or repair the host sleep inhibitor and check its permissions."
	case "settings_rejected":
		p.Message = "The runner could not apply the Hub's routing settings."
		p.FixHint = "Check the runner's routing cache permissions and configured host services."
	case "version_unsupported":
		p.Message = "The runner's protocol version is unsupported."
		p.FixHint = "Upgrade the runner to a version compatible with this Hub."
	case "policy_mismatch":
		p.Message = "A selected local project policy differs from the policy currently in use."
		p.FixHint = "Review the selected policy in the project's Integrations settings, approve the intended configuration and apply it through the runner configuration owner."
	}
	return p
}

func ValidateReportedProblems(problems []Problem) error {
	if len(problems) > 5 {
		return errors.New("too many runner problems")
	}
	for i, problem := range problems {
		if !slices.Contains([]string{"tier_unavailable", "backend_missing", "host_service_unreachable", "settings_invalid", "keep_awake_failed"}, problem.Code) || slices.ContainsFunc(problems[:i], func(p Problem) bool { return p.Code == problem.Code }) {
			return errors.New("invalid or duplicate runner problem code")
		}
		if strings.TrimSpace(problem.Message) == "" || strings.TrimSpace(problem.FixHint) == "" || len(problem.Message) > 1000 || len(problem.FixHint) > 1000 || strings.ContainsAny(problem.Message+problem.FixHint, "\x00") {
			return errors.New("runner problems require a bounded message and fix hint")
		}
	}
	return nil
}

func MergeProblems(previous, current []Problem, now time.Time) []Problem {
	result := make([]Problem, 0, len(current))
	for _, problem := range current {
		if slices.ContainsFunc(result, func(p Problem) bool { return p.Code == problem.Code }) {
			continue
		}
		problem.FirstSeen = now
		for _, old := range previous {
			if old.Code == problem.Code && !old.FirstSeen.IsZero() {
				problem.FirstSeen = old.FirstSeen
				break
			}
		}
		result = append(result, problem)
	}
	slices.SortFunc(result, func(a, b Problem) int { return strings.Compare(a.Code, b.Code) })
	return result
}
