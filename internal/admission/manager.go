package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

const (
	ProposalToolName = "propose_backlog_admission"
	admissionState   = "Admission"
	maxRationaleSize = 16 * 1024
)

var (
	ErrMissingStore      = errors.New("backlog admission store is required")
	ErrMissingIssueStore = errors.New("backlog admission issue store is required")
	ErrMissingRunner     = errors.New("backlog admission runner is required")
	ErrInvalidProposal   = errors.New("backlog admission proposal is invalid")
	ErrInvalidOutput     = errors.New("backlog admission agent output is invalid")
)

type Store interface {
	CreateAdmissionProposal(context.Context, admissionmodel.Proposal) (bool, error)
	OpenAdmissionProposals(context.Context, string, int) ([]admissionmodel.Proposal, error)
	AdmissionProposalHistory(context.Context, string, string) ([]admissionmodel.Proposal, error)
	CountOpenAdmissionProposals(context.Context, string) (int, error)
	ExpireAdmissionProposals(context.Context, string, time.Time) (int, error)
	TransitionAdmissionProposal(context.Context, string, admissionmodel.ProposalStatus, admissionmodel.ProposalStatus, time.Time) error
	ResolveAdmissionProposal(context.Context, admissionmodel.Decision) error
	MarkAdmissionProposalCommented(context.Context, string, time.Time) error
	RefreshAdmissionOutcomes(context.Context, admissionmodel.OutcomeRefresh) error
	RecordAdmissionRun(context.Context, admissionmodel.RunRecord) error
	LatestAdmissionRun(context.Context, string) (admissionmodel.RunRecord, bool, error)
}

type IssueStore interface {
	FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error)
	FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error)
	CreateComment(context.Context, string, string) error
}

type Settings struct {
	ProjectID          string
	Config             config.BacklogAdmission
	Criteria           config.AdmissionCriteria
	DispatchStates     []string
	DispatchLabels     []string
	PrioritizeBlockers bool
	Runner             runner.Backend
	Issues             IssueStore
	Scheduler          scheduler.Scheduler
	GlobalDispatchGate scheduler.ProjectDispatchGate
	ProjectCandidate   scheduler.ProjectCandidate
	TerminalStates     []string
	ReworkState        string
}

type Result struct {
	CandidatesFound int
	Candidates      int
	Proposals       []admissionmodel.Proposal
	Skipped         map[string]int
	Truncated       map[string]int
	DeferredReason  string
}

type AgentProposal struct {
	IssueID    string                   `json:"issue_id"`
	Findings   []admissionmodel.Finding `json:"findings"`
	Confidence *float64                 `json:"confidence"`
}

type Manager struct {
	mu        sync.RWMutex
	processMu sync.Mutex
	settings  Settings
	store     Store
	logger    *slog.Logger
	now       func() time.Time
	updates   chan struct{}
	baseline  time.Time
}

func New(settings Settings, store Store, logger *slog.Logger, now func() time.Time) (*Manager, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if now == nil {
		now = time.Now
	}
	manager := &Manager{
		store:   store,
		logger:  logger,
		now:     now,
		updates: make(chan struct{}, 1),
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
	if settings.Config.Enabled {
		if m.store == nil {
			return ErrMissingStore
		}
		if settings.Runner == nil {
			return ErrMissingRunner
		}
		if settings.Issues == nil {
			return ErrMissingIssueStore
		}
		if strings.TrimSpace(settings.Criteria.Text) == "" || len(settings.Criteria.Dimensions) == 0 {
			return errors.New("backlog admission criteria are unresolved")
		}
	}
	m.mu.Lock()
	m.settings = cloneSettings(settings)
	m.baseline = m.now()
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
	return m.settings.Config.Enabled
}

func (m *Manager) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		next, scheduled, err := m.nextScheduled(ctx)
		if err != nil {
			m.logger.ErrorContext(ctx, "backlog admission schedule lookup failed", "error", err)
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				stopTimer(timer)
				return nil
			case <-m.updates:
				stopTimer(timer)
			case <-timer.C:
			}
			continue
		}
		if !scheduled {
			select {
			case <-ctx.Done():
				return nil
			case <-m.updates:
				continue
			}
		}
		timer := time.NewTimer(max(next.Sub(m.now()), 0))
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil
		case <-m.updates:
			stopTimer(timer)
			continue
		case <-timer.C:
			m.runAndLog(ctx, next)
		}
	}
}

func (m *Manager) RunOnce(ctx context.Context) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return m.run(ctx, m.now(), false)
}

func (m *Manager) run(ctx context.Context, scheduledFor time.Time, scheduled bool) (Result, error) {
	m.processMu.Lock()
	defer m.processMu.Unlock()

	m.mu.RLock()
	settings := cloneSettings(m.settings)
	baseline := m.baseline
	m.mu.RUnlock()
	if !settings.Config.Enabled {
		return newResult(), nil
	}
	if scheduled {
		schedule, err := cron.ParseStandard(settings.Config.Schedule)
		if err != nil {
			return newResult(), err
		}
		if next := schedule.Next(baseline); !next.Equal(scheduledFor) {
			return newResult(), nil
		}
	}
	result, err := m.runOnce(ctx, settings, scheduledFor)
	completedAt := m.now()
	m.mu.Lock()
	if completedAt.After(m.baseline) {
		m.baseline = completedAt
	}
	m.mu.Unlock()
	if !scheduled {
		m.signalUpdate()
	}
	return result, err
}

func (m *Manager) runOnce(ctx context.Context, settings Settings, scheduledFor time.Time) (result Result, runErr error) {
	result = newResult()
	startedAt := m.now().UTC()
	record := admissionmodel.RunRecord{
		ProjectID:    settings.ProjectID,
		ScheduledFor: scheduledFor.UTC(),
		StartedAt:    startedAt,
		Outcome:      "completed",
	}
	defer func() {
		record.CompletedAt = m.now().UTC()
		record.CandidatesFound = result.CandidatesFound
		record.Candidates = result.Candidates
		record.Proposed = len(result.Proposals)
		record.Skipped = cloneCounts(result.Skipped)
		record.Truncated = cloneCounts(result.Truncated)
		record.DeferredReason = result.DeferredReason
		record.Issues = make([]admissionmodel.IssueRecord, 0, len(result.Proposals))
		for _, proposal := range result.Proposals {
			record.Issues = append(record.Issues, admissionmodel.IssueRecord{
				ID:         proposal.IssueID,
				Identifier: proposal.IssueIdentifier,
				URL:        proposal.IssueURL,
				ProposalID: proposal.ID,
			})
		}
		if result.DeferredReason != "" {
			record.Outcome = "deferred"
		}
		if runErr != nil {
			record.Outcome = "failed"
			record.Error = runErr.Error()
		}
		if err := m.store.RecordAdmissionRun(context.WithoutCancel(ctx), record); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("record backlog admission run: %w", err))
		}
	}()

	expired, err := m.store.ExpireAdmissionProposals(ctx, settings.ProjectID, startedAt)
	if err != nil {
		return result, err
	}
	if expired > 0 {
		result.Skipped["expired"] = expired
	}
	commentsRemaining := settings.Config.MaxProposalsPerRun
	commentsRemaining, err = m.reconcileOpenProposals(ctx, settings, commentsRemaining, startedAt)
	if err != nil {
		return result, err
	}
	if err := m.store.RefreshAdmissionOutcomes(ctx, admissionmodel.OutcomeRefresh{
		ProjectID:      settings.ProjectID,
		TerminalStates: settings.TerminalStates,
		ReworkState:    settings.ReworkState,
		ObservedAt:     startedAt,
	}); err != nil {
		return result, err
	}
	if deferred, reason, err := admissionBudgetDeferred(ctx, settings.Runner, startedAt); err != nil {
		return result, err
	} else if deferred {
		result.DeferredReason = reason
		return result, nil
	}
	release, acquired, reason, err := acquireCapacity(ctx, settings, startedAt)
	if err != nil {
		return result, err
	}
	if !acquired {
		result.DeferredReason = reason
		return result, nil
	}
	defer func() {
		runErr = errors.Join(runErr, release())
	}()

	candidates, err := settings.Issues.FetchIssuesByStates(ctx, settings.Config.Sources.States)
	if err != nil {
		return result, fmt.Errorf("fetch backlog admission candidates: %w", err)
	}
	result.CandidatesFound = len(candidates)
	candidates = filterCandidates(candidates, settings.Config, result.Skipped)
	sortCandidates(candidates, settings)
	candidates, err = m.unproposedCandidates(ctx, settings, candidates, result.Skipped)
	if err != nil {
		return result, err
	}
	if len(candidates) > settings.Config.MaxCandidatesPerRun {
		result.Truncated["candidates"] = len(candidates) - settings.Config.MaxCandidatesPerRun
		candidates = candidates[:settings.Config.MaxCandidatesPerRun]
	}
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}
	open, err := m.store.CountOpenAdmissionProposals(ctx, settings.ProjectID)
	if err != nil {
		return result, err
	}
	if open >= settings.Config.MaxOpenProposals {
		result.Skipped["open_proposal_cap"] += len(candidates)
		return result, nil
	}
	if commentsRemaining == 0 {
		result.Skipped["comment_cap"] += len(candidates)
		return result, nil
	}

	collector := &proposalCollector{}
	runResult, err := settings.Runner.Run(ctx, runner.RunRequest{
		Issue:            admissionIssue(settings.ProjectID),
		Mode:             runner.RunModeRoutine,
		StartedAt:        startedAt,
		Admission:        admissionRequest(settings, candidates),
		AgentTools:       []runner.AgentTool{proposalTool()},
		AgentToolHandler: collector.handle,
	})
	if err != nil {
		if runner.IsCapacityError(err) {
			result.DeferredReason = "agent_backend_capacity"
			return result, nil
		}
		return result, fmt.Errorf("run backlog admission agent: %w", err)
	}
	if runResult.BudgetRefusal != nil {
		result.DeferredReason = "budget"
		return result, nil
	}
	if runResult.FinalState != "" && runResult.FinalState != runner.FinalStateCompleted {
		return result, fmt.Errorf("run backlog admission agent: final state %s", runResult.FinalState)
	}
	proposals, proposalErr := collector.result()
	if len(proposals) == 0 && proposalErr == nil {
		proposals, proposalErr = parseProposals(runResult.Output)
	}
	if proposalErr != nil {
		return result, proposalErr
	}
	if len(proposals) > settings.Config.MaxProposalsPerRun {
		result.Truncated["proposals"] += len(proposals) - settings.Config.MaxProposalsPerRun
		proposals = proposals[:settings.Config.MaxProposalsPerRun]
	}
	if len(proposals) > commentsRemaining {
		result.Truncated["comments"] += len(proposals) - commentsRemaining
		proposals = proposals[:commentsRemaining]
	}
	available := settings.Config.MaxOpenProposals - open
	if len(proposals) > available {
		result.Truncated["open_proposals"] += len(proposals) - available
		proposals = proposals[:available]
	}
	return m.executeProposals(ctx, settings, candidates, proposals, result, commentsRemaining, startedAt)
}

func (m *Manager) reconcileOpenProposals(
	ctx context.Context,
	settings Settings,
	commentsRemaining int,
	at time.Time,
) (int, error) {
	proposals, err := m.store.OpenAdmissionProposals(ctx, settings.ProjectID, 0)
	if err != nil {
		return commentsRemaining, err
	}
	if len(proposals) == 0 {
		return commentsRemaining, nil
	}
	ids := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		ids = append(ids, proposal.IssueID)
	}
	issues, err := settings.Issues.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		return commentsRemaining, fmt.Errorf("reconcile backlog admission proposals: %w", err)
	}
	byID := issueMap(issues)
	commentReader, ok := settings.Issues.(connector.IssueCommentReader)
	if !ok {
		return commentsRemaining, errors.New("backlog admission issue comment reader is required")
	}
	commentsByIssue := map[string][]connector.IssueComment{}
	for _, proposal := range proposals {
		issue, ok := byID[proposal.IssueID]
		if !ok {
			continue
		}
		comments, loaded := commentsByIssue[proposal.IssueID]
		if !loaded {
			comments, err = commentReader.FetchIssueComments(ctx, issue)
			if err != nil {
				return commentsRemaining, fmt.Errorf("read backlog admission decision comments: %w", err)
			}
			commentsByIssue[proposal.IssueID] = comments
		}
		decision, decided := proposalDecision(proposal, comments)
		if decided && decision.Outcome == admissionmodel.ProposalRejected {
			if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
				return commentsRemaining, err
			}
			continue
		}
		if decided && decision.Outcome == admissionmodel.ProposalAccepted &&
			strings.EqualFold(strings.TrimSpace(issue.State), proposal.TargetState) {
			transition, found, err := admissionIssueTransition(ctx, settings.Issues, issue)
			if err != nil {
				return commentsRemaining, err
			}
			if found && !transition.EnteredAt.Before(decision.DecidedAt) &&
				admissionActorsCorrelate(decision.ActorLogin, transition.Actor.Login) {
				decision.TransitionAt = transition.EnteredAt
				if strings.TrimSpace(transition.Actor.Login) != "" {
					decision.ActorLogin = transition.Actor.Login
					decision.ActorKind = transition.Actor.Kind
				}
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, err
				}
				continue
			}
		}
		if proposal.CommentedAt.IsZero() && commentsRemaining > 0 {
			if err := m.commentProposal(ctx, settings, proposal); err != nil {
				return commentsRemaining, err
			}
			commentsRemaining--
		}
	}
	return commentsRemaining, nil
}

func (m *Manager) unproposedCandidates(
	ctx context.Context,
	settings Settings,
	candidates []connector.Issue,
	skipped map[string]int,
) ([]connector.Issue, error) {
	out := make([]connector.Issue, 0, len(candidates))
	for _, candidate := range candidates {
		history, err := m.store.AdmissionProposalHistory(ctx, settings.ProjectID, candidate.ID)
		if err != nil {
			return nil, err
		}
		fingerprint := issueFingerprint(candidate)
		suppress := false
		for _, proposal := range history {
			if proposal.Status == admissionmodel.ProposalAccepted {
				skipped["accepted_demotion"]++
				suppress = true
				break
			}
			if proposal.Status == admissionmodel.ProposalOpen && proposal.Fingerprint == fingerprint {
				skipped["unchanged_open_proposal"]++
				suppress = true
				break
			}
		}
		if !suppress {
			out = append(out, candidate)
		}
	}
	return out, nil
}

func (m *Manager) executeProposals(
	ctx context.Context,
	settings Settings,
	candidates []connector.Issue,
	agentProposals []AgentProposal,
	result Result,
	commentsRemaining int,
	at time.Time,
) (Result, error) {
	if len(agentProposals) == 0 {
		return result, nil
	}
	ids := make([]string, 0, len(candidates))
	candidateByID := issueMap(candidates)
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	fresh, err := settings.Issues.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		return result, fmt.Errorf("revalidate backlog admission candidates: %w", err)
	}
	freshByID := issueMap(fresh)
	seen := map[string]struct{}{}
	for _, agentProposal := range agentProposals {
		issueID := strings.TrimSpace(agentProposal.IssueID)
		if _, ok := seen[issueID]; ok {
			result.Skipped["duplicate_agent_proposal"]++
			continue
		}
		seen[issueID] = struct{}{}
		original, supplied := candidateByID[issueID]
		current, currentFound := freshByID[issueID]
		if !supplied || !currentFound || issueFingerprint(original) != issueFingerprint(current) ||
			current.Closed || !containsFold(settings.Config.Sources.States, current.State) ||
			excludedCandidate(current, settings.Config) {
			result.Skipped["stale_or_ineligible"]++
			continue
		}
		findings, err := validateFindings(agentProposal.Findings, settings.Criteria)
		if err != nil {
			result.Skipped["invalid_agent_proposal"]++
			continue
		}
		if agentProposal.Confidence == nil || math.IsNaN(*agentProposal.Confidence) ||
			math.IsInf(*agentProposal.Confidence, 0) ||
			*agentProposal.Confidence < 0 || *agentProposal.Confidence > 1 {
			result.Skipped["invalid_agent_proposal"]++
			continue
		}
		open, err := m.store.CountOpenAdmissionProposals(ctx, settings.ProjectID)
		if err != nil {
			return result, err
		}
		if open >= settings.Config.MaxOpenProposals {
			result.Truncated["open_proposals"] += len(agentProposals) - len(result.Proposals)
			break
		}
		proposal := admissionmodel.Proposal{
			ID:              proposalID(settings.ProjectID, current.ID, issueFingerprint(current), at),
			ProjectID:       settings.ProjectID,
			IssueID:         current.ID,
			IssueIdentifier: current.Identifier,
			IssueURL:        current.URL,
			TargetState:     settings.Config.TargetState,
			Fingerprint:     issueFingerprint(current),
			CriteriaSection: settings.Criteria.Section,
			CriteriaText:    settings.Criteria.Text,
			Findings:        findings,
			Confidence:      *agentProposal.Confidence,
			Status:          admissionmodel.ProposalOpen,
			CreatedAt:       at,
			ExpiresAt:       at.AddDate(0, 0, settings.Config.ProposalExpiryDays),
		}
		created, err := m.store.CreateAdmissionProposal(ctx, proposal)
		if err != nil {
			return result, err
		}
		if !created {
			result.Skipped["unchanged_open_proposal"]++
			continue
		}
		result.Proposals = append(result.Proposals, proposal)
		if commentsRemaining > 0 {
			if err := m.commentProposal(ctx, settings, proposal); err != nil {
				return result, err
			}
			commentsRemaining--
		}
	}
	return result, nil
}

func (m *Manager) commentProposal(ctx context.Context, settings Settings, proposal admissionmodel.Proposal) error {
	if err := settings.Issues.CreateComment(ctx, proposal.IssueID, proposalComment(proposal)); err != nil {
		return fmt.Errorf("create backlog admission audit comment: %w", err)
	}
	if err := m.store.MarkAdmissionProposalCommented(ctx, proposal.ID, m.now().UTC()); err != nil {
		return fmt.Errorf("mark backlog admission audit comment: %w", err)
	}
	return nil
}

func proposalDecision(proposal admissionmodel.Proposal, comments []connector.IssueComment) (admissionmodel.Decision, bool) {
	notBefore := proposal.CreatedAt
	if proposal.CommentedAt.After(notBefore) {
		notBefore = proposal.CommentedAt
	}
	accept := admissionAcceptCommand(proposal.ID)
	reject := admissionRejectCommand(proposal.ID)
	var decision admissionmodel.Decision
	for _, comment := range comments {
		if comment.CreatedAt == nil || comment.CreatedAt.IsZero() || comment.CreatedAt.Before(notBefore) {
			continue
		}
		var outcome admissionmodel.ProposalStatus
		switch strings.TrimSpace(comment.Body) {
		case accept:
			outcome = admissionmodel.ProposalAccepted
		case reject:
			outcome = admissionmodel.ProposalRejected
		default:
			continue
		}
		if !decision.DecidedAt.IsZero() && !comment.CreatedAt.After(decision.DecidedAt) {
			continue
		}
		decision = admissionmodel.Decision{
			ProposalID: proposal.ID,
			Outcome:    outcome,
			DecidedAt:  comment.CreatedAt.UTC(),
			CommentID:  comment.ID,
			ActorLogin: comment.AuthorLogin,
			ActorKind:  comment.AuthorKind,
		}
	}
	return decision, !decision.DecidedAt.IsZero()
}

func admissionIssueTransition(ctx context.Context, issues IssueStore, issue connector.Issue) (connector.IssueStateTransition, bool, error) {
	if reader, ok := issues.(connector.IssueStateTransitionReader); ok {
		transition, found, err := reader.IssueStateTransition(ctx, issue)
		if err != nil || found {
			return transition, found, err
		}
	}
	if issue.StageUpdatedAt == nil || issue.StageUpdatedAt.IsZero() {
		return connector.IssueStateTransition{}, false, nil
	}
	return connector.IssueStateTransition{EnteredAt: issue.StageUpdatedAt.UTC()}, true, nil
}

func admissionActorsCorrelate(decisionActor string, transitionActor string) bool {
	decisionActor = strings.TrimSpace(decisionActor)
	transitionActor = strings.TrimSpace(transitionActor)
	return decisionActor == "" || transitionActor == "" || strings.EqualFold(decisionActor, transitionActor)
}

func admissionAcceptCommand(proposalID string) string {
	return "/detent admission accept " + strings.TrimSpace(proposalID)
}

func admissionRejectCommand(proposalID string) string {
	return "/detent admission reject " + strings.TrimSpace(proposalID)
}

func (m *Manager) nextScheduled(ctx context.Context) (time.Time, bool, error) {
	m.mu.RLock()
	settings := cloneSettings(m.settings)
	baseline := m.baseline
	m.mu.RUnlock()
	if !settings.Config.Enabled {
		return time.Time{}, false, nil
	}
	latest, ok, err := m.store.LatestAdmissionRun(ctx, settings.ProjectID)
	if err != nil {
		return time.Time{}, false, err
	}
	if ok && latest.CompletedAt.After(baseline) {
		baseline = latest.CompletedAt
	}
	schedule, err := cron.ParseStandard(settings.Config.Schedule)
	if err != nil {
		return time.Time{}, false, err
	}
	return schedule.Next(baseline), true, nil
}

func acquireCapacity(ctx context.Context, settings Settings, now time.Time) (func() error, bool, string, error) {
	releaseLocal := func() error { return nil }
	if settings.Scheduler != nil {
		slot, err := settings.Scheduler.RequestSlot(ctx, scheduler.SlotRequest{State: admissionState})
		if errors.Is(err, scheduler.ErrNoSlots) {
			return func() error { return nil }, false, "fleet_capacity", nil
		}
		if err != nil {
			return func() error { return nil }, false, "", err
		}
		releaseLocal = func() error {
			return settings.Scheduler.ReleaseSlot(slot)
		}
	}
	if settings.GlobalDispatchGate == nil {
		return releaseLocal, true, "", nil
	}
	candidate := settings.ProjectCandidate
	candidate.ID = strings.TrimSpace(candidate.ID) + "/admission"
	if strings.TrimSpace(candidate.ID) == "/admission" {
		return func() error { return nil }, false, "", errors.Join(errors.New("backlog admission project candidate id is required"), releaseLocal())
	}
	slot, acquired, err := settings.GlobalDispatchGate.TryAcquire(
		ctx,
		candidate,
		scheduler.SlotRequest{State: admissionState},
		now,
	)
	if err != nil {
		return func() error { return nil }, false, "", errors.Join(err, releaseLocal())
	}
	if !acquired {
		settings.GlobalDispatchGate.MarkIdle(candidate.ID)
		return func() error { return nil }, false, "fleet_capacity", releaseLocal()
	}
	return func() error {
		return errors.Join(settings.GlobalDispatchGate.Release(slot), releaseLocal())
	}, true, "", nil
}

func admissionBudgetDeferred(ctx context.Context, backend runner.Backend, now time.Time) (bool, string, error) {
	provider, ok := backend.(runner.DailyBudgetStatusProvider)
	if !ok {
		return false, "", nil
	}
	status, known, err := provider.DailyBudgetStatus(ctx, now)
	if err != nil {
		return false, "", fmt.Errorf("read backlog admission budget status: %w", err)
	}
	if known && status.Active && status.MaxUSD > 0 && status.CurrentSpendUSD >= status.MaxUSD {
		return true, "budget", nil
	}
	return false, "", nil
}

func filterCandidates(issues []connector.Issue, cfg config.BacklogAdmission, skipped map[string]int) []connector.Issue {
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		switch {
		case issue.Closed:
			skipped["closed"]++
		case excludedByLabel(issue, cfg.ExcludeLabels):
			skipped["excluded_label"]++
		case !allowedAuthor(issue.AuthorID, cfg.Authors.Allow):
			skipped["author"]++
		default:
			out = append(out, issue)
		}
	}
	return out
}

func excludedCandidate(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return excludedByLabel(issue, cfg.ExcludeLabels) || !allowedAuthor(issue.AuthorID, cfg.Authors.Allow)
}

func excludedByLabel(issue connector.Issue, labels []string) bool {
	for _, issueLabel := range issue.Labels {
		if containsFold(labels, issueLabel) {
			return true
		}
	}
	return false
}

func allowedAuthor(author string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	return containsFold(allow, strings.TrimPrefix(strings.TrimSpace(author), "@"))
}

func sortCandidates(issues []connector.Issue, settings Settings) {
	ranker := dispatchpriority.New(settings.DispatchStates, settings.DispatchLabels)
	sort.SliceStable(issues, func(i, j int) bool {
		left := issues[i]
		right := issues[j]
		if leftRank, rightRank := ranker.State(left.State), ranker.State(right.State); leftRank != rightRank {
			return leftRank < rightRank
		}
		if leftRank, rightRank := dispatchpriority.Priority(left.Priority), dispatchpriority.Priority(right.Priority); leftRank != rightRank {
			return leftRank < rightRank
		}
		leftLabel, leftLabeled := ranker.MatchLabel(left.Labels)
		rightLabel, rightLabeled := ranker.MatchLabel(right.Labels)
		if leftLabeled != rightLabeled {
			return leftLabeled
		}
		if leftLabeled && leftLabel.Rank != rightLabel.Rank {
			return leftLabel.Rank < rightLabel.Rank
		}
		if settings.PrioritizeBlockers && !leftLabeled && left.UnblockerCount != right.UnblockerCount {
			return left.UnblockerCount > right.UnblockerCount
		}
		if left.CreatedAt != nil && right.CreatedAt != nil && !left.CreatedAt.Equal(*right.CreatedAt) {
			return left.CreatedAt.Before(*right.CreatedAt)
		}
		if left.CreatedAt != nil && right.CreatedAt == nil {
			return true
		}
		if left.CreatedAt == nil && right.CreatedAt != nil {
			return false
		}
		return left.Identifier < right.Identifier
	})
}

func validateFindings(findings []admissionmodel.Finding, criteria config.AdmissionCriteria) ([]admissionmodel.Finding, error) {
	if len(findings) == 0 {
		return nil, ErrInvalidProposal
	}
	dimensions := make(map[string]config.AdmissionDimension, len(criteria.Dimensions))
	for _, dimension := range criteria.Dimensions {
		dimensions[strings.ToLower(strings.TrimSpace(dimension.Name))] = dimension
	}
	out := make([]admissionmodel.Finding, 0, len(findings))
	matched := false
	seen := map[string]struct{}{}
	for _, finding := range findings {
		finding.Dimension = strings.TrimSpace(finding.Dimension)
		finding.CriterionQuote = strings.TrimSpace(finding.CriterionQuote)
		finding.Rationale = strings.TrimSpace(finding.Rationale)
		key := strings.ToLower(finding.Dimension)
		dimension, ok := dimensions[key]
		if !ok || finding.CriterionQuote == "" || !strings.Contains(dimension.Text, finding.CriterionQuote) ||
			finding.Rationale == "" || len(finding.Rationale) > maxRationaleSize {
			return nil, ErrInvalidProposal
		}
		if _, ok := seen[key]; ok {
			return nil, ErrInvalidProposal
		}
		seen[key] = struct{}{}
		matched = matched || finding.Matched
		out = append(out, finding)
	}
	if !matched {
		return nil, ErrInvalidProposal
	}
	return out, nil
}

func issueFingerprint(issue connector.Issue) string {
	sum := sha256.Sum256([]byte(
		strconv.Itoa(len(issue.Title)) + ":" + issue.Title +
			strconv.Itoa(len(issue.Description)) + ":" + issue.Description,
	))
	return hex.EncodeToString(sum[:])
}

func proposalID(projectID string, issueID string, fingerprint string, at time.Time) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(projectID) + "\x00" + strings.TrimSpace(issueID) + "\x00" + fingerprint + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "admission-" + hex.EncodeToString(sum[:12])
}

func proposalComment(proposal admissionmodel.Proposal) string {
	var b strings.Builder
	b.WriteString("## Detent backlog admission proposal\n\n")
	b.WriteString("Proposal `")
	b.WriteString(proposal.ID)
	b.WriteString("` recommends moving this issue to **")
	b.WriteString(proposal.TargetState)
	b.WriteString("**. Detent has not changed the issue status. To accept, reply with `")
	b.WriteString(admissionAcceptCommand(proposal.ID))
	b.WriteString("` and then move the issue to that state. To reject, reply with `")
	b.WriteString(admissionRejectCommand(proposal.ID))
	b.WriteString("`. Leaving it unactioned will expire the proposal without counting as rejection.\n\n")
	b.WriteString("Criteria section: **")
	b.WriteString(proposal.CriteriaSection)
	b.WriteString("**\n\n")
	for _, finding := range proposal.Findings {
		b.WriteString("- **")
		b.WriteString(finding.Dimension)
		b.WriteString("** — ")
		b.WriteString(finding.Rationale)
		b.WriteString("\n  - Criterion: “")
		b.WriteString(finding.CriterionQuote)
		b.WriteString("”\n")
	}
	b.WriteString("\nConfidence: ")
	b.WriteString(strconv.FormatFloat(proposal.Confidence, 'f', 2, 64))
	b.WriteString("\n\nExpires: ")
	b.WriteString(proposal.ExpiresAt.UTC().Format(time.RFC3339))
	b.WriteString("\n\n<!-- detent-backlog-admission:")
	b.WriteString(proposal.ID)
	b.WriteString(" -->")
	return b.String()
}

func admissionRequest(settings Settings, candidates []connector.Issue) *runner.AdmissionRequest {
	dimensions := make([]runner.AdmissionDimension, 0, len(settings.Criteria.Dimensions))
	for _, dimension := range settings.Criteria.Dimensions {
		dimensions = append(dimensions, runner.AdmissionDimension{Name: dimension.Name, Text: dimension.Text})
	}
	agentCandidates := make([]runner.AdmissionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		agentCandidates = append(agentCandidates, runner.AdmissionCandidate{
			ID:          candidate.ID,
			Identifier:  candidate.Identifier,
			Title:       candidate.Title,
			Description: candidate.Description,
			State:       candidate.State,
			AuthorID:    candidate.AuthorID,
			Labels:      append([]string(nil), candidate.Labels...),
		})
	}
	return &runner.AdmissionRequest{
		Schedule:        settings.Config.Schedule,
		TargetState:     settings.Config.TargetState,
		CriteriaSection: settings.Criteria.Section,
		CriteriaText:    settings.Criteria.Text,
		Dimensions:      dimensions,
		Candidates:      agentCandidates,
	}
}

func admissionIssue(projectID string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = strings.TrimSpace(projectID) + "/admission"
	issue.Identifier = issue.ID
	issue.Title = "Scheduled backlog admission"
	issue.State = admissionState
	return issue
}

func proposalTool() runner.AgentTool {
	return runner.AgentTool{
		Name:        ProposalToolName,
		Description: "Propose admission for one supplied candidate using only project-owned criteria.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["issue_id","findings","confidence"],"properties":{"issue_id":{"type":"string","minLength":1},"findings":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"required":["dimension","criterion_quote","matched","rationale"],"properties":{"dimension":{"type":"string","minLength":1},"criterion_quote":{"type":"string","minLength":1},"matched":{"type":"boolean"},"rationale":{"type":"string","minLength":1}}}},"confidence":{"type":"number","minimum":0,"maximum":1}}}`),
	}
}

type proposalCollector struct {
	mu        sync.Mutex
	proposals []AgentProposal
	err       error
}

func (c *proposalCollector) handle(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	if call.Name != ProposalToolName {
		return runner.AgentToolResult{Content: "unsupported tool", Success: false}, nil
	}
	var proposal AgentProposal
	if err := decodeStrictJSON(call.Arguments, &proposal); err != nil {
		c.mu.Lock()
		c.err = errors.Join(c.err, fmt.Errorf("%w: %w", ErrInvalidOutput, err))
		c.mu.Unlock()
		return runner.AgentToolResult{Content: "invalid proposal", Success: false}, nil
	}
	c.mu.Lock()
	c.proposals = append(c.proposals, proposal)
	c.mu.Unlock()
	return runner.AgentToolResult{Content: "proposal received", Success: true}, nil
}

func (c *proposalCollector) result() ([]AgentProposal, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AgentProposal(nil), c.proposals...), c.err
}

func parseProposals(output string) ([]AgentProposal, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	var envelope struct {
		Proposals *[]AgentProposal `json:"proposals"`
	}
	if err := decodeStrictJSON([]byte(output), &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	if envelope.Proposals == nil {
		return nil, fmt.Errorf("%w: proposals is required", ErrInvalidOutput)
	}
	return *envelope.Proposals, nil
}

func decodeStrictJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func issueMap(issues []connector.Issue) map[string]connector.Issue {
	out := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		id := strings.TrimSpace(issue.ID)
		if id != "" {
			out[id] = issue
		}
	}
	return out
}

func normalizeSettings(settings Settings) Settings {
	settings.ProjectID = strings.TrimSpace(settings.ProjectID)
	settings.Config.Normalize()
	settings.DispatchStates = normalizeStrings(settings.DispatchStates)
	settings.DispatchLabels = normalizeStrings(settings.DispatchLabels)
	settings.TerminalStates = normalizeStrings(settings.TerminalStates)
	settings.ReworkState = strings.TrimSpace(settings.ReworkState)
	if settings.ReworkState == "" {
		settings.ReworkState = "Rework"
	}
	settings.ProjectCandidate.ID = strings.TrimSpace(settings.ProjectCandidate.ID)
	if settings.ProjectCandidate.ID == "" {
		settings.ProjectCandidate.ID = settings.ProjectID
	}
	return settings
}

func cloneSettings(settings Settings) Settings {
	settings.Config.Sources.States = append([]string(nil), settings.Config.Sources.States...)
	settings.Config.ExcludeLabels = append([]string(nil), settings.Config.ExcludeLabels...)
	settings.Config.Authors.Allow = append([]string(nil), settings.Config.Authors.Allow...)
	settings.Criteria.Dimensions = append([]config.AdmissionDimension(nil), settings.Criteria.Dimensions...)
	settings.DispatchStates = append([]string(nil), settings.DispatchStates...)
	settings.DispatchLabels = append([]string(nil), settings.DispatchLabels...)
	settings.TerminalStates = append([]string(nil), settings.TerminalStates...)
	return settings
}

func normalizeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsFold(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func cloneCounts(values map[string]int) map[string]int {
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func newResult() Result {
	return Result{Skipped: map[string]int{}, Truncated: map[string]int{}}
}

func (m *Manager) signalUpdate() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}

func (m *Manager) runAndLog(ctx context.Context, scheduledFor time.Time) {
	result, err := m.run(ctx, scheduledFor, true)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.ErrorContext(ctx, "scheduled backlog admission failed", "error", err)
		}
		return
	}
	m.logger.InfoContext(ctx, "scheduled backlog admission completed",
		"candidates", result.Candidates,
		"proposals", len(result.Proposals),
		"deferred_reason", result.DeferredReason,
	)
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}
