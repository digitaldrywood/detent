package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

func TestKnownTrackerReferencesRenderLinks(t *testing.T) {
	t.Parallel()

	issueURL := "https://github.com/digitaldrywood/detent/issues/1132"
	prURL := "https://github.com/digitaldrywood/detent/pull/1133"
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		component templ.Component
		want      []string
	}{
		{
			name:      "runs",
			component: projectRunsTable("project-runs", []projectRunRow{{DomID: "run-1132", Ref: "#1132", URL: issueURL}}),
			want:      []string{`href="` + issueURL + `"`, ">#1132</a>"},
		},
		{
			name: "board issue and pull request",
			component: boardCardView2(boardCardView{
				DomID: "card-1132", Number: "#1132", URL: issueURL, Project: "detent", Title: "Link references", MetaRight: "PR #1133", PRURL: prURL,
			}),
			want: []string{`href="` + issueURL + `"`, ">#1132</a>", `href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name:      "sheet",
			component: sheetRowLink("Pull request", "PR #1133", prURL),
			want:      []string{`href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name:      "fleet agent",
			component: AgentActivityPanel([]fleetAgentRow{{ID: "agent-1132", Repo: "digitaldrywood/detent", Number: "#1132", URL: issueURL}}, "1 running"),
			want:      []string{`href="` + issueURL + `"`, ">#1132</a>"},
		},
		{
			name: "fleet pull request",
			component: fleetSnapshotBody(DashboardData{Snapshot: telemetry.Snapshot{
				GeneratedAt: now,
				Pipeline: []telemetry.Issue{{
					Identifier: "digitaldrywood/detent#1132", ProjectID: "detent", State: "Human Review", URL: issueURL,
					PullRequest: &telemetry.PullRequest{Number: 1133, URL: prURL},
				}},
			}}),
			want: []string{`href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name: "analytics",
			component: AnalyticsSnapshotV2(DashboardData{Snapshot: telemetry.Snapshot{WorkAttempts: []telemetry.WorkAttempt{{
				AttemptID: 1, Identifier: "digitaldrywood/detent#1132", IssueURL: issueURL,
			}}}}),
			want: []string{`href="` + issueURL + `"`, ">#1132</a>"},
		},
		{
			name:      "reports",
			component: reportsTopTable("reports-top", "Top issues", []reportsTopRow{{ID: "top-1132", Ref: "#1132", URL: issueURL}}),
			want:      []string{`href="` + issueURL + `"`, ">#1132</a>"},
		},
		{
			name: "issue identity",
			component: issueIdentityLine(issueIdentityView{
				IssueNumber: "#1132", IssueURL: issueURL, PullRequestLabel: "PR #1133", PullRequestURL: prURL,
			}, false),
			want: []string{`href="` + issueURL + `"`, `href="` + prURL + `"`},
		},
		{
			name: "merge lane detail",
			component: prPipelineCardView(prPipelineCard{
				Title: "Merge issue", MergeLaneStatus: "Merging now", MergeLaneDetail: "Active merge worker for PR #1133",
				MergeLanePrefix: "Active merge worker for ", MergeLaneRef: "PR #1133", MergeLaneRefURL: prURL,
			}),
			want: []string{`href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name:      "comments",
			component: kanbanPRCommentsPanel(KanbanConversationData{PRNumber: 1133, PRURL: prURL}),
			want:      []string{`href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name: "library",
			component: libraryTableRow(LibraryRow{
				ID: "library-1133", SourceKind: "pull_request", ArtifactPath: "PR #1133", PullRequestURL: prURL,
				SourceURL: issueURL, SourceLabel: "digitaldrywood/detent#1132",
			}),
			want: []string{`href="` + issueURL + `"`, `href="` + prURL + `"`, ">PR #1133</a>"},
		},
		{
			name:      "exception",
			component: primitives.ExceptionStrip([]primitives.Exception{{ID: "exception-1132", Kind: primitives.KindErr, Ref: "#1132", RefURL: issueURL}}),
			want:      []string{`href="` + issueURL + `"`, ">#1132</a>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			html := renderBoardComponent(t, tt.component)
			for _, want := range append(tt.want, `target="_blank"`, `rel="noopener noreferrer"`) {
				if !strings.Contains(html, want) {
					t.Fatalf("rendered reference missing %q:\n%s", want, html)
				}
			}
		})
	}
}
