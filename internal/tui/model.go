package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrNilHub = errors.New("nil telemetry hub")

const defaultDashboardURL = "http://localhost:4000"

const (
	shutdownDrainNotice = "shutdown requested; draining sessions; press Ctrl+C again to force quit immediately"
	shutdownForceNotice = "force quit requested; interrupting sessions"
)

type Option func(*options)

type options struct {
	now                   func() time.Time
	build                 buildinfo.Info
	interrupt             func()
	shutdownTimeoutSource func() time.Duration
	logPath               string
	launcher              func(context.Context, string) error
}

type dashboardSectionIndex int

const (
	runningSection dashboardSectionIndex = iota
	queueSection
	blockedSection
	completedSection
	dashboardSectionCount
)

type Model struct {
	subscription          *hub.Subscription[telemetry.Snapshot]
	updates               <-chan telemetry.Snapshot
	snapshot              telemetry.Snapshot
	hasSnapshot           bool
	width                 int
	height                int
	now                   func() time.Time
	build                 buildinfo.Info
	interrupt             func()
	shutdownTimeoutSource func() time.Duration
	interrupts            int
	shutdownNote          string
	logPath               string
	launcher              func(context.Context, string) error
	collapsed             [dashboardSectionCount]bool
	offsets               [dashboardSectionCount]int
	focusedSection        dashboardSectionIndex
	helpVisible           bool
	runningTable          table.Model
	styles                styles
}

type snapshotMsg struct {
	snapshot telemetry.Snapshot
}

type subscriptionClosedMsg struct{}

type shutdownInterruptMsg struct {
	force bool
}

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

	modelStyles := newStyles()
	launcher := cfg.launcher
	if launcher == nil {
		launcher = func(_ context.Context, url string) error {
			return launchDashboard(ctx, url)
		}
	}
	return Model{
		subscription:          subscription,
		updates:               subscription.C(),
		width:                 defaultTerminalWidth,
		height:                defaultTerminalHeight,
		now:                   cfg.now,
		build:                 cfg.build,
		interrupt:             cfg.interrupt,
		shutdownTimeoutSource: cfg.shutdownTimeoutSource,
		logPath:               cfg.logPath,
		launcher:              launcher,
		runningTable:          newRunningTable(modelStyles),
		styles:                modelStyles,
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

func WithInterruptFunc(interrupt func()) Option {
	return func(cfg *options) {
		cfg.interrupt = interrupt
	}
}

func WithShutdownTimeoutSource(source func() time.Duration) Option {
	return func(cfg *options) {
		cfg.shutdownTimeoutSource = source
	}
}

func WithLogPath(path string) Option {
	return func(cfg *options) {
		cfg.logPath = strings.TrimSpace(path)
	}
}

func WithDashboardLauncher(launcher func(context.Context, string) error) Option {
	return func(cfg *options) {
		cfg.launcher = launcher
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
		m.syncInteractiveState()
		return m, nil
	case snapshotMsg:
		m.snapshot = msg.snapshot
		m.hasSnapshot = true
		m.syncInteractiveState()
		return m, waitForSnapshot(m.updates)
	case subscriptionClosedMsg:
		return m, nil
	case shutdownInterruptMsg:
		if m.interrupt != nil {
			m.interrupt()
		}
		if msg.force {
			m.Close()
			return m, tea.Quit
		}
		return m, nil
	case tea.InterruptMsg:
		return m.handleInterrupt()
	case tea.KeyPressMsg:
		if m.helpVisible {
			m.helpVisible = false
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			return m.handleInterrupt()
		case "q", "esc":
			m.Close()
			return m, tea.Quit
		case "?":
			m.helpVisible = true
			return m, nil
		case "d":
			return m, m.openDashboardCmd()
		case "1", "2", "3", "4":
			section := dashboardSectionIndex(int(msg.String()[0] - '1'))
			m.collapsed[section] = !m.collapsed[section]
			m.syncInteractiveState()
			return m, nil
		case "tab":
			m.focusedSection = (m.focusedSection + 1) % dashboardSectionCount
			m.syncInteractiveState()
			return m, nil
		case "j", "down":
			m.scrollFocusedSection(msg, 1)
			return m, nil
		case "k", "up":
			m.scrollFocusedSection(msg, -1)
			return m, nil
		default:
			return m, nil
		}
	default:
		return m, nil
	}
}

func (m Model) handleInterrupt() (tea.Model, tea.Cmd) {
	if m.interrupt == nil {
		m.Close()
		return m, tea.Quit
	}

	m.interrupts++
	m.shutdownNote = shutdownForceNotice
	if m.interrupts == 1 {
		m.shutdownNote = shutdownDrainNotice
	}

	force := m.interrupts > 1
	return m, func() tea.Msg {
		return shutdownInterruptMsg{force: force}
	}
}

func (m Model) openDashboardCmd() tea.Cmd {
	launcher := m.launcher
	if launcher == nil {
		launcher = launchDashboard
	}
	url := formatDashboardURL(m.snapshot)
	return func() tea.Msg {
		if err := launcher(context.Background(), url); err != nil {
			slog.Default().Warn("open terminal dashboard failed", "url", url, "error", err)
		}
		return nil
	}
}

func launchDashboard(ctx context.Context, url string) error {
	command, err := dashboardCommand(ctx, runtime.GOOS, url)
	if err != nil {
		return err
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("open dashboard: %w", err)
	}
	return nil
}

func dashboardCommand(ctx context.Context, goos string, url string) (*exec.Cmd, error) {
	switch goos {
	case "darwin":
		return exec.CommandContext(ctx, "open", url), nil
	case "linux":
		return exec.CommandContext(ctx, "xdg-open", url), nil
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url), nil
	default:
		return nil, fmt.Errorf("open dashboard: unsupported operating system %s", goos)
	}
}

func (m *Model) scrollFocusedSection(msg tea.KeyPressMsg, delta int) {
	if m.collapsed[m.focusedSection] {
		return
	}
	if m.focusedSection == runningSection {
		m.runningTable, _ = m.runningTable.Update(msg)
		return
	}
	m.offsets[m.focusedSection] += delta
	m.clampOffsets()
}

func (m Model) View() tea.View {
	view := tea.NewView(m.renderDashboard())
	view.AltScreen = true
	view.DisableBracketedPasteMode = true
	return view
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

func formatLifecycle(shutdown telemetry.Shutdown, now func() time.Time, timeoutSource func() time.Duration, s styles) (string, string) {
	if shutdown.Draining {
		status := fmt.Sprintf("draining (%d sessions remaining", shutdown.SessionsRemaining)
		if shutdown.RequestedAt != nil && timeoutSource != nil {
			if now == nil {
				now = time.Now
			}
			remaining := timeoutSource() - now().Sub(*shutdown.RequestedAt)
			if remaining < 0 {
				remaining = 0
			}
			status += ", " + formatRuntimeSeconds(remaining.Seconds()) + " until force quit"
		}
		status += "; press Ctrl+C again to force quit immediately)"
		return "Shutdown", s.warn.Render(status)
	}
	status := strings.TrimSpace(shutdown.Status)
	if status == "" {
		status = "running"
	}
	if strings.EqualFold(status, "running") {
		return "Lifecycle", s.ok.Render(status)
	}
	return "Shutdown", s.warn.Render(status)
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

	seconds := max(int64(math.Ceil(bucket.ResetAt.Sub(now()).Seconds())), 0)

	return formatCount(seconds) + "s"
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

func issueDisplayLabel(issue telemetry.Issue, width int) string {
	label := issueLabel(issue)
	if width <= 0 || runeLen(label) <= width {
		return label
	}

	compact := compactIssueLabel(label)
	if compact != label && runeLen(compact) <= width {
		return compact
	}

	return label
}

func compactIssueLabel(label string) string {
	if key, ok := githubIssueKey(label); ok {
		return key
	}

	return label
}

func githubIssueKey(label string) (string, bool) {
	label = strings.TrimSpace(cleanInline(label))
	hash := strings.LastIndex(label, "#")
	if hash < 0 || hash == len(label)-1 || !strings.Contains(label[:hash], "/") {
		return "", false
	}

	number := label[hash+1:]
	for _, r := range number {
		if r < '0' || r > '9' {
			return "", false
		}
	}

	return "#" + number, true
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

func formatCacheReadSummary(tokens telemetry.Tokens, s styles) string {
	fraction, ok := tokens.CacheReadFraction()
	if !ok {
		return ""
	}
	return s.muted.Render(" | ") + s.info.Render("cache "+formatContextPercent(fraction*100))
}

func formatRunningTokenPressure(tokens telemetry.Tokens) string {
	label := formatCount(tokens.Total)
	pressure, ok := tokens.ContextPressure()
	if !ok {
		return label
	}
	return label + "/" + formatContextPercent(pressure.PercentUsed)
}

func runningTokenStyle(tokens telemetry.Tokens, s styles) lipgloss.Style {
	pressure, ok := tokens.ContextPressure()
	if !ok {
		return s.warn
	}
	switch pressure.ThresholdState {
	case telemetry.ContextPressureCritical:
		return s.error
	case telemetry.ContextPressureWarning, telemetry.ContextPressureWatch:
		return s.warn
	case telemetry.ContextPressureNormal:
		return s.ok
	default:
		return s.warn
	}
}

func formatContextPercent(percent float64) string {
	if percent < 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		percent = 0
	}
	return formatCount(int64(math.Round(percent))) + "%"
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

	text := strconv.FormatInt(value, 10)
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

func runeLen(value string) int {
	return len([]rune(value))
}

type styles struct {
	title  lipgloss.Style
	ok     lipgloss.Style
	info   lipgloss.Style
	warn   lipgloss.Style
	error  lipgloss.Style
	accent lipgloss.Style
	muted  lipgloss.Style
	focus  lipgloss.Style
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
		focus:  lipgloss.NewStyle().Bold(true).Reverse(true),
	}
}

const (
	defaultTerminalWidth  = 80
	defaultTerminalHeight = 24
)
