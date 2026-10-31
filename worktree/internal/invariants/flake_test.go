package invariants

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestINV19NoFlakeAccommodation(t *testing.T) {
	for _, problem := range checkFlakeAccommodation(os.DirFS(repositoryRoot(t))) {
		t.Error(problem)
	}
}

var (
	failOnFlaky     = regexp.MustCompile(`\bfailOnFlakyTests\s*:\s*(true|!!process\.env\.CI)\b`)
	retryCount      = regexp.MustCompile(`\bretries\s*:\s*(?:process\.env\.CI\s*\?\s*)?(\d+)`)
	specAccommodate = regexp.MustCompile(`\b(test|describe)\.(skip|fixme|fail)\s*\(|\btest\.describe\.(skip|fixme)\s*\(|\bretries\s*:`)
	goFlakySkip     = regexp.MustCompile(`(?i)\bt\.Skipf?\([^)]*flak`)
)

func checkFlakeAccommodation(root fs.FS) []string {
	var problems []string
	config, err := fs.ReadFile(root, "playwright.config.js")
	if err != nil {
		return []string{fmt.Sprintf("playwright.config.js: INV-19: %v", err)}
	}
	if !failOnFlaky.Match(config) {
		problems = append(problems, "playwright.config.js: INV-19: CI must keep failOnFlakyTests on")
	}
	for _, match := range retryCount.FindAllSubmatch(config, -1) {
		if retries, _ := strconv.Atoi(string(match[1])); retries > 1 {
			problems = append(problems, fmt.Sprintf("playwright.config.js: INV-19: retries %d exceeds 1", retries))
		}
	}
	err = fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git", "tmp", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		var pattern *regexp.Regexp
		switch {
		case strings.HasPrefix(path, "tests/visual/") && strings.HasSuffix(path, ".spec.js"):
			pattern = specAccommodate
		case strings.HasSuffix(path, "_test.go") && path != "internal/invariants/flake_test.go":
			pattern = goFlakySkip
		default:
			return nil
		}
		source, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		if pattern.Match(source) {
			problems = append(problems, path+": INV-19: never skip, mark or retry a flaky test; fix its cause or delete it")
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		problems = append(problems, fmt.Sprintf("INV-19: %v", err))
	}
	return problems
}

func TestINV19Violations(t *testing.T) {
	t.Parallel()
	const config = "module.exports = { retries: process.env.CI ? 1 : 0, failOnFlakyTests: !!process.env.CI };"
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "clean", files: map[string]string{"tests/visual/a.spec.js": `test("a", async () => {});`, "x_test.go": `t.Skip("needs docker")`}},
		{name: "flaky allowed", files: map[string]string{"playwright.config.js": strings.Replace(config, "!!process.env.CI", "false", 1)}, want: "failOnFlakyTests"},
		{name: "extra retries", files: map[string]string{"playwright.config.js": strings.Replace(config, "? 1", "? 3", 1)}, want: "retries 3"},
		{name: "skipped spec", files: map[string]string{"tests/visual/a.spec.js": `test.skip("a", async () => {});`}, want: "tests/visual/a.spec.js"},
		{name: "fixme spec", files: map[string]string{"tests/visual/a.spec.js": `test.fixme("a", async () => {});`}, want: "tests/visual/a.spec.js"},
		{name: "spec retries", files: map[string]string{"tests/visual/a.spec.js": `test.describe.configure({ retries: 2 });`}, want: "tests/visual/a.spec.js"},
		{name: "flaky go skip", files: map[string]string{"x_test.go": `t.Skip("flaky on CI")`}, want: "x_test.go"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := fstest.MapFS{"playwright.config.js": {Data: []byte(config)}}
			for path, data := range tt.files {
				root[path] = &fstest.MapFile{Data: []byte(data)}
			}
			problems := strings.Join(checkFlakeAccommodation(root), "\n")
			if (tt.want == "") != (problems == "") || !strings.Contains(problems, tt.want) {
				t.Fatalf("problems = %q, want %q", problems, tt.want)
			}
		})
	}
}
