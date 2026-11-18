package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	WorkflowDefaultTokenThreshold   = 4000
	workflowEstimatedCharsPerToken  = 4
	workflowMinimumSectionTokens    = 80
	workflowMaximumSkillSuggestions = 5
)

type WorkflowSkillSuggestion struct {
	Heading         string `json:"heading"`
	SkillName       string `json:"skill_name"`
	Path            string `json:"path"`
	EstimatedTokens int    `json:"estimated_tokens"`
}

type workflowPromptSection struct {
	heading string
	content string
	index   int
}

var workflowConditionalHeadingTerms = []string{
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

var workflowConditionalBodyTerms = []string{
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

var workflowCoreHeadings = []string{
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

func checkWorkflowPromptSize(projectID string, workflowPath string, prompt string, skillsPath string, threshold int) (Check, bool) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Check{}, false
	}
	threshold = workflowTokenThreshold(threshold)
	characters := utf8.RuneCountInString(prompt)
	estimatedTokens := workflowEstimatedTokens(prompt)
	if estimatedTokens <= threshold {
		return Check{}, false
	}

	skillsPath = workflowSkillsPath(skillsPath)
	suggestions := workflowPromptSkillSuggestions(prompt, skillsPath)
	detail := fmt.Sprintf("%s prompt body is approximately %d tokens (%d characters at %d characters/token), above the %d-token threshold", workflowPath, estimatedTokens, characters, workflowEstimatedCharsPerToken, threshold)
	if len(suggestions) > 0 {
		parts := make([]string, 0, len(suggestions))
		for _, suggestion := range suggestions {
			parts = append(parts, fmt.Sprintf("%q (~%d tokens) -> %s", suggestion.Heading, suggestion.EstimatedTokens, suggestion.Path))
		}
		detail += "; extraction candidates: " + strings.Join(parts, "; ")
	}

	return Check{
		Name:                     "Project " + projectID + " workflow lint prompt size",
		Status:                   Warn,
		Detail:                   detail,
		Hint:                     fmt.Sprintf("Keep WORKFLOW.md as a thin core and move domain-specific or conditional guidance into reviewed lazy skills under %s through the existing skill-creation flow. This check is guidance only and never rewrites WORKFLOW.md.", skillsPath),
		WorkflowSkillSuggestions: suggestions,
	}, true
}

func workflowTokenThreshold(threshold int) int {
	if threshold <= 0 {
		return WorkflowDefaultTokenThreshold
	}
	return threshold
}

func workflowEstimatedTokens(content string) int {
	characters := utf8.RuneCountInString(strings.TrimSpace(content))
	return (characters + workflowEstimatedCharsPerToken - 1) / workflowEstimatedCharsPerToken
}

func workflowPromptSkillSuggestions(prompt string, skillsPath string) []WorkflowSkillSuggestion {
	sections := workflowPromptSections(prompt)
	candidates := make([]workflowPromptSection, 0, len(sections))
	for _, section := range sections {
		if workflowCoreHeading(section.heading) || workflowEstimatedTokens(section.heading+"\n"+section.content) < workflowMinimumSectionTokens {
			continue
		}
		if workflowConditionalHeading(section.heading) || workflowConditionalBody(section.content) {
			candidates = append(candidates, section)
		}
	}
	if len(candidates) == 0 {
		for _, section := range sections {
			if !workflowCoreHeading(section.heading) && workflowEstimatedTokens(section.heading+"\n"+section.content) >= workflowMinimumSectionTokens {
				candidates = append(candidates, section)
			}
		}
	}

	sort.SliceStable(candidates, func(i int, j int) bool {
		left := workflowEstimatedTokens(candidates[i].heading + "\n" + candidates[i].content)
		right := workflowEstimatedTokens(candidates[j].heading + "\n" + candidates[j].content)
		if left != right {
			return left > right
		}
		return candidates[i].index < candidates[j].index
	})
	if len(candidates) > workflowMaximumSkillSuggestions {
		candidates = candidates[:workflowMaximumSkillSuggestions]
	}

	suggestions := make([]WorkflowSkillSuggestion, 0, len(candidates))
	for _, section := range candidates {
		skillName := workflowSkillName(section.heading)
		suggestions = append(suggestions, WorkflowSkillSuggestion{
			Heading:         section.heading,
			SkillName:       skillName,
			Path:            workflowSkillCandidatePath(skillsPath, skillName),
			EstimatedTokens: workflowEstimatedTokens(section.heading + "\n" + section.content),
		})
	}
	return suggestions
}

func workflowSkillsPath(skillsPath string) string {
	skillsPath = strings.TrimRight(filepath.ToSlash(strings.TrimSpace(skillsPath)), "/")
	if skillsPath == "" {
		return ".detent/skills"
	}
	return skillsPath
}

func workflowSkillCandidatePath(skillsPath string, skillName string) string {
	return workflowSkillsPath(skillsPath) + "/" + skillName + ".md"
}

func workflowPromptSections(prompt string) []workflowPromptSection {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	sections := make([]workflowPromptSection, 0)
	var heading string
	var content strings.Builder
	inFence := false
	sectionIndex := 0
	flush := func() {
		if heading == "" {
			return
		}
		sections = append(sections, workflowPromptSection{
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
			if nextHeading, ok := workflowMarkdownHeading(line); ok {
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

func workflowMarkdownHeading(line string) (string, bool) {
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

func workflowCoreHeading(heading string) bool {
	heading = strings.ToLower(strings.TrimSpace(heading))
	for _, core := range workflowCoreHeadings {
		if heading == core {
			return true
		}
	}
	return false
}

func workflowConditionalHeading(heading string) bool {
	heading = strings.ToLower(strings.TrimSpace(heading))
	words := strings.FieldsFunc(heading, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, term := range workflowConditionalHeadingTerms {
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

func workflowConditionalBody(content string) bool {
	content = strings.ToLower(content)
	matches := 0
	for _, term := range workflowConditionalBodyTerms {
		if strings.Contains(content, term) {
			matches++
			if matches >= 2 {
				return true
			}
		}
	}
	return false
}

func workflowSkillName(heading string) string {
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
