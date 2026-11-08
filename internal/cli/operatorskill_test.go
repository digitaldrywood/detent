package cli_test

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/cli"
	"github.com/digitaldrywood/detent/internal/operatorskill"
)

func TestOperatorSkillContentMatchesBundle(t *testing.T) {
	if content := detent.OperatorSkillContent(); !bytes.Equal(content, operatorskill.Content()) {
		t.Fatal("OperatorSkillContent() does not match the source bundle")
	}
}

func TestOperatorSkillCommandCatalogContract(t *testing.T) {
	body := strings.ReplaceAll(string(operatorskill.Content()), "\r\n", "\n")
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
