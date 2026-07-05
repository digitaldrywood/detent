package templates

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/ui/primitives"
)

// projectTab is one entry in the project sub-nav. Tabs are real links; the
// active one is marked by weight and a 2px accent underline (accent =
// interactivity, never status).
type projectTab struct {
	ID     string
	Label  string
	Href   string
	Active bool
}

func projectTabs(data DashboardData, active string) []projectTab {
	id := strings.TrimSpace(data.ProjectID)
	tabs := []projectTab{
		{ID: "overview", Label: "Overview", Href: projectDashboardPath(id)},
		{ID: "kanban", Label: "Kanban", Href: projectKanbanPath(id)},
		{ID: "runs", Label: "Runs", Href: projectRunsPath(id)},
		{ID: "configuration", Label: "Configuration", Href: projectConfigurationPath(id)},
		{ID: "diagnostics", Label: "Diagnostics", Href: projectDiagnosticsPath(id)},
	}
	for i := range tabs {
		tabs[i].Active = tabs[i].ID == active
	}
	return tabs
}

func projectTabClass(tab projectTab) string {
	base := "flex-none px-0.5 py-2.5 text-sm "
	if tab.Active {
		return base + "font-medium text-text shadow-[inset_0_-2px_0_var(--color-accent)]"
	}
	return base + "text-sec hover:text-text"
}

// projectRunRow is one row in the runs table. TITLE is the only flexible
// column; every other column is fixed so nothing ever clips or wraps.
type projectRunRow struct {
	DomID     string
	Ref       string
	Title     string
	StateKind primitives.Kind
	StateText string
	StateLive bool
	Runtime   string
	Tokens    string
	Cost      string
	Finished  string
}

// projectRunRows lists live sessions first, then completed sessions newest
// first. Per-run dollar cost is not part of the telemetry snapshot; the
// column renders an em dash until usage data carries it (Reports has the
// authoritative per-issue spend).
// runRowIDKey keeps run-row DOM ids unique for non-GitHub tracker
// identifiers (e.g. memory IDs like MT-1), whose split yields an empty
// #number; it falls back to the full identifier as fleetAgentRows does.
func runRowIDKey(repo string, number string) string {
	if strings.TrimSpace(number) != "" {
		return number
	}
	return repo
}

func projectRunRows(snapshot telemetry.Snapshot, limit int) []projectRunRow {
	rows := make([]projectRunRow, 0, len(snapshot.Running)+len(snapshot.Completed))
	for _, running := range snapshot.Running {
		repo, number := splitIssueIdentifier(issueIdentifier(running.Issue))
		rows = append(rows, projectRunRow{
			DomID:     "run-" + boardCardSlug(running.ProjectID, runRowIDKey(repo, number)),
			Ref:       projectKanbanIssueNumber(running.Issue),
			Title:     issueTitle(running.Issue),
			StateKind: primitives.KindOK,
			StateText: "In progress",
			StateLive: true,
			Runtime:   formatDuration(running.RuntimeSeconds) + " · " + strconv.Itoa(running.TurnCount),
			Tokens:    fleetCompactTokens(running.Tokens.Total),
			Cost:      "—",
			Finished:  "—",
		})
	}

	completed := append([]telemetry.Completed(nil), snapshot.Completed...)
	sort.SliceStable(completed, func(i, j int) bool {
		return completed[i].CompletedAt.After(completed[j].CompletedAt)
	})
	for _, session := range completed {
		repo, number := splitIssueIdentifier(issueIdentifier(session.Issue))
		kind, text := projectRunFinalState(session.FinalState)
		rows = append(rows, projectRunRow{
			DomID:     "run-" + boardCardSlug(session.ProjectID, runRowIDKey(repo, number)) + "-" + strings.TrimSpace(session.SessionID),
			Ref:       projectKanbanIssueNumber(session.Issue),
			Title:     issueTitle(session.Issue),
			StateKind: kind,
			StateText: text,
			Runtime:   formatDuration(session.RuntimeSeconds) + " · " + strconv.Itoa(session.Turns),
			Tokens:    fleetCompactTokens(session.Tokens.Total),
			Cost:      "—",
			Finished:  projectRunFinishedLabel(session.CompletedAt),
		})
	}

	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func projectRunFinalState(state string) (primitives.Kind, string) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "done", "completed", "success", "merged":
		return primitives.KindOK, "Completed"
	case "blocked":
		return primitives.KindErr, "Blocked"
	case "failed", "error":
		return primitives.KindErr, "Failed"
	case "cancelled", "canceled":
		return primitives.KindNeutral, "Cancelled"
	}
	return primitives.KindNeutral, state
}

func projectRunFinishedLabel(at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return at.UTC().Format("Jan 2 15:04")
}

// projectRepoURL resolves the project's external URL: the scoped snapshot
// first, then the registry-enriched project list.
func projectRepoURL(data DashboardData) string {
	if url := strings.TrimSpace(data.Snapshot.Project.URL); url != "" {
		return url
	}
	for _, project := range data.Projects {
		if strings.TrimSpace(project.ID) == strings.TrimSpace(data.ProjectID) {
			return strings.TrimSpace(project.URL)
		}
	}
	return ""
}

func projectSlugLabel(data DashboardData) string {
	if url := projectRepoURL(data); url != "" {
		slug := strings.TrimPrefix(strings.TrimPrefix(url, "https://github.com/"), "https://")
		return strings.TrimSuffix(slug, "/issues")
	}
	return strings.TrimSpace(data.ProjectID)
}

func projectAllClearLabel(data DashboardData) string {
	running := runningCount(data.Snapshot)
	switch running {
	case 0:
		return "All clear — nothing needs you on this project."
	case 1:
		return "All clear — 1 agent working on this project."
	}
	return "All clear — " + formatCount(running) + " agents working on this project."
}

func projectRunRowClass(last bool) string {
	base := "grid grid-cols-[70px_minmax(0,1fr)_120px_130px_90px_70px_110px] items-center gap-3.5 px-4 py-2"
	if !last {
		return base + " border-b border-line"
	}
	return base
}

func ProjectShellDataFromDashboard(data DashboardData, nav string) DashboardShellData {
	shell := DashboardShellDataFromDashboard(data)
	shell.ActiveNav = nav
	shell.IncludeDashboardCharts = false
	return shell
}
