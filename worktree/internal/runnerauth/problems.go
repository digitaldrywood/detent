package runnerauth

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type Problem struct {
	ProjectID   string    `json:"project_id,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Check       string    `json:"check,omitempty"`
	ErrorOutput string    `json:"error_output,omitempty"`
	FixCommand  string    `json:"fix_command,omitempty"`
	ReportedAt  time.Time `json:"reported_at,omitempty"`
	Code        string    `json:"code"`
	Message     string    `json:"message"`
	FixHint     string    `json:"fix_hint"`
	FirstSeen   time.Time `json:"first_seen"`
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
		p.Message = "The runner's project policy differs from the Hub's current approval."
		p.FixHint = "Approve the pending policy in the project's Integrations settings, or update the runner's project files to match the approved policy."
	}
	return p
}

func ValidateReportedProblems(problems []Problem) error {
	if len(problems) > 100 {
		return errors.New("too many runner problems")
	}
	for i, problem := range problems {
		if !slices.Contains([]string{"tier_unavailable", "backend_missing", "host_service_unreachable", "settings_invalid", "keep_awake_failed"}, problem.Code) || slices.ContainsFunc(problems[:i], func(p Problem) bool { return sameProblem(p, problem) }) {
			return errors.New("invalid or duplicate runner problem code")
		}
		for _, detail := range []string{problem.Subject, problem.Check, problem.ErrorOutput, problem.FixCommand} {
			if len(detail) > 2000 || strings.ContainsRune(detail, 0) {
				return errors.New("runner problem details must be bounded")
			}
		}
		if strings.TrimSpace(problem.Message) == "" || strings.TrimSpace(problem.FixHint) == "" || len(problem.ProjectID) > 200 || len(problem.Message) > 1000 || len(problem.FixHint) > 1000 || strings.ContainsAny(problem.ProjectID+problem.Message+problem.FixHint, "\x00") {
			return errors.New("runner problems require a bounded message and fix hint")
		}
	}
	return nil
}

func MergeProblems(previous, current []Problem, now time.Time) []Problem {
	result := make([]Problem, 0, len(current))
	for _, problem := range current {
		problem = SanitizeProblem(problem)
		if slices.ContainsFunc(result, func(p Problem) bool { return sameProblem(p, problem) }) {
			continue
		}
		if problem.ReportedAt.IsZero() {
			problem.ReportedAt = now
		}
		problem.FirstSeen = now
		for _, old := range previous {
			if sameProblem(old, problem) && !old.FirstSeen.IsZero() {
				problem.FirstSeen = old.FirstSeen
				break
			}
		}
		result = append(result, problem)
	}
	slices.SortFunc(result, func(a, b Problem) int {
		return strings.Compare(a.Code+"/"+a.ProjectID+"/"+a.Subject+"/"+a.Check, b.Code+"/"+b.ProjectID+"/"+b.Subject+"/"+b.Check)
	})
	return result
}

func sameProblem(a, b Problem) bool {
	return a.Code == b.Code && a.ProjectID == b.ProjectID && a.Subject == b.Subject && a.Check == b.Check
}

var problemSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	regexp.MustCompile(`(?i)(?:authorization|cookie|set-cookie)\s*:[^\r\n]*`),
	regexp.MustCompile(`(?i)["']?(?:[a-z0-9_]*(?:token|secret|password|api[_-]?key|credential)[a-z0-9_]*)["']?\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)--[a-z_-]*(?:token|secret|password|api-key|credential)[a-z_-]*\s+[^\s]+`),
	regexp.MustCompile(`(?i)bearer\s+[^\s"']+`),
	regexp.MustCompile(`https?://[^\s/@]+:[^\s/@]+@`),
	regexp.MustCompile(`(?:sk-[a-zA-Z0-9_-]+|gh[pousr]_[a-zA-Z0-9_]+|eyJ[a-zA-Z0-9_.=-]+)`),
}

var problemOpaqueSecret = regexp.MustCompile(`[a-zA-Z0-9_./+=-]{32,}`)

func SanitizeProblem(p Problem) Problem {
	p.Subject = boundedProblemText(p.Subject)
	p.Check = boundedProblemText(p.Check)
	p.ErrorOutput = boundedProblemText(problemOpaqueSecret.ReplaceAllString(p.ErrorOutput, "[redacted]"))
	p.FixCommand = boundedProblemText(p.FixCommand)
	p.Message = boundedProblemText(p.Message)
	p.FixHint = boundedProblemText(p.FixHint)
	return p
}

func boundedProblemText(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	for _, pattern := range problemSecrets {
		value = pattern.ReplaceAllString(value, "[redacted]")
	}
	if len(value) > 2000 {
		value = value[:1997]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		value += "..."
	}
	return strings.TrimSpace(value)
}
