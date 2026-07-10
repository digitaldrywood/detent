package retro

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/digitaldrywood/detent/internal/intake"
)

const (
	TriggerCompletion  = "completion"
	TriggerDaily       = "daily"
	pendingStateMarker = "<!-- detent-retro-state:pending -->"
)

var (
	ErrMissingTelemetryStore = errors.New("retro telemetry store is required")
	ErrMissingProjectStore   = errors.New("retro project issue store is required")
	ErrMissingProductStore   = errors.New("retro product issue store is required")
)

type TelemetryStore interface {
	LoadRetroSnapshot(context.Context, string, time.Time) (Snapshot, error)
	RetroFiledOnDay(context.Context, string, time.Time) (int, error)
	RecordRetroRun(context.Context, RunRecord) error
}

type RunRecord struct {
	ProjectID   string
	Trigger     string
	StartedAt   time.Time
	CompletedAt time.Time
	Findings    int
	Filed       int
	Updated     int
	Error       string
}

type Settings struct {
	ProjectID     string
	Config        Config
	ProjectIssues intake.IssueStore
	ProductIssues intake.IssueStore
}

type Result struct {
	Findings []Finding
	Filed    int
	Updated  int
	Capped   int
}

type Manager struct {
	mu        sync.RWMutex
	processMu sync.Mutex
	settings  Settings
	store     TelemetryStore
	logger    *slog.Logger
	now       func() time.Time
	triggers  chan string
	updates   chan struct{}
	issues    map[string]intake.Issue
}

func New(settings Settings, store TelemetryStore, logger *slog.Logger, now func() time.Time) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	manager := &Manager{
		store:    store,
		logger:   logger,
		now:      now,
		triggers: make(chan string, 1),
		updates:  make(chan struct{}, 1),
		issues:   map[string]intake.Issue{},
	}
	if err := manager.Update(settings); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Update(settings Settings) error {
	m.processMu.Lock()
	defer m.processMu.Unlock()
	settings.ProjectID = strings.TrimSpace(settings.ProjectID)
	settings.Config.Normalize()
	if settings.Config.Enabled {
		if m.store == nil {
			return ErrMissingTelemetryStore
		}
		if settings.ProjectIssues == nil {
			return ErrMissingProjectStore
		}
		if settings.ProductIssues == nil {
			return ErrMissingProductStore
		}
	}
	m.mu.Lock()
	m.settings = settings
	m.mu.Unlock()
	m.issues = map[string]intake.Issue{}
	m.signalUpdate()
	return nil
}

func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings.Config.Enabled
}

func (m *Manager) Trigger(trigger string) {
	if m == nil || !m.Enabled() {
		return
	}
	trigger = strings.TrimSpace(trigger)
	if trigger == "" {
		trigger = TriggerCompletion
	}
	select {
	case m.triggers <- trigger:
	default:
	}
}

func (m *Manager) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		next, scheduled := m.nextScheduled(m.now())
		if !scheduled {
			select {
			case <-ctx.Done():
				return nil
			case <-m.updates:
				continue
			case trigger := <-m.triggers:
				m.runAndLog(ctx, trigger)
			}
			continue
		}

		delay := max(next.Sub(m.now()), 0)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil
		case <-m.updates:
			stopTimer(timer)
			continue
		case trigger := <-m.triggers:
			stopTimer(timer)
			m.runAndLog(ctx, trigger)
		case <-timer.C:
			m.runAndLog(ctx, TriggerDaily)
		}
	}
}

func (m *Manager) RunOnce(ctx context.Context, trigger string) (Result, error) {
	if m == nil {
		return Result{}, nil
	}
	m.processMu.Lock()
	defer m.processMu.Unlock()

	m.mu.RLock()
	settings := cloneSettings(m.settings)
	m.mu.RUnlock()
	if !settings.Config.Enabled {
		return Result{}, nil
	}

	startedAt := m.now().UTC()
	record := RunRecord{ProjectID: settings.ProjectID, Trigger: strings.TrimSpace(trigger), StartedAt: startedAt}
	if record.Trigger == "" {
		record.Trigger = TriggerCompletion
	}
	finish := func(result Result, runErr error) (Result, error) {
		record.CompletedAt = m.now().UTC()
		record.Findings = len(result.Findings)
		record.Filed = result.Filed
		record.Updated = result.Updated
		if runErr != nil {
			record.Error = runErr.Error()
		}
		if err := m.store.RecordRetroRun(ctx, record); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record retro run: %w", err))
		}
		return result, runErr
	}

	since := startedAt.AddDate(0, 0, -settings.Config.LookbackDays)
	snapshot, err := m.store.LoadRetroSnapshot(ctx, settings.ProjectID, since)
	if err != nil {
		return finish(Result{}, fmt.Errorf("load retro snapshot: %w", err))
	}
	findings := Detect(snapshot, DetectorOptions{
		FallbackThreshold:       settings.Config.FallbackThreshold,
		ReceiptBaselineMultiple: settings.Config.ReceiptBaselineMultiple,
	})
	qualified := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if Qualifies(finding, settings.Config.MinOccurrences, settings.Config.SingleOccurrenceSeverity) {
			qualified = append(qualified, finding)
		}
	}
	result := Result{Findings: qualified}
	filedToday, err := m.store.RetroFiledOnDay(ctx, settings.ProjectID, startedAt)
	if err != nil {
		return finish(result, fmt.Errorf("read retro daily issue count: %w", err))
	}
	remaining := settings.Config.DailyIssueCap - filedToday
	var runErr error
	for _, finding := range qualified {
		issueStore := settings.ProjectIssues
		if finding.Scope == ScopeProduct {
			issueStore = settings.ProductIssues
		}
		outcome, err := m.upsertFinding(ctx, issueStore, settings, finding, remaining > 0)
		if outcome.created {
			result.Filed++
			remaining--
		} else if outcome.updated {
			result.Updated++
		} else if outcome.capped {
			result.Capped++
		}
		if err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("upsert retro finding %s: %w", finding.Pattern, err))
			continue
		}
	}
	return finish(result, runErr)
}

type upsertOutcome struct {
	created bool
	updated bool
	capped  bool
}

func (m *Manager) upsertFinding(ctx context.Context, issueStore intake.IssueStore, settings Settings, finding Finding, allowCreate bool) (upsertOutcome, error) {
	fingerprint := Fingerprint(settings.ProjectID, finding)
	marker := "<!-- detent-retro fingerprint=" + fingerprint + " -->"
	draft := intake.IssueDraft{
		Title:  "[retro] " + finding.Title,
		Body:   findingIssueBody(settings.ProjectID, finding, marker),
		Labels: append([]string(nil), settings.Config.Labels...),
	}
	pendingDraft := draft
	pendingDraft.Body = draft.Body + "\n\n" + pendingStateMarker
	cacheKey := finding.Scope + "\x00" + marker
	issue, found := m.issues[cacheKey]
	if !found {
		var err error
		issue, found, err = issueStore.FindIntakeIssue(ctx, marker)
		if err != nil {
			return upsertOutcome{}, err
		}
	}
	if found {
		if strings.Contains(issue.Body, pendingStateMarker) {
			updated, err := issueStore.UpdateIntakeIssue(ctx, issue.ID, pendingDraft)
			if err != nil {
				return upsertOutcome{}, err
			}
			m.issues[cacheKey] = updated
			if err := issueStore.SetIntakeIssueState(ctx, issue.ID, settings.Config.TargetState); err != nil {
				return upsertOutcome{}, err
			}
		}
		draft.Body = preserveFindingOutcome(draft.Body, issue.Body)
		if strings.TrimSpace(issue.Body) == strings.TrimSpace(draft.Body) {
			m.issues[cacheKey] = issue
			return upsertOutcome{}, nil
		}
		updated, err := issueStore.UpdateIntakeIssue(ctx, issue.ID, draft)
		if err != nil {
			return upsertOutcome{}, err
		}
		m.issues[cacheKey] = updated
		return upsertOutcome{updated: true}, nil
	}
	if !allowCreate {
		return upsertOutcome{capped: true}, nil
	}
	issue, err := issueStore.CreateIntakeIssue(ctx, pendingDraft)
	if err != nil {
		return upsertOutcome{}, err
	}
	m.issues[cacheKey] = issue
	if err := issueStore.SetIntakeIssueState(ctx, issue.ID, settings.Config.TargetState); err != nil {
		return upsertOutcome{created: true}, err
	}
	updated, err := issueStore.UpdateIntakeIssue(ctx, issue.ID, draft)
	if err != nil {
		return upsertOutcome{created: true}, err
	}
	m.issues[cacheKey] = updated
	return upsertOutcome{created: true}, nil
}

func preserveFindingOutcome(draft string, existing string) string {
	status := findingOutcomeStatus(existing)
	if status == "" || status == "pending" {
		return draft
	}
	return strings.Replace(draft, "- status: pending", "- status: "+status, 1)
}

func findingOutcomeStatus(body string) string {
	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "- status:")
		if !ok {
			continue
		}
		status := strings.ToLower(strings.Trim(strings.TrimSpace(value), "`"))
		if status == "pending" || status == "accepted" || status == "rejected" {
			return status
		}
	}
	return ""
}

func findingIssueBody(projectID string, finding Finding, marker string) string {
	var builder strings.Builder
	builder.WriteString("```detent-agent\nschema: 1\neffort: high\n```\n\n")
	builder.WriteString(marker)
	builder.WriteString("\n\n## Efficiency finding\n\n")
	builder.WriteString(finding.Detail)
	builder.WriteString("\n\n- project: ")
	builder.WriteString(projectID)
	builder.WriteString("\n- classification: ")
	builder.WriteString(finding.Scope)
	builder.WriteString("\n- pattern: ")
	builder.WriteString(finding.Pattern)
	builder.WriteString("\n- severity: ")
	builder.WriteString(finding.Severity)
	builder.WriteString("\n- occurrences: ")
	builder.WriteString(strconv.Itoa(len(finding.Occurrences)))
	if finding.TokenDelta > 0 {
		builder.WriteString("\n- avoidable token delta: ")
		builder.WriteString(strconv.FormatInt(finding.TokenDelta, 10))
	}
	builder.WriteString("\n\n## Evidence\n")
	for _, occurrence := range finding.Occurrences {
		builder.WriteString("\n- ")
		builder.WriteString(occurrence.Issue)
		if !occurrence.At.IsZero() {
			builder.WriteString(" at ")
			builder.WriteString(occurrence.At.UTC().Format(time.RFC3339))
		}
		if occurrence.Tokens > 0 {
			builder.WriteString("; tokens=")
			builder.WriteString(strconv.FormatInt(occurrence.Tokens, 10))
		}
		if occurrence.Detail != "" {
			builder.WriteString("; ")
			builder.WriteString(strings.Join(strings.Fields(occurrence.Detail), " "))
		}
	}
	if finding.Proposal != nil {
		proposalID := Fingerprint(projectID, finding)[:12]
		builder.WriteString("\n\n## Proposed WORKFLOW.md change\n\n")
		builder.WriteString("<!-- detent:self-improvement proposal_id=detent-retro-")
		builder.WriteString(proposalID)
		builder.WriteString(" -->\n\n")
		builder.WriteString("- target: `")
		builder.WriteString(finding.Proposal.Path)
		builder.WriteString("`\n- change: ")
		builder.WriteString(finding.Proposal.Change)
	}
	builder.WriteString("\n\n## Governance\n\nThis is a proposal only. Detent must not modify WORKFLOW.md, prompts, or runtime configuration without human approval.\n\n## Outcome\n\n- status: pending\n- accepted/rejected record: change status to `accepted` or `rejected`; later retro runs update this fingerprinted issue instead of opening a duplicate.\n")
	return builder.String()
}

func (m *Manager) nextScheduled(now time.Time) (time.Time, bool) {
	m.mu.RLock()
	settings := cloneSettings(m.settings)
	m.mu.RUnlock()
	if !settings.Config.Enabled {
		return time.Time{}, false
	}
	schedule, err := cron.ParseStandard(settings.Config.Schedule)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(now), true
}

func (m *Manager) runAndLog(ctx context.Context, trigger string) {
	result, err := m.RunOnce(ctx, trigger)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.ErrorContext(ctx, "efficiency retro failed", "trigger", trigger, "error", err)
		}
		return
	}
	m.logger.InfoContext(ctx, "efficiency retro completed", "trigger", trigger, "findings", len(result.Findings), "filed", result.Filed, "updated", result.Updated, "capped", result.Capped)
}

func cloneSettings(settings Settings) Settings {
	settings.Config.Labels = append([]string(nil), settings.Config.Labels...)
	return settings
}

func (m *Manager) signalUpdate() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
