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

type dashboardSection struct {
	label       string
	total       int
	rows        []string
	emptyLabel  string
	visibleRows int
	offset      int
	focused     bool
	collapsed   bool
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

	screen := strings.Join(normalizeScreen(lines, width, height), "\n")
	if m.helpVisible {
		return renderHelpOverlay(screen, width, height, m.styles)
	}
	return screen
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

func newRunningTable(s styles) table.Model {
	tableStyles := table.DefaultStyles()
	tableStyles.Header = s.muted.Bold(true)
	tableStyles.Cell = lipgloss.NewStyle()
	tableStyles.Selected = s.focus
	return table.New(table.WithStyles(tableStyles))
}

func (m *Model) syncInteractiveState() {
	if !m.hasSnapshot {
		return
	}
	width, height := m.dashboardSize()
	runningRows, runningColumns := m.runningTableRows(m.snapshot.Running, width)
	tableRows := runningRows
	if len(tableRows) == 0 {
		tableRows = []table.Row{{m.styles.muted.Render("No active agents")}}
	}
	demand := [dashboardSectionCount]int{
		len(runningRows),
		len(m.snapshot.Queue),
		len(m.snapshot.Blocked),
		len(m.snapshot.Completed),
	}
	caps := calculateSectionCaps(height, demand, m.collapsed)
	m.runningTable.SetColumns(runningColumns)
	m.runningTable.SetRows(tableRows)
	m.runningTable.SetWidth(max(width-4, 1))
	m.runningTable.SetHeight(caps.running + 1)
	if m.focusedSection == runningSection && !m.collapsed[runningSection] {
		m.runningTable.Focus()
	} else {
		m.runningTable.Blur()
	}
	m.clampOffsets()
}

func (m *Model) clampOffsets() {
	if !m.hasSnapshot {
		return
	}
	_, height := m.dashboardSize()
	demand := [dashboardSectionCount]int{
		len(m.snapshot.Running),
		len(m.snapshot.Queue),
		len(m.snapshot.Blocked),
		len(m.snapshot.Completed),
	}
	caps := calculateSectionCaps(height, demand, m.collapsed)
	visible := [dashboardSectionCount]int{caps.running, caps.queue, caps.blocked, caps.completed}
	for section := queueSection; section < dashboardSectionCount; section++ {
		if m.collapsed[section] || visible[section] <= 0 {
			m.offsets[section] = 0
			continue
		}
		m.offsets[section] = max(min(m.offsets[section], max(demand[section]-visible[section], 0)), 0)
	}
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
	lines = append(lines, sectionDivider("STATUS", 0, 0, 0, width, m.styles, false))
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
	m.syncInteractiveState()
	snapshot := m.snapshot
	runningRows, _ := m.runningTableRows(snapshot.Running, width)
	queueRows := m.queueRows(snapshot.Queue)
	blockedRows := m.blockedRows(snapshot.Blocked)
	completedRows := m.completedRows(snapshot.Completed)
	demand := [dashboardSectionCount]int{len(runningRows), len(queueRows), len(blockedRows), len(completedRows)}
	caps := calculateSectionCaps(height, demand, m.collapsed)

	lines := []string{m.renderTitleBorder(width)}
	for _, line := range m.snapshotHeader(snapshot) {
		lines = append(lines, frameContent(line, width))
	}

	counts := snapshot.EffectiveCounts()
	runningTotal := counts.Running
	if m.collapsed[runningSection] {
		lines = append(lines, collapsedSectionLine("RUNNING", runningTotal, width, m.styles, m.focusedSection == runningSection))
	} else {
		runningVisible := min(len(runningRows), caps.running)
		lines = append(lines, sectionDivider("RUNNING", runningTotal, len(runningRows), runningVisible, width, m.styles, m.focusedSection == runningSection))
		lines = append(lines, renderRunningTable(m.runningTable, caps.running, width)...)
	}

	sections := []dashboardSection{
		{
			label:       "QUEUE",
			total:       counts.Queue,
			rows:        queueRows,
			emptyLabel:  "No queued retries",
			visibleRows: caps.queue,
			offset:      m.offsets[queueSection],
			focused:     m.focusedSection == queueSection,
			collapsed:   m.collapsed[queueSection],
			styles:      m.styles,
		},
		{
			label:       "BLOCKED",
			total:       counts.Blocked,
			rows:        blockedRows,
			emptyLabel:  "No blocked work",
			visibleRows: caps.blocked,
			offset:      m.offsets[blockedSection],
			focused:     m.focusedSection == blockedSection,
			collapsed:   m.collapsed[blockedSection],
			styles:      m.styles,
		},
		{
			label:       "COMPLETED",
			total:       counts.Completed,
			rows:        completedRows,
			emptyLabel:  "No completed work",
			visibleRows: caps.completed,
			offset:      m.offsets[completedSection],
			focused:     m.focusedSection == completedSection,
			collapsed:   m.collapsed[completedSection],
			styles:      m.styles,
		},
	}
	for _, section := range sections {
		lines = append(lines, section.render(width)...)
	}

	for len(lines) < height-3 {
		lines = append(lines, frameContent("", width))
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

	effectiveCounts := snapshot.EffectiveCounts()
	counts := m.styles.ok.Render(fmt.Sprintf("● %d running", effectiveCounts.Running)) +
		m.styles.muted.Render("   ") +
		m.styles.warn.Render(fmt.Sprintf("◐ %d queued", effectiveCounts.Queue)) +
		m.styles.muted.Render("   ") +
		m.styles.error.Render(fmt.Sprintf("✗ %d blocked", effectiveCounts.Blocked)) +
		m.styles.muted.Render("   ") +
		m.styles.info.Render(fmt.Sprintf("✓ %d completed", effectiveCounts.Completed))

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
	} else if pendingUpdate, ok := formatPendingUpdate(snapshot.Update, m.styles); ok {
		statusLine = "Update: " + pendingUpdate
	}

	return []string{
		project + separator + instance + separator + m.styles.muted.Render("scope: ") + scope,
		counts,
		throughput + separator + tokens + separator + m.styles.muted.Render("budget ") + budget,
		statusLine,
	}
}

func calculateSectionCaps(height int, demand [dashboardSectionCount]int, collapsed [dashboardSectionCount]bool) dashboardSectionCaps {
	fixedLines := 12
	if !collapsed[runningSection] {
		fixedLines++
	}
	available := max(height-fixedLines, 0)
	for section := range demand {
		demand[section] = max(demand[section], 1)
	}
	caps := [dashboardSectionCount]int{}
	for section := dashboardSectionIndex(0); section < dashboardSectionCount && available > 0; section++ {
		if collapsed[section] {
			continue
		}
		caps[section] = 1
		available--
	}
	remaining := available
	order := [...]int{0, 3, 0, 1, 2}
	for remaining > 0 {
		allocated := false
		for _, section := range order {
			if remaining == 0 {
				break
			}
			if collapsed[section] || caps[section] >= demand[section] {
				continue
			}
			caps[section]++
			remaining--
			allocated = true
		}
		if !allocated {
			for _, section := range order {
				if !collapsed[section] {
					caps[section] += remaining
					remaining = 0
					break
				}
			}
			if remaining > 0 {
				break
			}
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

func renderRunningTable(component table.Model, visibleRows int, width int) []string {
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
	if s.collapsed {
		return []string{collapsedSectionLine(s.label, s.total, width, s.styles, s.focused)}
	}
	start := min(max(s.offset, 0), len(s.rows))
	visible := min(len(s.rows)-start, s.visibleRows)
	lines := []string{sectionDivider(s.label, s.total, len(s.rows), visible, width, s.styles, s.focused)}
	if s.visibleRows <= 0 {
		return lines
	}
	if visible == 0 {
		lines = append(lines, frameContent(s.styles.muted.Render(s.emptyLabel), width))
	} else {
		for _, row := range s.rows[start : start+visible] {
			lines = append(lines, frameContent(row, width))
		}
	}
	for len(lines) < s.visibleRows+1 {
		lines = append(lines, frameContent("", width))
	}
	return lines
}

func sectionDivider(label string, total int, rowCount int, visible int, width int, s styles, focused bool) string {
	if focused {
		label = "▶ " + label
	}
	title := fmt.Sprintf(" %s (%d) ", label, total)
	right := ""
	if overflow := max(rowCount-visible, 0); overflow > 0 {
		title = fmt.Sprintf(" %s (%d, showing %d) ", label, total, visible)
		right = fmt.Sprintf(" +%d more (j/k) ", overflow)
	}
	style := s.title
	if focused {
		style = s.focus
	}
	return titledBorder("├", "┤", title, right, width, style)
}

func collapsedSectionLine(label string, total int, width int, s styles, focused bool) string {
	style := s.title
	if focused {
		style = s.focus
	}
	return frameContent(style.Render(fmt.Sprintf("▸ %s (%d)", label, total)), width)
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
	left := m.styles.muted.Render("q quit · d dashboard · tab focus · 1-4 collapse · j/k/↑/↓ scroll · ? help")
	if width < 100 {
		left = m.styles.muted.Render("q quit · d open · tab · 1-4 fold · j/k/↑/↓ · ? help")
	}
	right := m.styles.muted.Render("logs " + defaultString(m.logPath, "n/a"))
	innerWidth := max(width-2, 0)
	contentWidth := max(innerWidth-2, 0)
	if ansi.StringWidth(left)+ansi.StringWidth(right)+1 > contentWidth {
		right = ansi.Truncate(right, max(contentWidth-ansi.StringWidth(left)-1, 0), "")
	}
	gap := max(contentWidth-ansi.StringWidth(left)-ansi.StringWidth(right), 1)
	return frameContent(left+strings.Repeat(" ", gap)+right, width)
}

func renderHelpOverlay(screen string, width int, height int, s styles) string {
	bindings := []string{
		"q / esc       quit",
		"ctrl+c       drain; press twice to force quit",
		"d            open web dashboard",
		"1-4          collapse Running / Queue / Blocked / Completed",
		"tab          focus next section",
		"j / ↓        scroll down",
		"k / ↑        scroll up",
		"?            show help; any key closes",
	}
	boxWidth := min(max(width-4, 1), 68)
	box := []string{titledBorder("┌", "┐", " HELP ", "", boxWidth, s.focus)}
	for _, binding := range bindings {
		box = append(box, frameContent(binding, boxWidth))
	}
	box = append(box, bottomBorder(boxWidth))
	if len(box) > height {
		box = box[:height]
	}
	x := max((width-boxWidth)/2, 0)
	y := max((height-len(box))/2, 0)
	lines := strings.Split(screen, "\n")
	for index, overlay := range box {
		lineIndex := y + index
		if lineIndex >= len(lines) {
			break
		}
		left := ansi.Cut(lines[lineIndex], 0, x)
		right := ansi.Cut(lines[lineIndex], x+boxWidth, width)
		lines[lineIndex] = fitANSI(left+overlay+right, width)
	}
	return strings.Join(lines, "\n")
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
