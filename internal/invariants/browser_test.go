package invariants

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestRepositoryBrowserTests(t *testing.T) {
	t.Parallel()
	root := os.DirFS(repositoryRoot(t))
	data, err := fs.ReadFile(root, "internal/invariants/browser_policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string][]string
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkBrowserTests(root, policy) {
		t.Error(problem)
	}
}

func checkBrowserTests(root fs.FS, policy map[string][]string) []string {
	rules := []struct{ name, pattern, rule string }{
		{"specs", "", "extend an existing critical journey spec instead of adding a file"},
		{"serial", `\bmode\s*:\s*["']serial["']`, "do not use describe serial mode"},
		{"waitForTimeout", `\bwaitForTimeout\b`, "never use waitForTimeout or fixed sleeps"},
		{"toHaveScreenshot", `\btoHaveScreenshot\b`, "screenshots are limited to allowlisted key pages"},
		{"beforeAll", `\bbeforeAll\b`, "do not keep mutable shared state in beforeAll; isolate state per test"},
	}
	var problems []string
	files, err := fs.Glob(root, "tests/visual/*.spec.js")
	if err != nil {
		return []string{fmt.Sprintf("tests/visual: AGENTS.md#browser-tests: %v", err)}
	}
	sources := make(map[string][]byte, len(files))
	for _, file := range files {
		data, err := fs.ReadFile(root, file)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: AGENTS.md#browser-tests: %v", file, err))
			continue
		}
		sources[file] = data
	}
	for name := range policy {
		if !slices.ContainsFunc(rules, func(rule struct{ name, pattern, rule string }) bool { return rule.name == name }) {
			problems = append(problems, fmt.Sprintf("internal/invariants/browser_policy.json: AGENTS.md#browser-tests: unknown allowlist %q", name))
		}
	}
	for _, rule := range rules {
		pattern := regexp.MustCompile(rule.pattern)
		allowed := make(map[string]bool)
		for _, file := range policy[rule.name] {
			if allowed[file] {
				problems = append(problems, fmt.Sprintf("%s: AGENTS.md#browser-tests: duplicate %s allowlist entry", file, rule.name))
			}
			allowed[file] = true
			data, exists := sources[file]
			if !exists || !pattern.Match(data) {
				problems = append(problems, fmt.Sprintf("%s: AGENTS.md#browser-tests: remove stale %s allowlist entry; file or pattern no longer exists", file, rule.name))
			}
		}
		for _, file := range files {
			if data, exists := sources[file]; exists && pattern.Match(data) && !allowed[file] {
				problems = append(problems, fmt.Sprintf("%s: AGENTS.md#browser-tests: %s (not on frozen %s allowlist)", file, rule.rule, rule.name))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func TestBrowserPolicyViolations(t *testing.T) {
	t.Parallel()
	const file = "tests/visual/journey.spec.js"
	for _, tt := range []struct {
		name, source, category string
		allowed, missing       bool
		want                   string
	}{
		{name: "existing file passes", allowed: true},
		{name: "new file rejected", want: "not on frozen specs allowlist"},
		{name: "deleted file", allowed: true, missing: true, want: "remove stale specs allowlist entry"},
		{name: "serial rejected", source: `test.describe.configure({ mode: "serial" });`, category: "serial", want: "not on frozen serial allowlist"},
		{name: "single quoted multiline serial", source: "mode:\n 'serial'", category: "serial", want: "not on frozen serial allowlist"},
		{name: "existing serial", source: `mode: "serial"`, category: "serial", allowed: true},
		{name: "serial removed", source: `mode: "parallel"`, category: "serial", allowed: true, want: "remove stale serial allowlist entry"},
		{name: "sleep rejected", source: "page.waitForTimeout(500)", category: "waitForTimeout", want: "not on frozen waitForTimeout allowlist"},
		{name: "existing sleep", source: "page.waitForTimeout(500)", category: "waitForTimeout", allowed: true},
		{name: "sleep removed", category: "waitForTimeout", allowed: true, want: "remove stale waitForTimeout allowlist entry"},
		{name: "screenshot rejected", source: "expect(page).toHaveScreenshot()", category: "toHaveScreenshot", want: "not on frozen toHaveScreenshot allowlist"},
		{name: "existing screenshot", source: "expect(page).toHaveScreenshot()", category: "toHaveScreenshot", allowed: true},
		{name: "screenshot removed", category: "toHaveScreenshot", allowed: true, want: "remove stale toHaveScreenshot allowlist entry"},
		{name: "beforeAll rejected", source: "test.beforeAll(() => {})", category: "beforeAll", want: "not on frozen beforeAll allowlist"},
		{name: "existing beforeAll", source: "test.beforeAll(() => {})", category: "beforeAll", allowed: true},
		{name: "beforeAll removed", source: "test.beforeEach(() => {})", category: "beforeAll", allowed: true, want: "remove stale beforeAll allowlist entry"},
		{name: "pattern file deleted", category: "beforeAll", allowed: true, missing: true, want: "remove stale beforeAll allowlist entry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := fstest.MapFS{}
			if !tt.missing {
				root[file] = &fstest.MapFile{Data: []byte(tt.source)}
			}
			policy := map[string][]string{}
			if tt.category != "" || tt.allowed {
				policy["specs"] = []string{file}
			}
			if tt.allowed && tt.category != "" {
				policy[tt.category] = []string{file}
			}
			problems := checkBrowserTests(root, policy)
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("unexpected violations: %v", problems)
				}
				return
			}
			want := file + ": AGENTS.md#browser-tests: "
			if !slices.ContainsFunc(problems, func(problem string) bool {
				return strings.HasPrefix(problem, want) && strings.Contains(problem, tt.want)
			}) {
				t.Fatalf("got %v, want file, rule and %q", problems, tt.want)
			}
		})
	}
}
