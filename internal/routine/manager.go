package routine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/provenance"
	routinemodel "github.com/digitaldrywood/detent/internal/routine/model"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const (
	IssueLabel              = "detent:todo"
	IssueState              = "Todo"
	ProposalToolName        = "propose_maintenance_issue"
	storeRetryInterval      = time.Minute
	maxProposalDedupKeySize = 256
	maxProposalTitleSize    = 256
	maxProposalBodySize     = 256 * 1024
)

var (
	ErrMissingStore          = errors.New("routine run store is required")
	ErrMissingIssueStore     = errors.New("routine issue store is required")
	ErrMissingRunner         = errors.New("routine runner is required")
	ErrRoutineNotFound       = errors.New("routine not found")
	ErrInvalidProposal       = errors.New("routine issue proposal is invalid")
	ErrInvalidProposalOutput = errors.New("routine agent output is invalid")
)

type Store interface {
	LatestRoutineRun(context.Context, string, string) (RunRecord, bool, error)
	RecordRoutineRun(context.Context, RunRecord) error
}

type IssueStore interface {
	FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error)
	CreateIntakeIssue(context.Context, intake.IssueDraft) (intake.Issue, error)
	SetIntakeIssueState(context.Context, string, string) error
}

type RunRecord = routinemodel.RunRecord

type IssueRecord = routinemodel.IssueRecord

type Proposal struct {
	DedupKey string `json:"dedup_key"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

type Result struct {
	Filed        []IssueRecord
	Deduplicated int
}

type Settings struct {
	ProjectID    string
	Definitions  []config.Routine
	SearchStates []string
	Runner       runner.Backend
	Issues       IssueStore
	Metrics      workflowmetrics.Recorder
}

type Manager struct {
	mu        sync.RWMutex
	processMu sync.Mutex
	settings  Settings
	store     Store
	logger    *slog.Logger
	now       func() time.Time
	updates   chan struct{}
	baselines map[string]time.Time
}

func New(settings Settings, store Store, logger *slog.Logger, now func() time.Time) (*Manager, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if now == nil {
		now = time.Now
	}
	manager := &Manager{
		store:     store,
		logger:    logger,
		now:       now,
		updates:   make(chan struct{}, 1),
		baselines: map[string]time.Time{},
	}
	if err := manager.Update(settings); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Update(settings Settings) error {
	if m == nil {
		return ErrMissingStore
	}
	m.processMu.Lock()
	defer m.processMu.Unlock()

	settings = normalizeSettings(settings)
	if problems := config.ValidateRoutines("routines", settings.Definitions); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	if len(settings.Definitions) > 0 {
		if m.store == nil {
			return ErrMissingStore
		}
		if settings.Runner == nil {
			return ErrMissingRunner
		}
		if settings.Issues == nil {
			return ErrMissingIssueStore
		}
	}

	m.mu.RLock()
	previous := cloneSettings(m.settings)
	previousBaselines := make(map[string]time.Time, len(m.baselines))
	for name, value := range m.baselines {
		previousBaselines[name] = value
	}
	m.mu.RUnlock()
	baseline := m.now()
	baselines := make(map[string]time.Time, len(settings.Definitions))
	for _, definition := range settings.Definitions {
		baselines[definition.Name] = baseline
		if prior, ok := routineByName(previous.Definitions, definition.Name); ok && prior.Schedule == definition.Schedule {
			baselines[definition.Name] = previousBaselines[definition.Name]
		}
	}
	m.mu.Lock()
	m.settings = cloneSettings(settings)
	m.baselines = baselines
	m.mu.Unlock()
	m.signalUpdate()
	return nil
}

func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.settings.Definitions) > 0
}

func (m *Manager) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		next, name, scheduled, err := m.nextScheduled(ctx)
		if err != nil {
			m.logger.ErrorContext(ctx, "scheduled routine lookup failed", "error", err)
			timer := time.NewTimer(storeRetryInterval)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return nil
			case <-m.updates:
				stopTimer(timer)
				continue
			case <-timer.C:
				continue
			}
		}
		if !scheduled {
			select {
			case <-ctx.Done():
				return nil
			case <-m.updates:
				continue
			}
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
		case <-timer.C:
			m.runAndLog(ctx, name, next)
		}
	}
}

func (m *Manager) RunOnce(ctx context.Context, name string) (Result, error) {
	return m.runNamed(ctx, name, m.now(), false)
}

func (m *Manager) runNamed(ctx context.Context, name string, scheduledFor time.Time, scheduled bool) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.processMu.Lock()
	defer m.processMu.Unlock()

	m.mu.RLock()
	settings := cloneSettings(m.settings)
	baseline := m.baselines[strings.ToLower(strings.TrimSpace(name))]
	m.mu.RUnlock()
	definition, ok := routineByName(settings.Definitions, name)
	if !ok {
		if scheduled {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("%w: %s", ErrRoutineNotFound, strings.TrimSpace(name))
	}
	if scheduled {
		next, err := m.nextForDefinition(ctx, settings, definition, baseline)
		if err != nil {
			return Result{}, err
		}
		if !next.Equal(scheduledFor) {
			m.logger.DebugContext(ctx, "skip stale scheduled routine", "routine", definition.Name, "scheduled_for", scheduledFor, "next", next)
			return Result{}, nil
		}
	}
	result, err := m.runOnce(ctx, settings, definition, scheduledFor)
	baselineAt := scheduledFor
	if completedAt := m.now(); completedAt.After(baselineAt) {
		baselineAt = completedAt
	}
	m.mu.Lock()
	if baseline, exists := m.baselines[definition.Name]; exists && baselineAt.After(baseline) {
		m.baselines[definition.Name] = baselineAt
	}
	m.mu.Unlock()
	if !scheduled {
		m.signalUpdate()
	}
	return result, err
}

func (m *Manager) runOnce(ctx context.Context, settings Settings, definition config.Routine, scheduledFor time.Time) (result Result, runErr error) {
	startedAt := m.now().UTC()
	record := RunRecord{
		ProjectID:    settings.ProjectID,
		RoutineName:  definition.Name,
		ScheduledFor: scheduledFor.UTC(),
		StartedAt:    startedAt,
	}
	defer func() {
		record.CompletedAt = m.now().UTC()
		record.Filed = len(result.Filed)
		record.Deduplicated = result.Deduplicated
		record.Issues = append([]IssueRecord{}, result.Filed...)
		if runErr != nil {
			record.Error = runErr.Error()
		}
		if err := m.store.RecordRoutineRun(context.WithoutCancel(ctx), record); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record routine run: %w", err))
		}
	}()

	collector := &proposalCollector{}
	runResult, err := settings.Runner.Run(ctx, runner.RunRequest{
		Issue:            routineIssue(settings.ProjectID, definition),
		Mode:             runner.RunModeRoutine,
		StartedAt:        startedAt,
		Routine:          &runner.RoutineRequest{Name: definition.Name, Schedule: definition.Schedule, Prompt: definition.Prompt},
		AgentTools:       []runner.AgentTool{proposalTool()},
		AgentToolHandler: collector.handle,
	})
	if err != nil {
		return result, fmt.Errorf("run routine agent: %w", err)
	}
	if runResult.BudgetRefusal != nil {
		return result, errors.New("run routine agent: budget refused")
	}
	if runResult.FinalState != "" && runResult.FinalState != runner.FinalStateCompleted {
		return result, fmt.Errorf("run routine agent: final state %s", runResult.FinalState)
	}

	proposals, proposalErr := collector.result()
	if len(proposals) == 0 && proposalErr == nil {
		proposals, proposalErr = parseProposals(runResult.Output)
	}
	if proposalErr != nil {
		return result, proposalErr
	}
	return m.fileProposals(ctx, settings, definition, proposals)
}

func (m *Manager) fileProposals(ctx context.Context, settings Settings, definition config.Routine, proposals []Proposal) (Result, error) {
	if len(proposals) == 0 {
		return Result{}, nil
	}
	issues, err := settings.Issues.FetchIssuesByStates(ctx, settings.SearchStates)
	if err != nil {
		return Result{}, fmt.Errorf("find open routine issues: %w", err)
	}
	openMarkers := map[string]struct{}{}
	for _, issue := range issues {
		if issue.Closed {
			continue
		}
		for _, proposal := range proposals {
			marker := proposalMarker(settings.ProjectID, definition.Name, proposal.DedupKey)
			if strings.Contains(issue.Description, marker) {
				openMarkers[marker] = struct{}{}
			}
		}
	}

	result := Result{}
	var runErr error
	for _, proposal := range proposals {
		proposal, err = normalizeProposal(proposal)
		if err != nil {
			runErr = errors.Join(runErr, err)
			continue
		}
		marker := proposalMarker(settings.ProjectID, definition.Name, proposal.DedupKey)
		if _, ok := openMarkers[marker]; ok {
			result.Deduplicated++
			continue
		}
		issue, createErr := settings.Issues.CreateIntakeIssue(ctx, intake.IssueDraft{
			Title:  proposal.Title,
			Body:   marker + "\n\n" + proposal.Body,
			Labels: []string{IssueLabel},
		})
		if createErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("file routine issue %s: %w", proposal.DedupKey, createErr))
			continue
		}
		record := IssueRecord{ID: issue.ID, Identifier: issue.Identifier, URL: issue.URL}
		result.Filed = append(result.Filed, record)
		openMarkers[marker] = struct{}{}
		if stateErr := settings.Issues.SetIntakeIssueState(ctx, issue.ID, IssueState); stateErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("set routine issue %s state: %w", proposal.DedupKey, stateErr))
			continue
		}
		if metricsErr := workflowmetrics.RecordLaneTransition(ctx, settings.Metrics, workflowmetrics.LaneTransition{
			ProjectID: settings.ProjectID,
			Issue: connector.Issue{
				ID:         issue.ID,
				Identifier: issue.Identifier,
				URL:        issue.URL,
			},
			TargetState:  IssueState,
			At:           m.now().UTC(),
			Reason:       "routine",
			MetadataJSON: provenance.Apply("{}", provenance.Attribution{Origin: provenance.OriginRoutine}, nil),
		}); metricsErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record routine issue %s state provenance: %w", proposal.DedupKey, metricsErr))
		}
	}
	return result, runErr
}

func (m *Manager) nextScheduled(ctx context.Context) (time.Time, string, bool, error) {
	m.mu.RLock()
	settings := cloneSettings(m.settings)
	baselines := make(map[string]time.Time, len(m.baselines))
	for name, baseline := range m.baselines {
		baselines[name] = baseline
	}
	m.mu.RUnlock()

	var earliest time.Time
	earliestName := ""
	for _, definition := range settings.Definitions {
		next, err := m.nextForDefinition(ctx, settings, definition, baselines[definition.Name])
		if err != nil {
			return time.Time{}, "", false, err
		}
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
			earliestName = definition.Name
		}
	}
	return earliest, earliestName, !earliest.IsZero(), nil
}

func (m *Manager) nextForDefinition(ctx context.Context, settings Settings, definition config.Routine, baseline time.Time) (time.Time, error) {
	schedule, err := cron.ParseStandard(definition.Schedule)
	if err != nil {
		return time.Time{}, err
	}
	last, found, err := m.store.LatestRoutineRun(ctx, settings.ProjectID, definition.Name)
	if err != nil {
		return time.Time{}, err
	}
	location := m.now().Location()
	after := baseline.In(location)
	if found && last.StartedAt.After(after) {
		after = last.StartedAt.In(location)
	}
	return schedule.Next(after), nil
}

func Due(schedule string, lastRun time.Time, now time.Time) (bool, error) {
	parsed, err := cron.ParseStandard(strings.TrimSpace(schedule))
	if err != nil {
		return false, err
	}
	if lastRun.IsZero() {
		return false, nil
	}
	return !parsed.Next(lastRun).After(now), nil
}

func proposalTool() runner.AgentTool {
	return runner.AgentTool{
		Name:        ProposalToolName,
		Description: "Record one actionable maintenance issue proposal for Detent to validate, deduplicate, and file after the routine finishes.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["dedup_key","title","body"],"properties":{"dedup_key":{"type":"string","minLength":1,"maxLength":256},"title":{"type":"string","minLength":1,"maxLength":256},"body":{"type":"string","minLength":1,"maxLength":262144}}}`),
	}
}

type proposalCollector struct {
	mu        sync.Mutex
	proposals []Proposal
	errs      []error
}

func (c *proposalCollector) handle(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	if strings.TrimSpace(call.Name) != ProposalToolName {
		err := fmt.Errorf("%w: unsupported tool %s", ErrInvalidProposal, strings.TrimSpace(call.Name))
		c.addError(err)
		return runner.AgentToolResult{Content: err.Error(), Success: false}, nil
	}
	var proposal Proposal
	if err := json.Unmarshal(call.Arguments, &proposal); err != nil {
		err = fmt.Errorf("%w: decode proposal: %w", ErrInvalidProposal, err)
		c.addError(err)
		return runner.AgentToolResult{Content: err.Error(), Success: false}, nil
	}
	proposal, err := normalizeProposal(proposal)
	if err != nil {
		c.addError(err)
		return runner.AgentToolResult{Content: err.Error(), Success: false}, nil
	}
	c.mu.Lock()
	c.proposals = append(c.proposals, proposal)
	c.mu.Unlock()
	return runner.AgentToolResult{Content: `{"accepted":true}`, Success: true}, nil
}

func (c *proposalCollector) addError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, err)
}

func (c *proposalCollector) result() ([]Proposal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Proposal(nil), c.proposals...), errors.Join(c.errs...)
}

func parseProposals(output string) ([]Proposal, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	candidates := []string{output}
	if start := strings.Index(output, "{"); start >= 0 {
		if end := strings.LastIndex(output, "}"); end >= start {
			candidates = append(candidates, output[start:end+1])
		}
	}
	if start := strings.Index(output, "["); start >= 0 {
		if end := strings.LastIndex(output, "]"); end >= start {
			candidates = append(candidates, output[start:end+1])
		}
	}
	for _, candidate := range candidates {
		var envelope struct {
			Issues []Proposal `json:"issues"`
		}
		if err := json.Unmarshal([]byte(candidate), &envelope); err == nil && envelope.Issues != nil {
			return normalizeProposals(envelope.Issues)
		}
		var proposals []Proposal
		if err := json.Unmarshal([]byte(candidate), &proposals); err == nil {
			return normalizeProposals(proposals)
		}
	}
	return nil, fmt.Errorf("%w: expected an issues JSON object", ErrInvalidProposalOutput)
}

func normalizeProposals(proposals []Proposal) ([]Proposal, error) {
	out := make([]Proposal, 0, len(proposals))
	var resultErr error
	for _, proposal := range proposals {
		normalized, err := normalizeProposal(proposal)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			continue
		}
		out = append(out, normalized)
	}
	return out, resultErr
}

func normalizeProposal(proposal Proposal) (Proposal, error) {
	proposal.DedupKey = strings.TrimSpace(proposal.DedupKey)
	proposal.Title = strings.TrimSpace(proposal.Title)
	proposal.Body = strings.TrimSpace(proposal.Body)
	switch {
	case proposal.DedupKey == "":
		return Proposal{}, fmt.Errorf("%w: dedup_key is required", ErrInvalidProposal)
	case len(proposal.DedupKey) > maxProposalDedupKeySize:
		return Proposal{}, fmt.Errorf("%w: dedup_key exceeds %d bytes", ErrInvalidProposal, maxProposalDedupKeySize)
	case proposal.Title == "":
		return Proposal{}, fmt.Errorf("%w: title is required", ErrInvalidProposal)
	case len(proposal.Title) > maxProposalTitleSize:
		return Proposal{}, fmt.Errorf("%w: title exceeds %d bytes", ErrInvalidProposal, maxProposalTitleSize)
	case strings.ContainsAny(proposal.Title, "\r\n"):
		return Proposal{}, fmt.Errorf("%w: title must be a single line", ErrInvalidProposal)
	case proposal.Body == "":
		return Proposal{}, fmt.Errorf("%w: body is required", ErrInvalidProposal)
	case len(proposal.Body) > maxProposalBodySize:
		return Proposal{}, fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidProposal, maxProposalBodySize)
	default:
		return proposal, nil
	}
}

func proposalMarker(projectID string, routineName string, dedupKey string) string {
	raw := strings.ToLower(strings.TrimSpace(projectID)) + "\x00" + strings.ToLower(strings.TrimSpace(routineName)) + "\x00" + strings.ToLower(strings.TrimSpace(dedupKey))
	sum := sha256.Sum256([]byte(raw))
	return "<!-- detent:routine fingerprint=" + hex.EncodeToString(sum[:]) + " -->"
}

func routineIssue(projectID string, definition config.Routine) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "routine:" + strings.TrimSpace(projectID) + ":" + definition.Name
	issue.Identifier = strings.TrimSpace(projectID) + "/routine/" + definition.Name
	issue.Title = "Scheduled routine: " + definition.Name
	issue.Description = definition.Prompt
	issue.State = "Routine"
	return issue
}

func normalizeSettings(settings Settings) Settings {
	settings.ProjectID = strings.TrimSpace(settings.ProjectID)
	settings.Definitions = config.NormalizeRoutines(settings.Definitions)
	settings.SearchStates = normalizeStates(settings.SearchStates)
	if len(settings.Definitions) > 0 && !containsFold(settings.SearchStates, IssueState) {
		settings.SearchStates = append(settings.SearchStates, IssueState)
	}
	return settings
}

func cloneSettings(settings Settings) Settings {
	settings.Definitions = append([]config.Routine(nil), settings.Definitions...)
	settings.SearchStates = append([]string(nil), settings.SearchStates...)
	return settings
}

func routineByName(definitions []config.Routine, name string) (config.Routine, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, definition := range definitions {
		if definition.Name == name {
			return definition, true
		}
	}
	return config.Routine{}, false
}

func normalizeStates(states []string) []string {
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		key := strings.ToLower(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func (m *Manager) signalUpdate() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}

func (m *Manager) runAndLog(ctx context.Context, name string, scheduledFor time.Time) {
	result, err := m.runNamed(ctx, name, scheduledFor, true)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.ErrorContext(ctx, "scheduled routine failed", "routine", name, "filed", len(result.Filed), "deduplicated", result.Deduplicated, "error", err)
		}
		return
	}
	m.logger.InfoContext(ctx, "scheduled routine completed", "routine", name, "filed", len(result.Filed), "deduplicated", result.Deduplicated)
}

func stopTimer(timer *time.Timer) {
	if timer == nil || !timer.Stop() {
		return
	}
}
