package selector

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const meToken = "@me"

type Context struct {
	InstanceLogin string
	Persona       string
}

type Selector struct {
	AssigneeIn []string      `yaml:"assignee_in"`
	AuthorIn   []string      `yaml:"author_in"`
	PriorityIn []int         `yaml:"priority_in"`
	Labels     Labels        `yaml:"labels"`
	Fields     []FieldEquals `yaml:"fields"`
	And        []Selector    `yaml:"and"`
	Or         []Selector    `yaml:"or"`
}

type Labels struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type FieldEquals struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

func Match(issue connector.Issue, selector Selector, ctx Context) bool {
	if !matchIdentityList(issueAssignees(issue), selector.AssigneeIn, ctx) {
		return false
	}
	if !matchIdentityList([]string{issue.AuthorID}, selector.AuthorIn, ctx) {
		return false
	}
	if !matchLabels(issue.Labels, selector.Labels) {
		return false
	}
	if !matchFields(issue.Fields, selector.Fields, ctx) {
		return false
	}
	if !matchPriority(issue.Priority, selector.PriorityIn) {
		return false
	}
	for _, child := range selector.And {
		if !Match(issue, child, ctx) {
			return false
		}
	}
	if len(selector.Or) > 0 {
		for _, child := range selector.Or {
			if Match(issue, child, ctx) {
				return true
			}
		}
		return false
	}
	return true
}

func matchPriority(priority *int, allowed []int) bool {
	if len(allowed) == 0 {
		return true
	}
	if priority == nil {
		return false
	}
	for _, allowedPriority := range allowed {
		if *priority == allowedPriority {
			return true
		}
	}
	return false
}

func issueAssignees(issue connector.Issue) []string {
	assignees := append([]string(nil), issue.Assignees...)
	if strings.TrimSpace(issue.AssigneeID) != "" {
		assignees = append(assignees, issue.AssigneeID)
	}
	return assignees
}

func matchIdentityList(values, allowed []string, ctx Context) bool {
	if len(allowed) == 0 {
		return true
	}

	present := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizeIdentity(value)
		if value == "" {
			continue
		}
		present[value] = struct{}{}
	}

	for _, allowedValue := range allowed {
		for _, resolved := range resolveValue(allowedValue, ctx) {
			if _, ok := present[normalizeIdentity(resolved)]; ok {
				return true
			}
		}
	}
	return false
}

func matchLabels(issueLabels []string, labels Labels) bool {
	present := make(map[string]struct{}, len(issueLabels))
	for _, label := range issueLabels {
		label = normalizeLabel(label)
		if label == "" {
			continue
		}
		present[label] = struct{}{}
	}

	for _, label := range labels.Include {
		if _, ok := present[normalizeLabel(label)]; !ok {
			return false
		}
	}
	for _, label := range labels.Exclude {
		if _, ok := present[normalizeLabel(label)]; ok {
			return false
		}
	}
	return true
}

func matchFields(fields map[string]string, rules []FieldEquals, ctx Context) bool {
	for _, rule := range rules {
		value, ok := fieldValue(fields, rule.Name)
		if !ok {
			return false
		}
		if !matchFieldValue(value, rule.Value, ctx) {
			return false
		}
	}
	return true
}

func fieldValue(fields map[string]string, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if value, ok := fields[name]; ok {
		return value, true
	}
	for fieldName, value := range fields {
		if strings.TrimSpace(fieldName) == name {
			return value, true
		}
	}
	return "", false
}

func matchFieldValue(value, expected string, ctx Context) bool {
	if isMeToken(expected) {
		value = normalizeIdentity(value)
		for _, resolved := range resolveMe(ctx) {
			if value == normalizeIdentity(resolved) {
				return true
			}
		}
		return false
	}
	return strings.TrimSpace(value) == strings.TrimSpace(expected)
}

func resolveValue(value string, ctx Context) []string {
	if isMeToken(value) {
		return resolveMe(ctx)
	}
	return []string{value}
}

func resolveMe(ctx Context) []string {
	values := []string{ctx.InstanceLogin, ctx.Persona}
	resolved := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized := normalizeIdentity(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		resolved = append(resolved, value)
	}
	return resolved
}

func isMeToken(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), meToken)
}

func normalizeIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}
