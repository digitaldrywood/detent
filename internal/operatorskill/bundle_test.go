package operatorskill

import (
	"bytes"
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/explain"
)

func TestContentReturnsCopy(t *testing.T) {
	first := Content()
	second := Content()
	if len(first) == 0 {
		t.Fatal("Content() is empty")
	}
	first[0] ^= 0xff
	if bytes.Equal(first, second) {
		t.Fatal("Content() returned shared mutable storage")
	}
}

func TestSkillDocumentContract(t *testing.T) {
	document := string(Content())
	frontmatter, body := splitSkillDocument(t, document)

	var metadata map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(metadata) != 2 || metadata["name"] != Name || strings.TrimSpace(stringValue(metadata["description"])) == "" {
		t.Fatalf("frontmatter = %#v, want only name and description", metadata)
	}
	if !strings.Contains(body, "Bundle version: 1") {
		t.Fatalf("body missing bundle version %d", Version)
	}
	if Version != 1 {
		t.Fatalf("Version = %d, update the version stamp contract", Version)
	}
	if !strings.Contains(body, "Expected response schema: 1") || explain.SchemaVersion != 1 {
		t.Fatalf("response schema stamp does not match explain.SchemaVersion = %d", explain.SchemaVersion)
	}

	assertDecisionTreeContract(t, body)
	assertCommandCatalogContract(t, body)
	assertSafetyContract(t, document)
}

func assertDecisionTreeContract(t *testing.T, body string) {
	t.Helper()
	_, decisionTree, found := strings.Cut(body, "## Decision tree\n")
	if !found {
		t.Fatal("skill is missing the decision tree")
	}
	decisionTree, _, _ = strings.Cut(decisionTree, "\n## ")
	rawSections := strings.Split(strings.TrimSpace(decisionTree), "\n### ")
	wantNames := []string{
		"Current lane or latest transition",
		"Active work",
		"Eligibility or required gate",
		"Evidence confidence",
	}
	if len(rawSections) != len(wantNames) {
		t.Fatalf("decision sections = %d, want %d", len(rawSections), len(wantNames))
	}
	for index, rawSection := range rawSections {
		rawSection = strings.TrimPrefix(rawSection, "### ")
		name, section, found := strings.Cut(rawSection, "\n")
		if !found || name != wantNames[index] {
			t.Fatalf("decision section %d = %q, want %q", index, name, wantNames[index])
		}
		for _, marker := range []string{"Call once:", "Stop:", "Escalate:"} {
			if got := strings.Count(section, marker); got != 1 {
				t.Fatalf("section %q contains %q %d times, want 1", name, marker, got)
			}
		}
	}
}

func assertCommandCatalogContract(t *testing.T, body string) {
	t.Helper()
	commandPattern := regexp.MustCompile(`(?m)^detent --format json issue '<issue-ref>' --explain --project '<project-id>'$`)
	if got := len(commandPattern.FindAllString(body, -1)); got != 4 {
		t.Fatalf("recommended command count = %d, want one for each decision class", got)
	}
	if other := regexp.MustCompile(`(?m)^detent .+$`).FindAllString(body, -1); len(other) != 4 {
		t.Fatalf("named Detent commands = %q, want only the four recommended calls", other)
	}

	root := cli.NewRootCommand(context.Background())
	issue, _, err := root.Find([]string{"issue"})
	if err != nil || issue == nil || issue.Name() != "issue" {
		t.Fatalf("issue command lookup = %v, %v", issue, err)
	}
	for _, name := range []string{"explain", "project"} {
		if issue.Flags().Lookup(name) == nil {
			t.Fatalf("issue command does not catalog --%s", name)
		}
	}
	if root.PersistentFlags().Lookup("format") == nil {
		t.Fatal("root command does not catalog --format")
	}

	for _, code := range []string{
		"dashboard_unauthorized",
		"dashboard_forbidden",
		"ambiguous_reference",
		"issue_not_found",
		"runtime_unavailable",
		"dashboard_unreachable",
		"dashboard_timeout",
		"unsupported_model_version",
		"dashboard_request_failed",
	} {
		if !strings.Contains(root.Long, code) {
			t.Fatalf("CLI catalog does not contain named outcome %q", code)
		}
	}
}

func assertSafetyContract(t *testing.T, document string) {
	t.Helper()
	for _, required := range []string{
		"api_token_required",
		"A successful response with degraded fields is still a success",
		"Never substitute raw database access",
		"dashboard HTML scraping",
		"plaintext credential recovery",
		"mutating commands",
		"proposal tools",
		"outside repository `.detent/skills` directories",
	} {
		if !strings.Contains(document, required) {
			t.Fatalf("skill missing safety contract %q", required)
		}
	}
	for _, prohibited := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)/api/`),
		regexp.MustCompile(`(?i)(?:^|[[:space:]\x60(])/(?:Users|home|var|tmp|etc)/`),
		regexp.MustCompile(`(?i)[A-Z]:\\`),
		regexp.MustCompile(`(?i)bearer[[:space:]]+[A-Za-z0-9._~-]+`),
		regexp.MustCompile(`(?i)digitaldrywood|corylanou`),
	} {
		if match := prohibited.FindString(document); match != "" {
			t.Fatalf("skill contains prohibited local or uncataloged content %q", match)
		}
	}
	if strings.Contains(document, "operationId") || strings.Contains(document, "tool name") {
		t.Fatal("skill names an API or tool surface without a catalog contract")
	}
}

func splitSkillDocument(t *testing.T, document string) (string, string) {
	t.Helper()
	parts := strings.SplitN(document, "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatal("skill document does not have YAML frontmatter")
	}
	return parts[1], parts[2]
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func TestBundlePathContract(t *testing.T) {
	if Directory != Name || SkillFile != "SKILL.md" {
		t.Fatalf("bundle path = %q/%q", Directory, SkillFile)
	}
	if slices.Contains(strings.Split(Directory, "/"), ".detent") {
		t.Fatalf("bundle directory %q must not use worker metadata", Directory)
	}
}
