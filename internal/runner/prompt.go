package runner

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/lessons"
	"github.com/digitaldrywood/detent/internal/skills"
)

const DefaultPromptTemplate = `You are working on a Linear issue.

Identifier: {{ issue.identifier }}
Title: {{ issue.title }}

Body:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}
`

var (
	templateVariablePattern = regexp.MustCompile(`{{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*}}`)
	conditionalTagPattern   = regexp.MustCompile(`{%\s*(if\s+([A-Za-z_][A-Za-z0-9_.]*)|else|endif)\s*%}`)
	windowsAbsPathPattern   = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
)

type PromptOptions struct {
	Attempt         *int
	WorkspacePath   string
	AutoBranch      *bool
	AvailableSkills []skills.Skill
}

func BuildPrompt(workflow config.Workflow, issue connector.Issue, opts PromptOptions) (string, error) {
	template := workflow.Prompt
	if strings.TrimSpace(template) == "" {
		template = DefaultPromptTemplate
	}

	assigns := promptAssigns(workflow.Config, issue, opts)
	rendered, err := renderTemplate(template, assigns)
	if err != nil {
		return "", err
	}

	rendered, err = appendLessonsBlock(rendered, workflow.Config.Agent.Lessons, opts.WorkspacePath)
	if err != nil {
		return "", err
	}

	rendered = appendGateBlock(rendered, workflow.Config.Gate)
	rendered = appendAvailableSkills(rendered, AvailableSkillsBlock(opts.AvailableSkills))
	return appendClosingReferenceInstruction(rendered, issue), nil
}

func AvailableSkillsBlock(skillList []skills.Skill) string {
	if len(skillList) == 0 {
		return ""
	}

	lines := make([]string, 0, len(skillList))
	for _, skill := range skillList {
		lines = append(lines, "- "+skill.Name+" — "+skill.WhenToUse)
	}

	return "## Available skills\n\n" + strings.Join(lines, "\n")
}

func appendAvailableSkills(prompt string, skillsBlock string) string {
	if strings.TrimSpace(skillsBlock) == "" {
		return prompt
	}

	return strings.TrimRight(prompt, " \t\r\n") + "\n\n" + skillsBlock
}

func appendGateBlock(prompt string, cfg gate.Config) string {
	instructions := gate.Instructions(cfg)
	if strings.TrimSpace(instructions) == "" {
		return prompt
	}
	return strings.TrimRight(prompt, " \t\r\n") + "\n\n## Validation gate\n\n" + instructions
}

func appendClosingReferenceInstruction(prompt string, issue connector.Issue) string {
	number := githubIssueNumber(issue.Identifier)
	if number == "" {
		return prompt
	}

	reference := "Fixes #" + number
	if strings.Contains(prompt, reference) ||
		strings.Contains(prompt, "Closes #"+number) ||
		strings.Contains(prompt, "Resolves #"+number) {
		return prompt
	}

	return strings.TrimRight(prompt, " \t\r\n") +
		"\n\n## Pull request\n\nWhen creating or updating the pull request body, include `" +
		reference + "`."
}

func githubIssueNumber(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index == -1 || index == len(identifier)-1 {
		return ""
	}

	number := identifier[index+1:]
	for _, r := range number {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return number
}

func appendLessonsBlock(prompt string, cfg config.Lessons, workspacePath string) (string, error) {
	if !cfg.Enabled || strings.TrimSpace(workspacePath) == "" || cfg.RecallN <= 0 {
		return prompt, nil
	}

	path := cfg.Path
	if strings.TrimSpace(path) == "" {
		path = lessons.DefaultPath
	}

	lessonsPath, err := promptWorkspaceRelativePath(workspacePath, path)
	if err != nil {
		return "", err
	}

	entries, err := lessons.Recent(lessonsPath, cfg.RecallN)
	if err != nil || len(entries) == 0 {
		return prompt, nil
	}

	return strings.TrimRight(prompt, " \t\r\n") + "\n\n## Lessons from prior runs\n\n" + strings.Join(entries, "\n\n"), nil
}

func promptAssigns(cfg config.Config, issue connector.Issue, opts PromptOptions) map[string]any {
	var attempt any
	if opts.Attempt != nil {
		attempt = *opts.Attempt
	}

	autoBranch := cfg.Workspace.AutoBranch
	if opts.AutoBranch != nil {
		autoBranch = *opts.AutoBranch
	}

	return map[string]any{
		"attempt": attempt,
		"issue":   issueAssigns(issue),
		"tracker": map[string]any{
			"kind":         cfg.Tracker.Kind,
			"endpoint":     cfg.Tracker.Endpoint,
			"project_slug": cfg.Tracker.ProjectSlug,
		},
		"workspace": map[string]any{
			"auto_branch": autoBranch,
		},
		"gate": gateAssigns(cfg.Gate),
	}
}

func gateAssigns(cfg gate.Config) map[string]any {
	effective := gate.Effective(cfg)
	return map[string]any{
		"kind":           effective.Kind,
		"run":            effective.Run,
		"approval_label": effective.ApprovalLabel,
	}
}

func issueAssigns(issue connector.Issue) map[string]any {
	return map[string]any{
		"id":                 issue.ID,
		"identifier":         issue.Identifier,
		"title":              issue.Title,
		"description":        issue.Description,
		"priority":           intPointerValue(issue.Priority),
		"state":              issue.State,
		"branch_name":        issue.BranchName,
		"url":                issue.URL,
		"pr_number":          intPointerValue(issue.PRNumber),
		"pull_request":       pullRequestAssigns(issue.PullRequest),
		"author_id":          issue.AuthorID,
		"assignee_id":        issue.AssigneeID,
		"assignees":          issue.Assignees,
		"blocked_by":         issue.BlockedBy,
		"labels":             issue.Labels,
		"fields":             issue.Fields,
		"assigned_to_worker": issue.AssignedToWorker,
		"created_at":         timePointerValue(issue.CreatedAt),
		"updated_at":         timePointerValue(issue.UpdatedAt),
		"model_override":     issue.ModelOverride,
	}
}

func pullRequestAssigns(pullRequest *connector.PullRequest) map[string]any {
	if pullRequest == nil {
		return nil
	}
	return map[string]any{
		"number":                pullRequest.Number,
		"url":                   pullRequest.URL,
		"branch_name":           pullRequest.BranchName,
		"state":                 pullRequest.State,
		"ci_status":             pullRequest.CIStatus,
		"codex_review_state":    pullRequest.CodexReviewState,
		"head_repository":       pullRequest.HeadRepository,
		"base_repository":       pullRequest.BaseRepository,
		"maintainer_can_modify": pullRequest.MaintainerCanModify,
	}
}

func intPointerValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func timePointerValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339)
}

func renderTemplate(template string, assigns map[string]any) (string, error) {
	rendered, err := renderConditionals(template, assigns)
	if err != nil {
		return "", err
	}

	var renderErr error
	rendered = templateVariablePattern.ReplaceAllStringFunc(rendered, func(match string) string {
		if renderErr != nil {
			return match
		}
		parts := templateVariablePattern.FindStringSubmatch(match)
		value, ok := lookupAssign(assigns, parts[1])
		if !ok {
			renderErr = fmt.Errorf("unknown template variable %q", parts[1])
			return match
		}
		return formatTemplateValue(value)
	})
	if renderErr != nil {
		return "", renderErr
	}

	return rendered, nil
}

func renderConditionals(template string, assigns map[string]any) (string, error) {
	var out strings.Builder
	offset := 0

	for offset < len(template) {
		tag := findConditionalTag(template, offset)
		if tag == nil {
			out.WriteString(template[offset:])
			break
		}
		if tag.kind != "if" {
			return "", fmt.Errorf("unexpected template tag %q", tag.kind)
		}

		out.WriteString(template[offset:tag.start])

		name := tag.expr
		value, ok := lookupAssign(assigns, name)
		if !ok {
			return "", fmt.Errorf("unknown template variable %q", name)
		}

		thenBranch, elseBranch, nextOffset, err := splitConditional(template, tag.end)
		if err != nil {
			return "", err
		}

		selected := thenBranch
		if !truthy(value) {
			selected = elseBranch
		}

		rendered, err := renderConditionals(selected, assigns)
		if err != nil {
			return "", err
		}
		out.WriteString(rendered)
		offset = nextOffset
	}

	return out.String(), nil
}

type conditionalTag struct {
	start int
	end   int
	kind  string
	expr  string
}

func findConditionalTag(template string, offset int) *conditionalTag {
	matches := conditionalTagPattern.FindStringSubmatchIndex(template[offset:])
	if matches == nil {
		return nil
	}

	start := offset + matches[0]
	end := offset + matches[1]
	kind := template[offset+matches[2] : offset+matches[3]]
	expr := ""
	if matches[4] >= 0 {
		kind = "if"
		expr = template[offset+matches[4] : offset+matches[5]]
	}

	return &conditionalTag{
		start: start,
		end:   end,
		kind:  kind,
		expr:  expr,
	}
}

func splitConditional(template string, offset int) (string, string, int, error) {
	depth := 1
	thenStart := offset
	elseTagStart := -1
	elseStart := -1
	searchOffset := offset

	for searchOffset < len(template) {
		tag := findConditionalTag(template, searchOffset)
		if tag == nil {
			return "", "", 0, errors.New("missing endif template tag")
		}

		switch tag.kind {
		case "if":
			depth++
		case "else":
			if depth == 1 && elseStart < 0 {
				elseTagStart = tag.start
				elseStart = tag.end
			}
		case "endif":
			depth--
			if depth == 0 {
				if elseStart >= 0 {
					return template[thenStart:elseTagStart], template[elseStart:tag.start], tag.end, nil
				}
				return template[thenStart:tag.start], "", tag.end, nil
			}
		}

		searchOffset = tag.end
	}

	return "", "", 0, errors.New("missing endif template tag")
}

func lookupAssign(assigns map[string]any, name string) (any, bool) {
	parts := strings.Split(name, ".")
	var current any = assigns
	for _, part := range parts {
		switch values := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = values[part]
			if !ok {
				return nil, false
			}
		case map[string]string:
			value, ok := values[part]
			if !ok {
				return nil, false
			}
			current = value
		default:
			return nil, false
		}
	}
	return current, true
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return strings.TrimSpace(v) != ""
	case int:
		return v != 0
	case []string:
		return len(v) > 0
	default:
		return true
	}
}

func formatTemplateValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case []string:
		return strings.Join(v, ", ")
	default:
		return fmt.Sprint(v)
	}
}

func promptWorkspaceRelativePath(workspacePath string, relativePath string) (string, error) {
	workspace := strings.TrimSpace(workspacePath)
	if workspace == "" {
		return "", errors.New("workspace path is required")
	}

	path := strings.TrimSpace(relativePath)
	if path == "" || strings.HasPrefix(path, "~") || filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\`) || windowsAbsPathPattern.MatchString(path) ||
		pathEscapesWorkspace(path) {
		return "", errors.New("path must be a relative path inside the workspace")
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("absolute workspace path: %w", err)
	}

	return filepath.Join(absWorkspace, filepath.Clean(path)), nil
}

func pathEscapesWorkspace(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}
