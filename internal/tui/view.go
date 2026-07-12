package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const dashboardFrameLines = 13

type dashboardSection struct {
	label       string
	total       int
	rows        []string
	emptyLabel  string
	visibleRows int
	styles      styles
}

type dashboardSectionCaps struct {
	running   int
	queue     int
	blocked   int
	completed int
}

func (m Model) renderDashboard() string {
	width, height := m.dashboardSize()
	var lines []string
	if m.hasSnapshot {
		lines = m.renderSnapshotDashboard(width, height)
	} else {
		lines = m.renderWaitingDashboard(width, height)
	}

	return strings.Join(normalizeScreen(lines, width, height), "\n")
}

func (m Model) dashboardSize() (int, int) {
	width := m.width
	if width <= 0 {
		width = defaultTerminalWidth
	}
	height := m.height
	if height <= 0 {
		height = defaultTerminalHeight
	}
	return width, height
}

func (m Model) renderWaitingDashboard(width int, height int) []string {
	lines := []string{
		m.renderTitleBorder(width),
		frameContent("Dashboard: "+m.styles.info.Render(defaultDashboardURL), width),
		frameContent("Logs: "+m.styles.info.Render(defaultString(m.logPath, "n/a")), width),
		frameContent(m.styles.muted.Render("Telemetry subscription active"), width),
	}
	if m.shutdownNote == "" {
		lines = append(lines, frameContent("", width))
	} else {
		lines = append(lines, frameContent("Shutdown: "+m.styles.warn.Render(m.shutdownNote), width))
	}
	lines = append(lines, sectionDivider("STATUS", 0, 0, width, m.styles))
	for len(lines) < height-3 {
		content := ""
		if len(lines) == 7 {
			content = m.styles.muted.Render("Waiting for telemetry snapshot")
		}
		lines = append(lines, frameContent(content, width))
	}
	lines = append(lines, horizontalDivider(width), m.renderFooter(width), bottomBorder(width))
	return lines
}

func (m Model) renderSnapshotDashboard(width int, height int) []string {
	snapshot := m.snapshot
	runningRows, runningColumns := m.runningTableRows(snapshot.Running, width)
	queueRows := m.queueRows(snapshot.Queue)
	blockedRows := m.blockedRows(snapshot.Blocked)
	completedRows := m.completedRows(snapshot.Completed)
	caps := calculateSectionCaps(height, len(runningRows), len(queueRows), len(blockedRows), len(completedRows))

	lines := []string{m.renderTitleBorder(width)}
	for _, line := range m.snapshotHeader(snapshot) {
		lines = append(lines, frameContent(line, width))
	}

	runningTotal := countOrLen(snapshot.Counts.Running, len(snapshot.Running))
	runningVisible := min(len(runningRows), caps.running)
	lines = append(lines, sectionDivider("RUNNING", runningTotal, runningVisible, width, m.styles))
	lines = append(lines, renderRunningTable(runningColumns, runningRows, caps.running, width, m.styles)...)

	sections := []dashboardSection{
		{
			label:       "QUEUE",
			total:       countOrLen(snapshot.Counts.Queue, len(snapshot.Queue)),
			rows:        queueRows,
			emptyLabel:  "No queued retries",
			visibleRows: caps.queue,
			styles:      m.styles,
		},
		{
			label:       "BLOCKED",
			total:       countOrLen(snapshot.Counts.Blocked, len(snapshot.Blocked)),
			rows:        blockedRows,
			emptyLabel:  "No blocked work",
			visibleRows: caps.blocked,
			styles:      m.styles,
		},
		{
			label:       "COMPLETED",
			total:       countOrLen(snapshot.Counts.Completed, len(snapshot.Completed)),
			rows:        completedRows,
			emptyLabel:  "No completed work",
			visibleRows: caps.completed,
			styles:      m.styles,
		},
	}
	for _, section := range sections {
		lines = append(lines, section.render(width)...)
	}

	lines = append(lines, horizontalDivider(width), m.renderFooter(width), bottomBorder(width))
	return lines
}

func (m Model) renderTitleBorder(width int) string {
	label := " detent "
	if !buildinfo.IsZero(m.build) {
		build := buildinfo.Normalize(m.build)
		commit := buildinfo.ShortCommit(build.Commit)
		if build.Dirty {
			commit += ", dirty"
		}
		label = fmt.Sprintf(" detent %s (%s) ", build.Version, commit)
	}
	return titledBorder("┌", "┐", label, "", width, m.styles.title)
}

func (m Model) snapshotHeader(snapshot telemetry.Snapshot) []string {
	separator := m.styles.muted.Render(" · ")
	project := formatOptionalInfo(formatProject(snapshot.Project), m.styles)
	instance := formatOptionalInfo(formatInstance(snapshot.Instance), m.styles)
	scope := formatOptionalInfo(formatAuthorizationScope(snapshot.Instance), m.styles)

	counts := m.styles.ok.Render(fmt.Sprintf("● %d running", countOrLen(snapshot.Counts.Running, len(snapshot.Running)))) +
		m.styles.muted.Render("   ") +
		m.styles.warn.Render(fmt.Sprintf("◐ %d queued", countOrLen(snapshot.Counts.Queue, len(snapshot.Queue)))) +
		m.styles.muted.Render("   ") +
		m.styles.error.Render(fmt.Sprintf("✗ %d blocked", countOrLen(snapshot.Counts.Blocked, len(snapshot.Blocked)))) +
		m.styles.muted.Render("   ") +
		m.styles.info.Render(fmt.Sprintf("✓ %d completed", countOrLen(snapshot.Counts.Completed, len(snapshot.Completed))))

	refresh := formatNextRefresh(snapshot.Refresh)
	if refresh == "" {
		refresh = "n/a"
	}
	counts += separator + m.styles.accent.Render("up "+formatRuntimeSeconds(snapshot.Tokens.RuntimeSeconds)) +
		separator + m.styles.muted.Render("next "+refresh)

	tokens := m.styles.warn.Render("tok in "+formatCount(snapshot.Tokens.Input)+" / out "+formatCount(snapshot.Tokens.Output)) +
		formatCacheReadSummary(snapshot.Tokens, m.styles)
	throughput := m.styles.info.Render(formatTokenThroughput(snapshot.Throughput))
	budget := formatBudget(snapshot.Budget, m.styles)

	statusLine := m.styles.muted.Render("dashboard ") + m.styles.info.Render(formatDashboardURL(snapshot)) +
		separator + m.styles.muted.Render("rate ") + formatRateLimits(snapshot.RateLimits, m.now, m.styles)
	lifecycleLabel, lifecycleStatus := formatLifecycle(snapshot.Shutdown, m.now, m.shutdownTimeoutSource, m.styles)
	if m.shutdownNote != "" && !snapshot.Shutdown.Draining {
		lifecycleLabel = "Shutdown"
		lifecycleStatus = m.styles.warn.Render(m.shutdownNote)
	}
	if lifecycleLabel == "Shutdown" {
		statusLine = lifecycleLabel + ": " + lifecycleStatus
	}

	return []string{
		project + separator + instance + separator + m.styles.muted.Render("scope: ") + scope,
		counts,
		throughput + separator + tokens + separator + m.styles.muted.Render("budget ") + budget,
		statusLine,
	}
}

func calculateSectionCaps(height int, runningRows int, queueRows int, blockedRows int, completedRows int) dashboardSectionCaps {
	available := max(height-dashboardFrameLines, 4)
	demand := [4]int{
		max(runningRows, 1),
		max(queueRows, 1),
		max(blockedRows, 1),
		max(completedRows, 1),
	}
	caps := [4]int{1, 1, 1, 1}
	remaining := available - len(caps)
	order := [...]int{0, 3, 0, 1, 2}
	for remaining > 0 {
		allocated := false
		for _, section := range order {
			if remaining == 0 {
				break
			}
			if caps[section] >= demand[section] {
				continue
			}
			caps[section]++
			remaining--
			allocated = true
		}
		if !allocated {
			caps[0] += remaining
			break
		}
	}
	return dashboardSectionCaps{
		running:   caps[0],
		queue:     caps[1],
		blocked:   caps[2],
		completed: caps[3],
	}
}

func (m Model) runningTableRows(running []telemetry.Running, width int) ([]table.Row, []table.Column) {
	contentWidth := max(width-4, 1)
	columns := runningTableColumns(contentWidth, width)
	rows := append([]telemetry.Running(nil), running...)
	sort.Slice(rows, func(i int, j int) bool {
		return issueLabel(rows[i].Issue) < issueLabel(rows[j].Issue)
	})

	tableRows := make([]table.Row, 0, len(rows))
	for _, row := range rows {
		event := cleanInline(row.LastMessage)
		if row.LastMessageTruncation != nil && row.LastMessageTruncation.Truncated && event != "" {
			event += " [truncated]"
		}
		if event == "" {
			event = cleanInline(row.LastEvent)
		}
		if event == "" {
			event = "none"
		}

		values := map[string]string{
			"ID":         m.styles.ok.Render("●") + " " + m.styles.info.Render(issueDisplayLabel(row.Issue, 8)),
			"STAGE":      statusStyle(row.LastEvent, m.styles).Render(defaultString(row.State, "unknown")),
			"PID":        m.styles.warn.Render(defaultString(row.ProcessIdentity, "n/a")),
			"AGE / TURN": m.styles.accent.Render(formatRuntimeAndTurns(row.RuntimeSeconds, row.TurnCount)),
			"TOKENS/CTX": runningTokenStyle(row.Tokens, m.styles).Render(formatRunningTokenPressure(row.Tokens)),
			"SESSION":    m.styles.info.Render(compactSessionID(row.SessionID)),
			"EVENT":      statusStyle(row.LastEvent, m.styles).Render(event),
		}
		tableRow := make(table.Row, 0, len(columns))
		for _, column := range columns {
			tableRow = append(tableRow, values[column.Title])
		}
		tableRows = append(tableRows, tableRow)
	}
	return tableRows, columns
}

func runningTableColumns(contentWidth int, terminalWidth int) []table.Column {
	columns := []table.Column{
		{Title: "ID", Width: 10},
		{Title: "STAGE", Width: 14},
	}
	fixedWidth := 24
	if terminalWidth >= 110 {
		columns = append(columns, table.Column{Title: "PID", Width: 12})
		fixedWidth += 12
	}
	columns = append(columns,
		table.Column{Title: "AGE / TURN", Width: 14},
		table.Column{Title: "TOKENS/CTX", Width: 14},
	)
	fixedWidth += 28
	if terminalWidth >= 90 {
		columns = append(columns, table.Column{Title: "SESSION", Width: 16})
		fixedWidth += 16
	}
	columns = append(columns, table.Column{Title: "EVENT", Width: max(contentWidth-fixedWidth, 1)})
	return columns
}

func renderRunningTable(columns []table.Column, rows []table.Row, visibleRows int, width int, s styles) []string {
	contentWidth := max(width-4, 1)
	visibleRows = max(visibleRows, 1)
	if len(rows) == 0 {
		rows = []table.Row{{s.muted.Render("No active agents")}}
	} else if len(rows) > visibleRows {
		rows = rows[:visibleRows]
	}

	tableStyles := table.DefaultStyles()
	tableStyles.Header = s.muted.Bold(true)
	tableStyles.Cell = lipgloss.NewStyle()
	tableStyles.Selected = lipgloss.NewStyle()
	component := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithStyles(tableStyles),
	)
	component.SetWidth(contentWidth)
	component.SetHeight(visibleRows + 1)

	rendered := strings.Split(component.View(), "\n")
	if len(rendered) > visibleRows+1 {
		rendered = rendered[:visibleRows+1]
	}
	for len(rendered) < visibleRows+1 {
		rendered = append(rendered, "")
	}
	lines := make([]string, 0, len(rendered))
	for _, line := range rendered {
		lines = append(lines, frameTableContent(line, width))
	}
	return lines
}

func (m Model) queueRows(queue []telemetry.Queued) []string {
	rows := append([]telemetry.Queued(nil), queue...)
	sort.Slice(rows, func(i int, j int) bool {
		if rows[i].DueInMillis == rows[j].DueInMillis {
			return issueLabel(rows[i].Issue) < issueLabel(rows[j].Issue)
		}
		return rows[i].DueInMillis < rows[j].DueInMillis
	})

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		line := m.styles.warn.Render("◐") + " " + m.styles.info.Render(issueDisplayLabel(row.Issue, 8)) +
			m.styles.muted.Render(fmt.Sprintf("  attempt %d  retry in ", row.Attempt)) + m.styles.info.Render(formatDueIn(row.DueInMillis))
		if detail := cleanInline(row.Error); detail != "" {
			line += m.styles.muted.Render("   ") + m.styles.error.Render(detail)
		}
		lines = append(lines, line)
	}
	return lines
}

func (m Model) blockedRows(blocked []telemetry.Blocked) []string {
	rows := append([]telemetry.Blocked(nil), blocked...)
	sort.Slice(rows, func(i int, j int) bool {
		return issueLabel(rows[i].Issue) < issueLabel(rows[j].Issue)
	})

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		detail := cleanInline(row.Error)
		if detail == "" {
			detail = cleanInline(row.LastMessage)
		}
		if detail == "" {
			detail = "blocked"
		}
		lines = append(lines, m.styles.error.Render("✗")+" "+m.styles.info.Render(issueDisplayLabel(row.Issue, 8))+m.styles.muted.Render("  ")+m.styles.error.Render(detail))
	}
	return lines
}

func (m Model) completedRows(completed []telemetry.Completed) []string {
	rows := append([]telemetry.Completed(nil), completed...)
	sort.Slice(rows, func(i int, j int) bool {
		return rows[i].CompletedAt.After(rows[j].CompletedAt)
	})

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		state := defaultString(row.FinalState, row.State)
		if state == "" {
			state = "completed"
		}
		line := m.styles.ok.Render("✓") + " " + m.styles.info.Render(issueDisplayLabel(row.Issue, 8)) +
			m.styles.muted.Render("  ") + m.styles.ok.Render(state) + m.styles.muted.Render("  ") +
			m.styles.accent.Render(formatRuntimeAndTurns(row.RuntimeSeconds, row.Turns)) + m.styles.muted.Render("  ") +
			m.styles.warn.Render(formatCount(row.Tokens.Total))
		if model := cleanInline(row.Model); model != "" {
			line += m.styles.muted.Render("  ") + m.styles.info.Render(model)
		}
		lines = append(lines, line)
	}
	return lines
}

func (s dashboardSection) render(width int) []string {
	visible := min(len(s.rows), s.visibleRows)
	lines := []string{sectionDivider(s.label, s.total, visible, width, s.styles)}
	if visible == 0 {
		lines = append(lines, frameContent(s.styles.muted.Render(s.emptyLabel), width))
	} else {
		for _, row := range s.rows[:visible] {
			lines = append(lines, frameContent(row, width))
		}
	}
	for len(lines) < s.visibleRows+1 {
		lines = append(lines, frameContent("", width))
	}
	return lines
}

func sectionDivider(label string, total int, visible int, width int, s styles) string {
	title := fmt.Sprintf(" %s (%d) ", label, total)
	right := ""
	if overflow := max(total-visible, 0); overflow > 0 {
		title = fmt.Sprintf(" %s (%d, showing %d) ", label, total, visible)
		right = fmt.Sprintf(" +%d more ", overflow)
	}
	return titledBorder("├", "┤", title, right, width, s.title)
}

func titledBorder(left string, right string, title string, suffix string, width int, style lipgloss.Style) string {
	if width <= 1 {
		return truncate(title, width)
	}
	innerWidth := width - 2
	title = ansi.Truncate(title, innerWidth, "")
	suffixWidth := ansi.StringWidth(suffix)
	if suffixWidth > innerWidth-ansi.StringWidth(title) {
		suffix = ""
		suffixWidth = 0
	}
	fill := max(innerWidth-ansi.StringWidth(title)-suffixWidth, 0)
	return left + style.Render(title) + strings.Repeat("─", fill) + style.Render(suffix) + right
}

func horizontalDivider(width int) string {
	if width <= 1 {
		return "├"
	}
	return "├" + strings.Repeat("─", max(width-2, 0)) + "┤"
}

func bottomBorder(width int) string {
	if width <= 1 {
		return "└"
	}
	return "└" + strings.Repeat("─", max(width-2, 0)) + "┘"
}

func frameContent(content string, width int) string {
	if width <= 1 {
		return fitANSI(content, width)
	}
	return "│" + fitANSI(" "+content, width-2) + "│"
}

func frameTableContent(content string, width int) string {
	if width <= 1 {
		return fitANSI(content, width)
	}
	return "│ " + fitANSI(content, max(width-4, 0)) + " │"
}

func (m Model) renderFooter(width int) string {
	left := m.styles.muted.Render("q quit · ? help")
	right := m.styles.muted.Render("logs " + defaultString(m.logPath, "n/a"))
	innerWidth := max(width-2, 0)
	contentWidth := max(innerWidth-2, 0)
	if ansi.StringWidth(left)+ansi.StringWidth(right)+1 > contentWidth {
		right = ansi.Truncate(right, max(contentWidth-ansi.StringWidth(left)-1, 0), "")
	}
	gap := max(contentWidth-ansi.StringWidth(left)-ansi.StringWidth(right), 1)
	return frameContent(left+strings.Repeat(" ", gap)+right, width)
}

func normalizeScreen(lines []string, width int, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = fitANSI(lines[i], width)
	}
	return lines
}

func fitANSI(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Truncate(value, width, "")
	return value + strings.Repeat(" ", max(width-ansi.StringWidth(value), 0))
}
