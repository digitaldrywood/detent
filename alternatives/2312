package orchestrator

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyRefsFromIssueTextFences(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       []string
	}{
		{name: "backticks", body: "```text\nDepends on: #123\n```"},
		{name: "tildes", body: "~~~\nBlocked by: #123\n~~~"},
		{name: "after closing", body: "```\nDepends on: #123\n```\nDepends on: #7", want: []string{"owner/repo#7"}},
		{name: "surrounding tildes", body: "Depends on: #6\n~~~go\nDepends on: #123\n~~~\nBlocked by: #7", want: []string{"owner/repo#6", "owner/repo#7"}},
		{name: "nested shorter fence", body: "````\n```\nDepends on: #123\n```\n````\nDepends on: #7", want: []string{"owner/repo#7"}},
		{name: "unfinished", body: "Depends on: #7\n```\nDepends on: #123", want: []string{"owner/repo#7"}},
		{name: "blockers section example", body: "## Blockers\n~~~\nDepends on: #123\n~~~\n#7", want: []string{"owner/repo#7"}},
		{name: "example blockers heading", body: "```\n## Blockers\n#123\n```\nDepends on: #7", want: []string{"owner/repo#7"}},
		{name: "ordinary", body: "- **Depends on:** other/repo#8 and #7. See #123", want: []string{"other/repo#8", "owner/repo#7"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := connector.Issue{Identifier: "owner/repo#99", Description: tt.body}
			var got []string
			for _, ref := range dependencyRefsFromIssueText(issue) {
				got = append(got, ref.Identifier)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("refs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDependencyReasonRefsFences(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, reason string
		want         []string
	}{
		{name: "example only", reason: "```\nDepends on: #123\n```"},
		{name: "waiting example", reason: "~~~\nwaiting on #123\n~~~"},
		{name: "real waiting reason", reason: "```\nDepends on: #123\n```\nwaiting on #7", want: []string{"owner/repo#7"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, ref := range dependencyRefsFromIssueText(connector.Issue{Identifier: "owner/repo#99", BlockerReason: tt.reason}) {
				got = append(got, ref.Identifier)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("refs = %v, want %v", got, tt.want)
			}
		})
	}
}
