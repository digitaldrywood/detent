package cli

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	doctorWorkflowDefaultTokenThreshold   = 4000
	doctorWorkflowEstimatedCharsPerToken  = 4
	doctorWorkflowMinimumSectionTokens    = 80
	doctorWorkflowMaximumSkillSuggestions = 5
)

type doctorWorkflowSkillSuggestion struct {
	Heading         string `json:"heading"`
	SkillName       string `json:"skill_name"`
	Path            string `json:"path"`
	EstimatedTokens int    `json:"estimated_tokens"`
}

type doctorWorkflowPromptSection struct {
	heading string
	content string
	index   int
}

var doctorWorkflowConditionalHeadingTerms = []string{
	"api",
	"browser",
	"ci",
	"cleanup",
	"concurrency",
	"database",
	"deploy",
	"docker",
	"frontend",
	"github",
	"git",
	"google",
	"isolation",
	"kubernetes",
	"linux",
	"macos",
	"merge",
	"migration",
	"observability",
	"performance",
	"process lifecycle",
	"profiling",
	"recovery",
	"release",
	"rollout",
	"security",
	"slack",
	"sql",
	"subprocess",
	"terraform",
	"tracker",
	"ui",
	"windows",
	"worktree",
}

var doctorWorkflowConditionalBodyTerms = []string{
	".github/",
	"api endpoint",
	"database",
	"docker",
	"gh ",
	"git ",
	"go test",
	"graphql",
	"http://",
	"https://",
	"kubectl",
	"kubernetes",
	"make ",
	"migration",
	"npm ",
	"pull request",
	"sql",
	"terraform",
	"worktree",
}

var doctorWorkflowCoreHeadings = []string{
	"acceptance criteria",
	"general instructions",
	"operating rules",
	"overview",
	"problem",
	"required execution flow",
	"scope",
	"validation gate",
	"workflow states",
	"workpad status contract",
}

func checkDoctorWorkflowPromptSize(projectID string, workflowPath string, prompt string, skillsPath string, threshold int) (doctorCheck, bool) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return doctorCheck{}, false
	}
	threshold = doctorWorkflowTokenThreshold(threshold)
	characters := utf8.RuneCountInString(prompt)
	estimatedTokens := doctorWorkflowEstimatedTokens(prompt)
	if estimatedTokens <= threshold {
		return doctorCheck{}, false
	}

	skillsPath = doctorWorkflowSkillsPath(skillsPath)
	suggestions := doctorWorkflowPromptSkillSuggestions(prompt, skillsPath)
	detail := fmt.Sprintf("%s prompt body is approximately %d tokens (%d characters at %d characters/token), above the %d-token threshold", workflowPath, estimatedTokens, characters, doctorWorkflowEstimatedCharsPerToken, threshold)
	if len(suggestions) > 0 {
		parts := make([]string, 0, len(suggestions))
		for _, suggestion := range suggestions {
			parts = append(parts, fmt.Sprintf("%q (~%d tokens) -> %s", suggestion.Heading, suggestion.EstimatedTokens, suggestion.Path))
		}
		detail += "; extraction candidates: " + strings.Join(parts, "; ")
	}

	return doctorCheck{
		Name:                     "Project " + projectID + " workflow lint prompt size",
		Status:                   doctorWarn,
		Detail:                   detail,
		Hint:                     fmt.Sprintf("Keep WORKFLOW.md as a thin core and move domain-specific or conditional guidance into reviewed lazy skills under %s through the existing skill-creation flow. This check is guidance only and never rewrites WORKFLOW.md.", skillsPath),
		WorkflowSkillSuggestions: suggestions,
	}, true
}

func doctorWorkflowTokenThreshold(threshold int) int {
	if threshold <= 0 {
		return doctorWorkflowDefaultTokenThreshold
	}
	return threshold
}

func doctorWorkflowEstimatedTokens(content string) int {
	characters := utf8.RuneCountInString(strings.TrimSpace(content))
	return (characters + doctorWorkflowEstimatedCharsPerToken - 1) / doctorWorkflowEstimatedCharsPerToken
}

func doctorWorkflowPromptSkillSuggestions(prompt string, skillsPath string) []doctorWorkflowSkillSuggestion {
	sections := doctorWorkflowPromptSections(prompt)
	candidates := make([]doctorWorkflowPromptSection, 0, len(sections))
	for _, section := range sections {
		if doctorWorkflowCoreHeading(section.heading) || doctorWorkflowEstimatedTokens(section.heading+"\n"+section.content) < doctorWorkflowMinimumSectionTokens {
			continue
		}
		if doctorWorkflowConditionalHeading(section.heading) || doctorWorkflowConditionalBody(section.content) {
			candidates = append(candidates, section)
		}
	}
	if len(candidates) == 0 {
		for _, section := range sections {
			if !doctorWorkflowCoreHeading(section.heading) && doctorWorkflowEstimatedTokens(section.heading+"\n"+section.content) >= doctorWorkflowMinimumSectionTokens {
				candidates = append(candidates, section)
			}
		}
	}

	sort.SliceStable(candidates, func(i int, j int) bool {
		left := doctorWorkflowEstimatedTokens(candidates[i].heading + "\n" + candidates[i].content)
		right := doctorWorkflowEstimatedTokens(candidates[j].heading + "\n" + candidates[j].content)
		if left != right {
			return left > right
		}
		return candidates[i].index < candidates[j].index
	})
	if len(candidates) > doctorWorkflowMaximumSkillSuggestions {
		candidates = candidates[:doctorWorkflowMaximumSkillSuggestions]
	}

	suggestions := make([]doctorWorkflowSkillSuggestion, 0, len(candidates))
	for _, section := range candidates {
		skillName := doctorWorkflowSkillName(section.heading)
		suggestions = append(suggestions, doctorWorkflowSkillSuggestion{
			Heading:         section.heading,
			SkillName:       skillName,
			Path:            doctorWorkflowSkillCandidatePath(skillsPath, skillName),
			EstimatedTokens: doctorWorkflowEstimatedTokens(section.heading + "\n" + section.content),
		})
	}
	return suggestions
}

func doctorWorkflowSkillsPath(skillsPath string) string {
	skillsPath = strings.TrimRight(filepath.ToSlash(strings.TrimSpace(skillsPath)), "/")
	if skillsPath == "" {
		return ".detent/skills"
	}
	return skillsPath
}

func doctorWorkflowSkillCandidatePath(skillsPath string, skillName string) string {
	return doctorWorkflowSkillsPath(skillsPath) + "/" + skillName + ".md"
}

func doctorWorkflowPromptSections(prompt string) []doctorWorkflowPromptSection {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	sections := make([]doctorWorkflowPromptSection, 0)
	var heading string
	var content strings.Builder
	inFence := false
	sectionIndex := 0
	flush := func() {
		if heading == "" {
			return
		}
		sections = append(sections, doctorWorkflowPromptSection{
			heading: heading,
			content: strings.TrimSpace(content.String()),
			index:   sectionIndex,
		})
		sectionIndex++
		content.Reset()
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if nextHeading, ok := doctorWorkflowMarkdownHeading(line); ok {
				flush()
				heading = nextHeading
				continue
			}
		}
		if heading != "" {
			content.WriteString(line)
			content.WriteByte('\n')
		}
	}
	flush()
	return sections
}

func doctorWorkflowMarkdownHeading(line string) (string, bool) {
	line = strings.TrimSpace(line)
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level < 2 || level > 4 || len(line) <= level || line[level] != ' ' {
		return "", false
	}
	heading := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(line[level+1:]), "#"))
	return heading, heading != ""
}

func doctorWorkflowCoreHeading(heading string) bool {
	heading = strings.ToLower(strings.TrimSpace(heading))
	for _, core := range doctorWorkflowCoreHeadings {
		if heading == core {
			return true
		}
	}
	return false
}

func doctorWorkflowConditionalHeading(heading string) bool {
	heading = strings.ToLower(strings.TrimSpace(heading))
	words := strings.FieldsFunc(heading, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, term := range doctorWorkflowConditionalHeadingTerms {
		if strings.ContainsRune(term, ' ') {
			if strings.Contains(heading, term) {
				return true
			}
			continue
		}
		for _, word := range words {
			if word == term {
				return true
			}
		}
	}
	return false
}

func doctorWorkflowConditionalBody(content string) bool {
	content = strings.ToLower(content)
	matches := 0
	for _, term := range doctorWorkflowConditionalBodyTerms {
		if strings.Contains(content, term) {
			matches++
			if matches >= 2 {
				return true
			}
		}
	}
	return false
}

func doctorWorkflowSkillName(heading string) string {
	var builder strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			builder.WriteRune(r)
			separator = false
			continue
		}
		separator = true
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		return "workflow-guidance"
	}
	return name
}
