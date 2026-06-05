package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrNilHub = errors.New("nil telemetry hub")

const defaultDashboardURL = "http://localhost:4000"

type Option func(*options)

type options struct {
	now   func() time.Time
	build buildinfo.Info
}

type Model struct {
	subscription *hub.Subscription[telemetry.Snapshot]
	updates      <-chan telemetry.Snapshot
	snapshot     telemetry.Snapshot
	hasSnapshot  bool
	width        int
	height       int
	now          func() time.Time
	build        buildinfo.Info
	styles       styles
}

type snapshotMsg struct {
	snapshot telemetry.Snapshot
}

type subscriptionClosedMsg struct{}

func NewModel(ctx context.Context, snapshots *hub.Hub[telemetry.Snapshot], opts ...Option) (Model, error) {
	if snapshots == nil {
		return Model{}, ErrNilHub
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := options{now: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}

	subscription, err := snapshots.Subscribe(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("subscribe telemetry snapshots: %w", err)
	}

	return Model{
		subscription: subscription,
		updates:      subscription.C(),
		width:        defaultTerminalColumns,
		now:          cfg.now,
		build:        cfg.build,
		styles:       newStyles(),
	}, nil
}

func WithNow(now func() time.Time) Option {
	return func(cfg *options) {
		if now != nil {
			cfg.now = now
		}
	}
}

func WithBuild(build buildinfo.Info) Option {
	return func(cfg *options) {
		cfg.build = build
	}
}

func (m Model) Init() tea.Cmd {
	return waitForSnapshot(m.updates)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case snapshotMsg:
		m.snapshot = msg.snapshot
		m.hasSnapshot = true
		return m, waitForSnapshot(m.updates)
	case subscriptionClosedMsg:
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.Close()
			return m, tea.Quit
		default:
			return m, nil
		}
	default:
		return m, nil
	}
}

func (m Model) View() string {
	if !m.hasSnapshot {
		return m.renderWaiting()
	}

	return m.renderSnapshot()
}

func (m Model) Close() {
	if m.subscription != nil {
		m.subscription.Close()
	}
}

func waitForSnapshot(updates <-chan telemetry.Snapshot) tea.Cmd {
	if updates == nil {
		return nil
	}

	return func() tea.Msg {
		snapshot, ok := <-updates
		if !ok {
			return subscriptionClosedMsg{}
		}

		return snapshotMsg{snapshot: snapshot}
	}
}

func (m Model) renderWaiting() string {
	lines := []string{
		m.styles.title.Render("╭─ DETENT STATUS"),
		"│ Dashboard: " + m.styles.info.Render(defaultDashboardURL),
		"│ " + m.styles.muted.Render("Waiting for telemetry snapshot"),
		closingBorder,
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderSnapshot() string {
	snapshot := m.snapshot
	lines := []string{
		m.styles.title.Render("╭─ DETENT STATUS"),
		"│ Generated: " + m.styles.muted.Render(formatTimestamp(snapshot.GeneratedAt)),
	}
	if !buildinfo.IsZero(m.build) {
		lines = append(lines, "│ Build: "+m.styles.muted.Render(buildinfo.DisplayLabel(m.build)))
	}
	lines = append(lines, []string{
		"│ Agents: " + m.styles.ok.Render(fmt.Sprintf("%d running", countOrLen(snapshot.Counts.Running, len(snapshot.Running)))) +
			m.styles.muted.Render(" | ") +
			m.styles.warn.Render(fmt.Sprintf("%d queued", countOrLen(snapshot.Counts.Queue, len(snapshot.Queue)))) +
			m.styles.muted.Render(" | ") +
			m.styles.error.Render(fmt.Sprintf("%d blocked", countOrLen(snapshot.Counts.Blocked, len(snapshot.Blocked)))) +
			m.styles.muted.Render(" | ") +
			m.styles.info.Render(fmt.Sprintf("%d completed", countOrLen(snapshot.Counts.Completed, len(snapshot.Completed)))),
		"│ Throughput: " + m.styles.info.Render(formatTokenThroughput(snapshot.Throughput)),
		"│ Runtime: " + m.styles.accent.Render(formatRuntimeSeconds(snapshot.Tokens.RuntimeSeconds)),
		"│ Tokens: " + m.styles.warn.Render("in "+formatCount(snapshot.Tokens.Input)) +
			m.styles.muted.Render(" | ") +
			m.styles.warn.Render("out "+formatCount(snapshot.Tokens.Output)) +
			m.styles.muted.Render(" | ") +
			m.styles.warn.Render("total "+formatCount(snapshot.Tokens.Total)),
		"│ Budget: " + formatBudget(snapshot.Budget, m.styles),
		"│ Rate Limits: " + formatRateLimits(snapshot.RateLimits, m.now, m.styles),
		"│ Project: " + formatOptionalInfo(formatProject(snapshot.Project), m.styles),
		"│ Instance: " + formatOptionalInfo(formatInstance(snapshot.Instance), m.styles),
		"│ Scope: " + formatOptionalInfo(formatAuthorizationScope(snapshot.Instance), m.styles),
		"│ Dashboard: " + m.styles.info.Render(formatDashboardURL(snapshot)),
		"│ Next refresh: " + formatOptionalInfo(formatNextRefresh(snapshot.Refresh), m.styles),
		m.styles.title.Render("├─ Running"),
		"│",
	}...)

	runningWidth := runningEventWidth(m.width)
	lines = append(lines, runningTableHeader(runningWidth, m.styles), runningTableSeparator(runningWidth, m.styles))
	lines = append(lines, formatRunningRows(snapshot.Running, runningWidth, m.styles)...)
	lines = append(lines, m.styles.title.Render("├─ Backoff queue"), "│")
	lines = append(lines, formatQueueRows(snapshot.Queue, m.styles)...)
	lines = append(lines, m.styles.title.Render("├─ Blocked"), "│")
	lines = append(lines, formatBlockedRows(snapshot.Blocked, m.styles)...)
	lines = append(lines, m.styles.title.Render("├─ Completed"), "│")
	lines = append(lines, formatCompletedRows(snapshot.Completed, m.styles)...)
	lines = append(lines, closingBorder)

	return strings.Join(lines, "\n")
}

func formatRunningRows(running []telemetry.Running, eventWidth int, s styles) []string {
	if len(running) == 0 {
		return []string{"│  " + s.muted.Render("No active agents")}
	}

	rows := append([]telemetry.Running(nil), running...)
	sort.Slice(rows, func(i int, j int) bool {
		return issueLabel(rows[i].Issue) < issueLabel(rows[j].Issue)
	})

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		event := row.LastMessage
		if strings.TrimSpace(event) == "" {
			event = row.LastEvent
		}
		if strings.TrimSpace(event) == "" {
			event = "none"
		}

		statusStyle := statusStyle(row.LastEvent, s)
		cells := []string{
			formatCell(issueLabel(row.Issue), runningIDWidth, alignLeft),
			formatCell(defaultString(row.State, "unknown"), runningStageWidth, alignLeft),
			formatCell(defaultString(row.ProcessIdentity, "n/a"), runningProcessWidth, alignLeft),
			formatCell(formatRuntimeAndTurns(row.RuntimeSeconds, row.TurnCount), runningAgeWidth, alignLeft),
			formatCell(formatCount(row.Tokens.Total), runningTokensWidth, alignRight),
			formatCell(compactSessionID(row.SessionID), runningSessionWidth, alignLeft),
			formatCell(cleanInline(event), eventWidth, alignLeft),
		}

		lines = append(lines, "│ "+s.ok.Render("●")+" "+
			s.info.Render(cells[0])+" "+
			statusStyle.Render(cells[1])+" "+
			s.warn.Render(cells[2])+" "+
			s.accent.Render(cells[3])+" "+
			s.warn.Render(cells[4])+" "+
			s.info.Render(cells[5])+" "+
			statusStyle.Render(cells[6]))
	}

	return lines
}

func formatQueueRows(queue []telemetry.Queued, s styles) []string {
	if len(queue) == 0 {
		return []string{"│  " + s.muted.Render("No queued retries")}
	}

	rows := append([]telemetry.Queued(nil), queue...)
	sort.Slice(rows, func(i int, j int) bool {
		if rows[i].DueInMillis == rows[j].DueInMillis {
			return issueLabel(rows[i].Issue) < issueLabel(rows[j].Issue)
		}
		return rows[i].DueInMillis < rows[j].DueInMillis
	})

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		line := "│  " + s.warn.Render("↻") + " " +
			s.error.Render(issueLabel(row.Issue)) + " " +
			s.warn.Render(fmt.Sprintf("attempt=%d", row.Attempt)) +
			s.muted.Render(" in ") +
			s.info.Render(formatDueIn(row.DueInMillis))

		if errorText := cleanInline(row.Error); errorText != "" {
			line += " " + s.muted.Render("error="+truncate(errorText, 96))
		}

		lines = append(lines, line)
	}

	return lines
}

func formatBlockedRows(blocked []telemetry.Blocked, s styles) []string {
	if len(blocked) == 0 {
		return []string{"│  " + s.muted.Render("No blocked work")}
	}

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

		lines = append(lines, "│  "+s.error.Render("●")+" "+
			s.info.Render(issueLabel(row.Issue))+" "+
			s.error.Render(defaultString(row.State, "Blocked"))+" "+
			s.muted.Render(truncate(detail, 120)))
	}

	return lines
}

func formatCompletedRows(completed []telemetry.Completed, s styles) []string {
	if len(completed) == 0 {
		return []string{"│  " + s.muted.Render("No completed work")}
	}

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

		line := "│  " + s.ok.Render("✓") + " " +
			s.info.Render(issueLabel(row.Issue)) + " " +
			s.ok.Render(state) + " " +
			s.accent.Render(formatRuntimeAndTurns(row.RuntimeSeconds, row.Turns)) + " " +
			s.warn.Render(formatCount(row.Tokens.Total))

		if strings.TrimSpace(row.Model) != "" {
			line += " " + s.info.Render(row.Model)
		}

		lines = append(lines, line)
	}

	return lines
}

func formatBudget(budget telemetry.Budget, s styles) string {
	status := "disabled"
	statusStyle := s.muted
	if budget.Enabled {
		status = "enabled"
		statusStyle = s.ok
	}

	parts := []string{
		statusStyle.Render(status) + " " + s.muted.Render("current ") + s.warn.Render(formatUSD(budget.CurrentSpendUSD)),
		s.muted.Render("projected ") + s.warn.Render(formatUSD(budget.ProjectedCostUSD)),
		s.muted.Render("day max ") + s.info.Render(formatUSDCap(budget.PerDayMaxUSD)),
		s.muted.Render("issue max ") + s.info.Render(formatUSDCap(budget.PerIssueMaxUSD)),
	}

	return strings.Join(parts, s.muted.Render(" | "))
}

func formatRateLimits(rateLimits *telemetry.RateLimits, now func() time.Time, s styles) string {
	if rateLimits == nil {
		return s.muted.Render("unavailable")
	}

	limitID := rateLimits.LimitID
	if strings.TrimSpace(limitID) == "" {
		limitID = rateLimits.LimitName
	}
	if strings.TrimSpace(limitID) == "" {
		limitID = "unknown"
	}

	parts := []string{
		s.warn.Render(limitID),
		s.info.Render("primary " + formatRateLimitBucket(rateLimits.Primary, now)),
		s.info.Render("secondary " + formatRateLimitBucket(rateLimits.Secondary, now)),
		s.ok.Render(formatRateLimitCredits(rateLimits.Credits, now)),
	}

	return strings.Join(parts, s.muted.Render(" | "))
}

func formatRateLimitCredits(bucket *telemetry.RateLimitBucket, now func() time.Time) string {
	if bucket == nil {
		return "credits n/a"
	}
	if bucket.Unlimited {
		return "credits unlimited"
	}
	if bucket.HasCredits {
		if strings.TrimSpace(bucket.Balance) != "" {
			return "credits " + strings.TrimSpace(bucket.Balance)
		}
		return "credits available"
	}
	if bucket.Limit > 0 || bucket.Remaining > 0 || bucket.Used > 0 || bucket.ResetAt != nil || bucket.ResetInSeconds > 0 {
		return "credits " + formatRateLimitBucket(bucket, now)
	}
	return "credits none"
}

func formatRateLimitBucket(bucket *telemetry.RateLimitBucket, now func() time.Time) string {
	if bucket == nil {
		return "n/a"
	}

	var base string
	switch {
	case bucket.Limit > 0:
		base = formatCount(bucket.Remaining) + "/" + formatCount(bucket.Limit)
	case bucket.Remaining > 0:
		base = "remaining " + formatCount(bucket.Remaining)
	case bucket.Remaining == 0:
		base = "remaining 0"
	default:
		base = "n/a"
	}

	reset := formatReset(bucket, now)
	if reset == "" {
		return base
	}

	return base + " reset " + reset
}

func formatReset(bucket *telemetry.RateLimitBucket, now func() time.Time) string {
	if bucket.ResetInSeconds > 0 {
		return formatCount(bucket.ResetInSeconds) + "s"
	}
	if bucket.ResetAt == nil {
		return ""
	}
	if now == nil {
		now = time.Now
	}

	seconds := int64(math.Ceil(bucket.ResetAt.Sub(now()).Seconds()))
	if seconds < 0 {
		seconds = 0
	}

	return formatCount(seconds) + "s"
}

func runningTableHeader(eventWidth int, s styles) string {
	cells := []string{
		formatCell("ID", runningIDWidth, alignLeft),
		formatCell("STAGE", runningStageWidth, alignLeft),
		formatCell("PID", runningProcessWidth, alignLeft),
		formatCell("AGE / TURN", runningAgeWidth, alignLeft),
		formatCell("TOKENS", runningTokensWidth, alignLeft),
		formatCell("SESSION", runningSessionWidth, alignLeft),
		formatCell("EVENT", eventWidth, alignLeft),
	}

	return "│   " + s.muted.Render(strings.Join(cells, " "))
}

func runningTableSeparator(eventWidth int, s styles) string {
	width := runningIDWidth + runningStageWidth + runningProcessWidth + runningAgeWidth + runningTokensWidth + runningSessionWidth + eventWidth + runningColumnGaps
	return "│   " + s.muted.Render(strings.Repeat("─", width))
}

func statusStyle(event string, s styles) lipgloss.Style {
	switch event {
	case "":
		return s.error
	case "codex/event/token_count":
		return s.warn
	case "codex/event/task_started":
		return s.ok
	case "turn_completed":
		return s.accent
	default:
		return s.info
	}
}

func issueLabel(issue telemetry.Issue) string {
	for _, value := range []string{issue.Identifier, issue.ID} {
		if strings.TrimSpace(value) != "" {
			return cleanInline(value)
		}
	}

	return "unknown"
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return cleanInline(value)
}

func compactSessionID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "n/a"
	}
	sessionID = cleanInline(sessionID)
	if len(sessionID) <= 10 {
		return sessionID
	}
	runes := []rune(sessionID)
	if len(runes) <= 10 {
		return sessionID
	}
	return string(runes[:4]) + "..." + string(runes[len(runes)-6:])
}

func countOrLen(count int, length int) int {
	if count > 0 {
		return count
	}

	return length
}

func formatTimestamp(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}

	return value.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func formatProject(project telemetry.Project) string {
	if strings.TrimSpace(project.URL) != "" {
		return cleanInline(project.URL)
	}
	if strings.TrimSpace(project.DisplayName) != "" {
		return cleanInline(project.DisplayName)
	}
	return ""
}

func formatInstance(instance telemetry.Instance) string {
	name := cleanInline(instance.Name)
	login := cleanInline(instance.GitHubLogin)
	switch {
	case name != "" && login != "":
		return name + " (" + login + ")"
	case name != "":
		return name
	case login != "":
		return login
	default:
		return ""
	}
}

func formatAuthorizationScope(instance telemetry.Instance) string {
	if strings.TrimSpace(instance.AuthorizationScope) != "" {
		return cleanInline(instance.AuthorizationScope)
	}
	return "All issues"
}

func formatDashboardURL(snapshot telemetry.Snapshot) string {
	if strings.TrimSpace(snapshot.DashboardURL) != "" {
		return cleanInline(snapshot.DashboardURL)
	}
	return defaultDashboardURL
}

func formatNextRefresh(refresh telemetry.Refresh) string {
	if refresh.NextRefreshAt == nil {
		return ""
	}
	return formatTimestamp(*refresh.NextRefreshAt)
}

func formatOptionalInfo(value string, s styles) string {
	if strings.TrimSpace(value) == "" {
		return s.muted.Render("n/a")
	}
	return s.info.Render(value)
}

func formatRuntimeAndTurns(seconds float64, turns int) string {
	runtime := formatRuntimeSeconds(seconds)
	if turns > 0 {
		return fmt.Sprintf("%s / %d", runtime, turns)
	}

	return runtime
}

func formatTokenThroughput(throughput telemetry.TokenThroughput) string {
	if throughput.TokensPerSecond <= 0 || math.IsNaN(throughput.TokensPerSecond) || math.IsInf(throughput.TokensPerSecond, 0) {
		return "0 tps"
	}
	return formatCount(int64(math.Round(throughput.TokensPerSecond))) + " tps"
}

func formatRuntimeSeconds(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}

	total := int64(math.Round(seconds))
	minutes := total / 60
	remainder := total % 60

	return fmt.Sprintf("%dm %ds", minutes, remainder)
}

func formatDueIn(milliseconds int64) string {
	if milliseconds < 0 {
		milliseconds = 0
	}

	seconds := milliseconds / 1000
	millis := milliseconds % 1000

	return fmt.Sprintf("%d.%03ds", seconds, millis)
}

func formatCount(value int64) string {
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}

	text := fmt.Sprintf("%d", value)
	for i := len(text) - 3; i > 0; i -= 3 {
		text = text[:i] + "," + text[i:]
	}

	return sign + text
}

func formatUSD(value float64) string {
	return fmt.Sprintf("$%.2f", value)
}

func formatUSDCap(value *float64) string {
	if value == nil {
		return "n/a"
	}

	return formatUSD(*value)
}

type align int

const (
	_ align = iota
	alignLeft
	alignRight
)

func formatCell(value string, width int, alignment align) string {
	text := truncate(cleanInline(value), width)
	if alignment == alignRight {
		return fmt.Sprintf("%*s", width, text)
	}

	return fmt.Sprintf("%-*s", width, text)
}

func cleanInline(value string) string {
	value = strings.NewReplacer(
		`\r\n`, " ",
		`\r`, " ",
		`\n`, " ",
		"\r\n", " ",
		"\r", " ",
		"\n", " ",
	).Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}

	return string(runes[:width-3]) + "..."
}

func runningEventWidth(columns int) int {
	if columns <= 0 {
		columns = defaultTerminalColumns
	}

	width := columns - fixedRunningWidth - runningRowChromeWidth
	if width < runningEventMinWidth {
		return runningEventMinWidth
	}

	return width
}

type styles struct {
	title  lipgloss.Style
	ok     lipgloss.Style
	info   lipgloss.Style
	warn   lipgloss.Style
	error  lipgloss.Style
	accent lipgloss.Style
	muted  lipgloss.Style
}

func newStyles() styles {
	return styles{
		title:  lipgloss.NewStyle().Bold(true),
		ok:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		info:   lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		warn:   lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		error:  lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		accent: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		muted:  lipgloss.NewStyle().Faint(true),
	}
}

const (
	defaultTerminalColumns = 123
	runningIDWidth         = 8
	runningStageWidth      = 14
	runningProcessWidth    = 12
	runningAgeWidth        = 12
	runningTokensWidth     = 10
	runningSessionWidth    = 18
	runningEventMinWidth   = 12
	runningRowChromeWidth  = 10
	runningColumnGaps      = 6
	fixedRunningWidth      = runningIDWidth + runningStageWidth + runningProcessWidth + runningAgeWidth + runningTokensWidth + runningSessionWidth
	closingBorder          = "╰─"
)
