package postgres

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestCommentFingerprintRefreshSelectsEveryFingerprintedComment(t *testing.T) {
	raw, err := os.ReadFile("migrations/20261114014702_search_comment_fingerprint_refresh.sql")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`terms && ARRAY\[([^\]]+)\]`).FindSubmatch(raw)
	if match == nil {
		t.Fatal("comment refresh migration has no term filter")
	}
	var selected []string
	for _, term := range strings.Split(string(match[1]), ",") {
		selected = append(selected, strings.Trim(strings.TrimSpace(term), "'"))
	}
	for _, body := range []string{
		"internal/example/pool.go:23:4: undefined: capacity",
		"[internal/example/pool.go:23] - (G101 (CWE-798): Potential hardcoded credentials)",
		"--- FAIL: TestJourney (0.10s)\nFAIL\texample/hub\t0.2s",
		"goose: duplicate version 20261114011610",
		"duplicate migration version 20261114011610",
		"Problem: `go-test:example/hub:TestJourney`",
		"Problem: `go-diagnostic:internal/example/pool.go:1:2:undefined: capacity`",
		issueorigin.Occurrence(issueorigin.Stamp("Another occurrence", issueorigin.Origin{Kind: "worker", Fingerprint: "occurrence-failure"})),
		issueorigin.Stamp("Recorded audit", issueorigin.Origin{Kind: "audit", Fingerprint: "audit-failure"}),
		"<!-- detent-audit-fp: audit-marker -->",
	} {
		document := searchstore.Document{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Kind: "comment"}
		if len(searchstore.DuplicateFingerprintKeys(body, true)) == 0 {
			t.Fatalf("fixture comment carries no fingerprint: %q", body)
		}
		terms := searchstore.Describe(document, searchstore.Text{Body: body}).Terms
		if !slices.ContainsFunc(terms, func(term string) bool { return slices.Contains(selected, term) }) {
			t.Fatalf("refresh filter %v misses fingerprinted comment %q with terms %v", selected, body, terms)
		}
	}
}
