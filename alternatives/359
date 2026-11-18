package doctor

import (
	"regexp"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

var nativeWorkflowGitHubMarkers = []struct {
	label   string
	pattern *regexp.Regexp
}{
	{label: "Codex Workpad", pattern: regexp.MustCompile(`(?i)codex workpad`)},
	{label: "gh", pattern: regexp.MustCompile(`(?i)(^|[^a-z0-9_-])gh\s+[a-z]`)},
	{label: "pull request", pattern: regexp.MustCompile(`(?i)pull request`)},
	{label: "detent-status", pattern: regexp.MustCompile(`(?i)detent-status`)},
	{label: "GitHub API", pattern: regexp.MustCompile(`(?i)github (rest |graphql )?api`)},
}

func NativeWorkflowGitHubSteps(cfg workflowconfig.Config, prompt string) []string {
	if cfg.Tracker.Kind != workflowconfig.TrackerHubNative {
		return nil
	}
	var found []string
	for _, marker := range nativeWorkflowGitHubMarkers {
		if marker.pattern.MatchString(prompt) {
			found = append(found, marker.label)
		}
	}
	return found
}

func NativeWorkflowGitHubStepsWarning(steps []string) string {
	return "WORKFLOW.md references GitHub-only steps (" + strings.Join(steps, ", ") + "); native runs commit on the attempt branch and Detent records the Change Request"
}

func CheckNativeWorkflowInstructions(id string, cfg workflowconfig.Config, prompt string) Check {
	name := "Project " + id + " native workflow instructions"
	steps := NativeWorkflowGitHubSteps(cfg, prompt)
	if len(steps) == 0 {
		return Check{Name: name, Status: OK, Detail: "WORKFLOW.md body references no GitHub-only steps"}
	}
	return Check{
		Name:   name,
		Status: Warn,
		Detail: NativeWorkflowGitHubStepsWarning(steps),
		Hint:   "Remove Workpad comment, push, and pull request steps from the WORKFLOW.md body; the runner prompt's native completion contract overrides them.",
	}
}
