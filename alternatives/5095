package searchtest

import (
	"net/url"
	"strings"
)

type SnippetCase struct{ Name, Title, Body, Query, Expression, Want string }

func Snippets() []SnippetCase {
	return []SnippetCase{
		{"stemming", "Relational runners", "", "relation", `"relation"*`, "[Relational] runners"},
		{"diacritics", "Café résumé", "", "cafe", `"cafe"*`, "[Café] résumé"},
		{"body coverage", "Runner", "Runner checkout deadlocks", "runner checkout", `"runner"* OR "checkout"*`, "[Runner] [checkout] deadlocks"},
		{"Unicode offsets", strings.Repeat("é", 80) + " runners", "", "runner", `"runner"*`, strings.Repeat("é", 30) + " [runners]"},
		{"embedded NUL", "prefix\x00runners", "", "runner", `"runner"*`, "prefix\x00[runners]"},
		{"plain terms", "repository association", "", "repository association", `"repository" OR "association"`, "[repository] [association]"},
	}
}

type ThreadFilter struct {
	Name, Query string
	Want        []string
}

type PrefixCase struct {
	Name, Query  string
	Exact, First bool
}

func Prefixes() []PrefixCase {
	return []PrefixCase{
		{"long prefix one", strings.Repeat("a", 64) + "o", false, true},
		{"long prefix two", strings.Repeat("a", 64) + "t", false, false},
		{"long exact", strings.Repeat("a", 64) + "one", true, true},
		{"unicode prefix", "café", false, true},
		{"exact excludes longer tokens", "tokenalpha", true, true},
		{"prefix excludes adjacent terms", "tokenb", false, true},
	}
}

func ThreadFilters(project, issue, first, middle, last string) []ThreadFilter {
	return []ThreadFilter{
		{"general scope", "scope=general", []string{first}},
		{"project scope", "scope=project", []string{middle}},
		{"project includes issue threads", "project_id=" + url.QueryEscape(project), []string{last, middle}},
		{"issue", "issue_id=" + url.QueryEscape(issue), []string{last}},
		{"open status", "status=open", []string{middle, first}},
		{"resolved status", "status=resolved", []string{last}},
		{"combined", "scope=issue&project_id=" + url.QueryEscape(project) + "&issue_id=" + url.QueryEscape(issue) + "&status=resolved&search=shared", []string{last}},
		{"conflicting filters", "scope=general&project_id=" + url.QueryEscape(project), nil},
		{"bound project value", "project_id=anything%27%20OR%201=1--", nil},
		{"bound issue value", "issue_id=anything%27%20OR%201=1--", nil},
	}
}
