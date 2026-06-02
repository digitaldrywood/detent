package selector

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const meToken = "@me"

type Context struct {
	InstanceLogin string
	Persona       string
}

type Selector struct {
	AssigneeIn []string      `yaml:"assignee_in,omitempty"`
	AuthorIn   []string      `yaml:"author_in,omitempty"`
	PriorityIn []int         `yaml:"priority_in,omitempty"`
	Labels     Labels        `yaml:"labels,omitempty"`
	Fields     []FieldEquals `yaml:"fields,omitempty"`
	And        []Selector    `yaml:"and,omitempty"`
	Or         []Selector    `yaml:"or,omitempty"`
}

type Labels struct {
	Include []string `yaml:"include,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
}

type FieldEquals struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

func (s Selector) Configured() bool {
	if anyNonBlank(s.AssigneeIn) || anyNonBlank(s.AuthorIn) ||
		len(s.PriorityIn) > 0 || anyNonBlank(s.Labels.Include) ||
		anyNonBlank(s.Labels.Exclude) {
		return true
	}
	for _, field := range s.Fields {
		if strings.TrimSpace(field.Name) != "" || strings.TrimSpace(field.Value) != "" {
			return true
		}
	}
	return len(s.And) > 0 || len(s.Or) > 0
}

func (s Selector) IsZero() bool {
	return !s.Configured()
}

func (s *Selector) Normalize() {
	if s == nil {
		return
	}
	s.AssigneeIn = trimStringSlice(s.AssigneeIn)
	s.AuthorIn = trimStringSlice(s.AuthorIn)
	s.Labels.Include = trimStringSlice(s.Labels.Include)
	s.Labels.Exclude = trimStringSlice(s.Labels.Exclude)
	for index := range s.Fields {
		s.Fields[index].Name = strings.TrimSpace(s.Fields[index].Name)
		s.Fields[index].Value = strings.TrimSpace(s.Fields[index].Value)
	}
	for index := range s.And {
		s.And[index].Normalize()
	}
	for index := range s.Or {
		s.Or[index].Normalize()
	}
}

func (s Selector) Validate(prefix string) []string {
	var problems []string
	problems = append(problems, stringListProblems(prefix+".assignee_in", s.AssigneeIn)...)
	problems = append(problems, stringListProblems(prefix+".author_in", s.AuthorIn)...)
	problems = append(problems, priorityProblems(prefix+".priority_in", s.PriorityIn)...)
	problems = append(problems, stringListProblems(prefix+".labels.include", s.Labels.Include)...)
	problems = append(problems, stringListProblems(prefix+".labels.exclude", s.Labels.Exclude)...)

	for index, field := range s.Fields {
		fieldPrefix := fmt.Sprintf("%s.fields[%d]", prefix, index)
		if strings.TrimSpace(field.Name) == "" {
			problems = append(problems, fieldPrefix+".name must not be blank")
		}
		if strings.TrimSpace(field.Value) == "" {
			problems = append(problems, fieldPrefix+".value must not be blank")
		}
	}
	for index, child := range s.And {
		problems = append(problems, child.Validate(fmt.Sprintf("%s.and[%d]", prefix, index))...)
	}
	for index, child := range s.Or {
		problems = append(problems, child.Validate(fmt.Sprintf("%s.or[%d]", prefix, index))...)
	}
	return problems
}

func Describe(selector Selector, ctx Context) string {
	parts := describeSelectorParts(selector, ctx)
	if len(parts) == 0 {
		return "All issues"
	}
	return strings.Join(parts, "; ")
}

func describeSelectorParts(selector Selector, ctx Context) []string {
	parts := []string{}
	if part := describeIdentityRule("assignee in", selector.AssigneeIn, ctx); part != "" {
		parts = append(parts, part)
	}
	if part := describeIdentityRule("author in", selector.AuthorIn, ctx); part != "" {
		parts = append(parts, part)
	}
	if part := describeStringRule("labels include", selector.Labels.Include); part != "" {
		parts = append(parts, part)
	}
	if part := describeStringRule("labels exclude", selector.Labels.Exclude); part != "" {
		parts = append(parts, part)
	}
	if part := describeFieldRules(selector.Fields, ctx); part != "" {
		parts = append(parts, part)
	}
	if part := describePriorityRule(selector.PriorityIn); part != "" {
		parts = append(parts, part)
	}
	if part := describeChildSelectors("all", selector.And, ctx); part != "" {
		parts = append(parts, part)
	}
	if part := describeChildSelectors("any", selector.Or, ctx); part != "" {
		parts = append(parts, part)
	}
	return parts
}

func describeIdentityRule(label string, values []string, ctx Context) string {
	described := describeSelectorValues(values, ctx)
	if len(described) == 0 {
		return ""
	}
	return label + " " + strings.Join(described, ", ")
}

func describeStringRule(label string, values []string) string {
	values = nonBlankStrings(values)
	if len(values) == 0 {
		return ""
	}
	return label + " " + strings.Join(values, ", ")
}

func describeFieldRules(fields []FieldEquals, ctx Context) string {
	described := []string{}
	for _, field := range fields {
		name := strings.TrimSpace(field.Name)
		value := strings.TrimSpace(field.Value)
		if name == "" || value == "" {
			continue
		}
		described = append(described, name+" = "+describeSelectorValue(value, ctx))
	}
	if len(described) == 0 {
		return ""
	}
	if len(described) == 1 {
		return "field " + described[0]
	}
	return "fields " + strings.Join(described, ", ")
}

func describePriorityRule(priorities []int) string {
	if len(priorities) == 0 {
		return ""
	}
	described := make([]string, 0, len(priorities))
	for _, priority := range priorities {
		described = append(described, strconv.Itoa(priority))
	}
	return "priority in " + strings.Join(described, ", ")
}

func describeChildSelectors(label string, selectors []Selector, ctx Context) string {
	described := []string{}
	for _, child := range selectors {
		parts := describeSelectorParts(child, ctx)
		if len(parts) == 0 {
			if label == "any" {
				described = append(described, "All issues")
			}
			continue
		}
		described = append(described, strings.Join(parts, "; "))
	}
	if len(described) == 0 {
		return ""
	}
	return label + " (" + strings.Join(described, "; ") + ")"
}

func describeSelectorValues(values []string, ctx Context) []string {
	values = nonBlankStrings(values)
	described := make([]string, 0, len(values))
	for _, value := range values {
		described = append(described, describeSelectorValue(value, ctx))
	}
	return described
}

func describeSelectorValue(value string, ctx Context) string {
	value = strings.TrimSpace(value)
	if !isMeToken(value) {
		return value
	}

	resolved := nonBlankStrings(resolveMe(ctx))
	if len(resolved) == 0 {
		return meToken
	}
	return meToken + " (" + strings.Join(resolved, ", ") + ")"
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

func anyNonBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func nonBlankStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func trimStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.TrimSpace(value)
	}
	return out
}

func stringListProblems(field string, values []string) []string {
	var problems []string
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Sprintf("%s[%d] must not be blank", field, index))
		}
	}
	return problems
}

func priorityProblems(field string, priorities []int) []string {
	for _, priority := range priorities {
		if priority < 1 || priority > 4 {
			return []string{field + " values must be integers 1 through 4"}
		}
	}
	return nil
}
