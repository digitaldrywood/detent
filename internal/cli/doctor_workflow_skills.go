package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

var workflowSkillMention = regexp.MustCompile(`\$[a-z][a-z0-9_-]*`)

// Codex interprets a skill sigil in a user prompt as an invocation. Report
// potential invocations at their source lines before they reach workers.
func checkDoctorWorkflowSkillMentions(projectID string, definition workflowconfig.ProjectDefinition) []doctorCheck {
	paths := []string{definition.WorkflowPath, definition.LocalWorkflowPath}
	var checks []doctorCheck
	for _, path := range paths {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // The workflow load check reports unreadable files.
		}
		lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
		inFrontmatter := len(lines) > 0 && strings.TrimSpace(lines[0]) == "---"
		for index, line := range lines {
			if inFrontmatter {
				if index > 0 && strings.TrimSpace(line) == "---" {
					inFrontmatter = false
				}
				continue
			}
			for _, mention := range workflowSkillMention.FindAllString(line, -1) {
				checks = append(checks, doctorCheck{
					Name:   "Project " + projectID + " workflow lint skill invocation",
					Status: doctorWarn,
					Detail: fmt.Sprintf("%s:%d: %s in the worker prompt may auto-attach an installed Codex skill to every session", path, index+1, mention),
					Hint:   "Spell the skill name without the $ sigil when documenting it in WORKFLOW.md.",
				})
			}
		}
	}
	return checks
}
