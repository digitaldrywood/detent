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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/agentoverride"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/dispatchpriority"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/schedulehealth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	ProposalToolName                       = "propose_backlog_admission"
	admissionState                         = "Admission"
	admissionResolutionExplicitAccept      = "explicit_accept"
	admissionResolutionExplicitReject      = "explicit_reject"
	admissionResolutionImplicitAccept      = "implicit_accept"
	admissionResolutionIssueClosed         = "issue_closed"
	admissionResolutionTerminalState       = "terminal_state_reached"
	admissionResolutionAutoAdmit           = "auto_admit"
	admissionResolutionSourceStateChanged  = "source_state_changed_before_acceptance"
	admissionResolutionAutoAdmitIneligible = "candidate_became_ineligible_before_auto_admit"
	admissionResolutionEffortUnavailable   = "effort_recommendation_unavailable"
	admissionResolutionNonDeliverable      = "non_deliverable_declined"
	admissionResolutionCriteriaNotMet      = "criteria_not_met_declined"
	admissionOptOutMarker                  = "<!-- detent:no-dispatch -->"
	admissionDeclineExplicitOptOut         = "explicit_opt_out"
	admissionDeclineTracker                = "tracker"
	admissionDeclineIntake                 = "intake"
	admissionDeclineStudy                  = "study"
	admissionDeclineResearch               = "research"
	admissionDeclineLinkedChecklist        = "linked_issue_checklist"
	admissionDeclineCriteriaNotMet         = "criteria_not_met"
	admissionDispositionProposed           = "proposed"
	admissionDispositionDeclined           = "declined"
	maxRationaleSize                       = 16 * 1024
	maxEffortRationaleSize                 = 2 * 1024
)

var (
	ErrMissingStore       = errors.New("backlog admission store is required")
	ErrMissingIssueStore  = errors.New("backlog admission issue store is required")
	ErrMissingRunner      = errors.New("backlog admission runner is required")
	ErrInvalidProposal    = errors.New("backlog admission proposal is invalid")
	ErrInvalidOutput      = errors.New("backlog admission agent output is invalid")
	artifactTitlePattern  = regexp.MustCompile(`(?i)(?:\(\s*(tracker|intake|study|research)\s*\)|\[\s*(tracker|intake|study|research)\s*\]|(?:^|[—–:]|\s-\s)\s*(master\s+tracker|tracker|intake|study|research)(?:\s+(?:issue|artifact))?)\s*$`)
	issueReferencePattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+/[a-z0-9_.-]+)?#\d+|https://github\.com/[a-z0-9_.-]+/[a-z0-9_.-]+/issues/\d+`)
)

type Store interface {
	CreateAdmissionProposal(context.Context, admissionmodel.Proposal) (bool, error)
	CreateAdmissionDecline(context.Context, admissionmodel.Decline) (bool, error)
	AdmissionDecline(context.Context, string, string, string) (admissionmodel.Decline, bool, error)
	MarkAdmissionDeclineCommented(context.Context, string, time.Time) error
	OpenAdmissionProposals(context.Context, string, int) ([]admissionmodel.Proposal, error)
	AdmissionProposalHistory(context.Context, string, string) ([]admissionmodel.Proposal, error)
	CountOpenAdmissionProposals(context.Context, string) (int, error)
	ExpireAdmissionProposals(context.Context, string, time.Time) (int, error)
	TransitionAdmissionProposal(context.Context, string, admissionmodel.ProposalStatus, admissionmodel.ProposalStatus, time.Time) error
	ResolveAdmissionProposal(context.Context, admissionmodel.Decision) error
	AdmissionTargetTransitions(context.Context, admissionmodel.TargetTransitionQuery) ([]admissionmodel.TargetTransition, error)
	MarkAdmissionProposalCommented(context.Context, string, time.Time) error
	RefreshAdmissionOutcomes(context.Context, admissionmodel.OutcomeRefresh) error
	RecordAdmissionRun(context.Context, admissionmodel.RunRecord) error
	LatestAdmissionRun(context.Context, string) (admissionmodel.RunRecord, bool, error)
}

type IssueStore interface {
	connector.CandidateReader
	FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error)
	CreateComment(context.Context, string, string) error
	UpdateIssueState(context.Context, string, string) error
}

type Settings struct {
	ProjectID          string
	Config             config.BacklogAdmission
	Criteria           config.AdmissionCriteria
	EffortRubric       config.AdmissionEffortRubric
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
	ScheduleRuns       schedulehealth.Recorder
}

type Result struct {
	ProjectID       string
	CandidatesFound int
	Candidates      int
	ItemsRead       int
	Proposals       []admissionmodel.Proposal
	Skipped         map[string]int
	Truncated       map[string]int
	DeferredReason  string
	ProposalReason  string
}

type AgentEvaluation struct {
	IssueID           string                   `json:"issue_id"`
	Disposition       string                   `json:"disposition"`
	Findings          []admissionmodel.Finding `json:"findings"`
	Confidence        *float64                 `json:"confidence"`
	RecommendedEffort string                   `json:"recommended_effort,omitempty"`
	EffortRationale   string                   `json:"effort_rationale,omitempty"`
}

type AgentProposal = AgentEvaluation

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
		if settings.Config.RequireEffort {
			if strings.TrimSpace(settings.EffortRubric.Text) == "" || len(settings.EffortRubric.Efforts) == 0 {
				return errors.New("backlog admission effort rubric is unresolved")
			}
			if _, ok := settings.Issues.(connector.IssueBodyUpdater); !ok {
				return errors.New("backlog admission issue body updater is required")
			}
		}
	}
	m.mu.Lock()
	m.settings = cloneSettings(settings)
	m.baseline = m.now()
	m.mu.Unlock()
	m.signalUpdate()
	return nil
}

func (m *Manager) UpdateProjectCandidate(candidate scheduler.ProjectCandidate) {
	if m == nil {
		return
	}
	m.mu.Lock()
	candidate.ID = strings.TrimSpace(candidate.ID)
	if candidate.ID == "" {
		candidate.ID = m.settings.ProjectID
	}
	candidate.ActiveHours = candidate.ActiveHours.Normalize()
	m.settings.ProjectCandidate = candidate
	m.mu.Unlock()
	m.signalUpdate()
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
	result, err := m.runOnce(ctx, settings, scheduledFor, scheduled)
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

func (m *Manager) runOnce(ctx context.Context, settings Settings, scheduledFor time.Time, scheduled bool) (result Result, runErr error) {
	result = newResult()
	result.ProjectID = strings.TrimSpace(settings.ProjectID)
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
		record.ProposalReason = result.ProposalReason
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
		if scheduled && settings.ScheduleRuns != nil {
			if err := settings.ScheduleRuns.RecordScheduledRun(context.WithoutCancel(ctx), schedulehealth.Run{
				ScheduleID:   schedulehealth.AdmissionID,
				ScheduledFor: record.ScheduledFor,
				StartedAt:    record.StartedAt,
				CompletedAt:  record.CompletedAt,
				Error:        record.Error,
			}); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("record backlog admission liveness: %w", err))
			}
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
	autoAdmitsRemaining := settings.Config.MaxProposalsPerRun
	commentsRemaining, autoAdmitsRemaining, err = m.reconcileOpenProposals(
		ctx,
		settings,
		commentsRemaining,
		autoAdmitsRemaining,
		startedAt,
	)
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

	candidates, readerTruncations, itemsRead, readerFiltered, err := readAdmissionCandidates(ctx, settings.Issues, settings.Config)
	if err != nil {
		return result, fmt.Errorf("fetch backlog admission candidates: %w", err)
	}
	result.ItemsRead = itemsRead
	if readerTruncations > 0 {
		result.Truncated["candidate_reader"] += readerTruncations
	}
	for reason, count := range readerFiltered {
		result.Skipped[reason] += count
	}
	result.CandidatesFound = len(candidates)
	candidates = filterCandidates(
		candidates,
		settings.Config,
		settings.TerminalStates,
		result.Skipped,
		readerFiltered["author"] > 0,
	)
	sortCandidates(candidates, settings)
	var truncatedCandidates int
	candidates, commentsRemaining, truncatedCandidates, err = m.unproposedCandidates(
		ctx,
		settings,
		candidates,
		result.Skipped,
		commentsRemaining,
		startedAt,
		settings.Config.MaxCandidatesPerRun,
	)
	if err != nil {
		return result, err
	}
	if truncatedCandidates > 0 {
		result.Truncated["candidates"] = truncatedCandidates
	}
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
	available := settings.Config.MaxOpenProposals - open
	evaluationLimit := min(settings.Config.MaxProposalsPerRun, commentsRemaining, available)
	if len(candidates) > evaluationLimit {
		result.Truncated["candidates"] += len(candidates) - evaluationLimit
		candidates = candidates[:evaluationLimit]
	}
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}

	collector := &proposalCollector{}
	runResult, err := settings.Runner.Run(ctx, runner.RunRequest{
		Issue:            admissionIssue(settings.ProjectID),
		Mode:             runner.RunModeRoutine,
		StartedAt:        startedAt,
		Admission:        admissionRequest(settings, candidates),
		AgentTools:       []runner.AgentTool{proposalTool(settings.Config.RequireEffort)},
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
	evaluations, proposalErr := collector.result()
	if len(evaluations) == 0 && proposalErr == nil {
		evaluations, proposalErr = parseEvaluations(runResult.Output)
	}
	if proposalErr != nil {
		return result, proposalErr
	}
	return m.executeEvaluations(ctx, settings, candidates, evaluations, result, commentsRemaining, autoAdmitsRemaining, startedAt)
}

func candidateReadLimit(maxCandidates int) int {
	return max(maxCandidates, connector.DefaultCandidatePageSize)
}

func readAdmissionCandidates(
	ctx context.Context,
	issues IssueStore,
	cfg config.BacklogAdmission,
) ([]connector.Issue, int, int, map[string]int, error) {
	requests := make([]connector.CandidateRequest, 0, 3)
	if len(cfg.Sources.States) > 0 {
		requests = append(requests, connector.CandidateRequest{
			Selector: connector.CandidateSelectorStates,
			States:   cfg.Sources.States,
		})
	}
	if len(cfg.Sources.Labels) > 0 {
		requests = append(requests, connector.CandidateRequest{
			Selector: connector.CandidateSelectorLabels,
			Labels:   cfg.Sources.Labels,
		})
	}
	if cfg.Sources.Untracked {
		requests = append(requests, connector.CandidateRequest{
			Selector: connector.CandidateSelectorUntracked,
		})
	}

	limit := candidateReadLimit(cfg.MaxCandidatesPerRun)
	candidates := []connector.Issue{}
	seen := map[string]struct{}{}
	truncations := 0
	itemsRead := 0
	filtered := map[string]int{}
	for _, request := range requests {
		request.Limit = limit
		request.PageSize = connector.DefaultCandidatePageSize
		if request.Selector == connector.CandidateSelectorStates &&
			len(cfg.Authors.AllowAssociation) == 0 &&
			issues.CandidateCapabilities().SupportsPushdown(connector.CandidateFilterAuthorHandle) {
			request.Authors = append([]string(nil), cfg.Authors.Allow...)
		}
		result, err := issues.ReadCandidates(ctx, request)
		if err != nil {
			return nil, 0, 0, nil, fmt.Errorf("read %s selector: %w", request.Selector, err)
		}
		itemsRead += result.ItemsRead
		if result.Truncated {
			truncations++
		}
		for reason, count := range result.Filtered {
			filtered[reason] += count
		}
		for _, issue := range result.Issues {
			key := admissionCandidateKey(issue)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, issue)
		}
	}
	return candidates, truncations, itemsRead, filtered, nil
}

func (m *Manager) reconcileOpenProposals(
	ctx context.Context,
	settings Settings,
	commentsRemaining int,
	autoAdmitsRemaining int,
	at time.Time,
) (int, int, error) {
	proposals, err := m.store.OpenAdmissionProposals(ctx, settings.ProjectID, 0)
	if err != nil {
		return commentsRemaining, autoAdmitsRemaining, err
	}
	if len(proposals) == 0 {
		return commentsRemaining, autoAdmitsRemaining, nil
	}
	ids := make([]string, 0, len(proposals))
	for _, proposal := range proposals {
		ids = append(ids, proposal.IssueID)
	}
	issues, err := settings.Issues.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		return commentsRemaining, autoAdmitsRemaining, fmt.Errorf("reconcile backlog admission proposals: %w", err)
	}
	byID := issueMap(issues)
	commentReader, ok := settings.Issues.(connector.IssueCommentReader)
	if !ok {
		return commentsRemaining, autoAdmitsRemaining, errors.New("backlog admission issue comment reader is required")
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
				return commentsRemaining, autoAdmitsRemaining, fmt.Errorf("read backlog admission decision comments: %w", err)
			}
			commentsByIssue[proposal.IssueID] = comments
		}
		if classification, declined := classifyNonDeliverable(issue); declined {
			decline, created, err := m.createAdmissionDecline(ctx, settings, issue, classification, at)
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			commented, err := m.ensureAdmissionDeclineComment(
				ctx,
				settings.Issues,
				issue,
				decline,
				created,
				commentsRemaining > 0,
			)
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			if commented {
				commentsRemaining--
			}
			continue
		}
		decision, decided, err := proposalDecision(ctx, settings.Issues, issue, proposal, comments)
		if err != nil {
			return commentsRemaining, autoAdmitsRemaining, err
		}
		if decided && decision.Outcome == admissionmodel.ProposalRejected {
			decision.Reason = admissionResolutionExplicitReject
			if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			continue
		}
		if !decided && autoAdmitProposal(settings.Config, settings.Criteria, proposal, issue.Labels) {
			recovered, err := m.recoverAutomaticAdmission(ctx, settings, issue, proposal)
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			if recovered {
				continue
			}
		}
		if !decided {
			resolved, err := m.resolveProposalFromIssueState(ctx, settings, issue, proposal, at)
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			if resolved {
				continue
			}
		}
		if decided && decision.Outcome == admissionmodel.ProposalAccepted {
			decision.Reason = admissionResolutionExplicitAccept
			transitions, err := m.store.AdmissionTargetTransitions(ctx, admissionmodel.TargetTransitionQuery{
				ProjectID:   proposal.ProjectID,
				IssueID:     proposal.IssueID,
				TargetState: proposal.TargetState,
				NotBefore:   decision.DecidedAt,
			})
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			resolved := false
			for _, transition := range transitions {
				decision.TransitionAt = transition.EnteredAt
				decision.TransitionEventID = transition.EventID
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				resolved = true
				break
			}
			if resolved {
				continue
			}
		}
		if decided && decision.Outcome == admissionmodel.ProposalAccepted &&
			strings.EqualFold(strings.TrimSpace(issue.State), proposal.TargetState) {
			transition, found, err := admissionIssueTransition(ctx, settings.Issues, issue)
			if err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			if found && !transition.EnteredAt.Before(decision.DecidedAt) {
				decision.TransitionAt = transition.EnteredAt
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				continue
			}
		}
		if decided && decision.Outcome == admissionmodel.ProposalAccepted {
			if reason := inactiveProposalResolution(issue, settings.TerminalStates); reason != "" {
				decision.Outcome = admissionmodel.ProposalSuperseded
				decision.Reason = reason
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				continue
			}
			if !admissionSourceEligible(issue, settings.Config) {
				decision.Outcome = admissionmodel.ProposalSuperseded
				decision.Reason = admissionResolutionSourceStateChanged
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				continue
			}
			if err := m.admitProposal(ctx, settings, issue, proposal, decision); err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			continue
		}
		if !decided && autoAdmitProposal(settings.Config, settings.Criteria, proposal, issue.Labels) {
			if !autoAdmitCandidateEligible(issue, settings.Config) {
				decision := automaticAdmissionDecision(settings.Issues, proposal, at)
				decision.Outcome = admissionmodel.ProposalSuperseded
				decision.Reason = admissionResolutionAutoAdmitIneligible
				if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				continue
			}
			if proposal.CommentedAt.IsZero() {
				if commentsRemaining == 0 {
					continue
				}
				if err := m.commentProposal(ctx, settings, proposal, issue); err != nil {
					return commentsRemaining, autoAdmitsRemaining, err
				}
				commentsRemaining--
			}
			if autoAdmitsRemaining == 0 {
				continue
			}
			if err := m.admitProposal(
				ctx,
				settings,
				issue,
				proposal,
				automaticAdmissionDecision(settings.Issues, proposal, at),
			); err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			autoAdmitsRemaining--
			continue
		}
		if proposal.CommentedAt.IsZero() && commentsRemaining > 0 {
			if err := m.commentProposal(ctx, settings, proposal, issue); err != nil {
				return commentsRemaining, autoAdmitsRemaining, err
			}
			commentsRemaining--
		}
	}
	return commentsRemaining, autoAdmitsRemaining, nil
}

func (m *Manager) resolveProposalFromIssueState(
	ctx context.Context,
	settings Settings,
	issue connector.Issue,
	proposal admissionmodel.Proposal,
	at time.Time,
) (bool, error) {
	transitions, err := m.store.AdmissionTargetTransitions(ctx, admissionmodel.TargetTransitionQuery{
		ProjectID:   proposal.ProjectID,
		IssueID:     proposal.IssueID,
		TargetState: proposal.TargetState,
		NotBefore:   proposal.CreatedAt,
	})
	if err != nil {
		return false, err
	}
	if len(transitions) > 0 {
		transition := transitions[0]
		decision := admissionmodel.Decision{
			ProposalID:        proposal.ID,
			Outcome:           admissionmodel.ProposalAccepted,
			DecidedAt:         transition.EnteredAt.UTC(),
			ActorLogin:        transition.ActorLogin,
			ActorKind:         transition.ActorKind,
			TransitionAt:      transition.EnteredAt.UTC(),
			TransitionEventID: transition.EventID,
			Reason:            admissionResolutionImplicitAccept,
			Implicit:          true,
		}
		if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
			return false, err
		}
		return true, nil
	}

	var reason string
	outcome := admissionmodel.ProposalSuperseded
	if strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(proposal.TargetState)) {
		reason = admissionResolutionImplicitAccept
		outcome = admissionmodel.ProposalAccepted
	} else {
		reason = inactiveProposalResolution(issue, settings.TerminalStates)
	}
	if reason == "" {
		return false, nil
	}

	decision := admissionmodel.Decision{
		ProposalID: proposal.ID,
		Outcome:    outcome,
		DecidedAt:  at.UTC(),
		Reason:     reason,
		Implicit:   true,
	}
	if outcome == admissionmodel.ProposalAccepted {
		transition, found, err := admissionIssueTransition(ctx, settings.Issues, issue)
		if err != nil {
			return false, err
		}
		if found && !transition.EnteredAt.IsZero() && !transition.EnteredAt.Before(proposal.CreatedAt) {
			decision.DecidedAt = transition.EnteredAt.UTC()
			decision.ActorLogin = transition.Actor.Login
			decision.ActorKind = transition.Actor.Kind
		}
		decision.TransitionAt = decision.DecidedAt
	}
	if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
		return false, err
	}
	return true, nil
}

func inactiveProposalResolution(issue connector.Issue, terminalStates []string) string {
	if issue.Closed {
		return admissionResolutionIssueClosed
	}
	if containsFold(terminalStates, issue.State) {
		return admissionResolutionTerminalState
	}
	return ""
}

func (m *Manager) unproposedCandidates(
	ctx context.Context,
	settings Settings,
	candidates []connector.Issue,
	skipped map[string]int,
	commentsRemaining int,
	at time.Time,
	candidateLimit int,
) ([]connector.Issue, int, int, error) {
	out := make([]connector.Issue, 0, min(len(candidates), candidateLimit))
	processed := 0
	truncated := 0
	for _, candidate := range candidates {
		history, err := m.store.AdmissionProposalHistory(ctx, settings.ProjectID, candidate.ID)
		if err != nil {
			return nil, commentsRemaining, truncated, err
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
		if suppress {
			continue
		}
		decline, found, err := m.store.AdmissionDecline(ctx, settings.ProjectID, candidate.ID, fingerprint)
		if err != nil {
			return nil, commentsRemaining, truncated, err
		}
		if found && decline.Reason == admissionDeclineCriteriaNotMet {
			found = false
		}
		criteriaDecline, criteriaDeclined, err := m.store.AdmissionDecline(
			ctx,
			settings.ProjectID,
			candidate.ID,
			criteriaDeclineFingerprint(candidate, settings.Criteria),
		)
		if err != nil {
			return nil, commentsRemaining, truncated, err
		}
		if criteriaDeclined && criteriaDecline.Reason == admissionDeclineCriteriaNotMet {
			skipped[admissionDeclineCriteriaNotMet]++
			continue
		}
		var classification admissionDeclineClassification
		classified := false
		if !found {
			classification, classified = classifyNonDeliverable(candidate)
		}
		if !found && !classified {
			if processed >= candidateLimit {
				truncated++
				continue
			}
			processed++
			out = append(out, candidate)
			continue
		}
		skipped["non_deliverable"]++
		if found && !decline.CommentedAt.IsZero() {
			continue
		}
		if processed >= candidateLimit {
			truncated++
			continue
		}
		processed++
		created := false
		if !found {
			decline, created, err = m.createAdmissionDecline(ctx, settings, candidate, classification, at)
			if err != nil {
				return nil, commentsRemaining, truncated, err
			}
		}
		commented, err := m.ensureAdmissionDeclineComment(ctx, settings.Issues, candidate, decline, created, commentsRemaining > 0)
		if err != nil {
			return nil, commentsRemaining, truncated, err
		}
		if commented {
			commentsRemaining--
		}
	}
	return out, commentsRemaining, truncated, nil
}

type admissionDeclineClassification struct {
	reason          string
	detail          string
	confidence      *float64
	failedDimension string
	failedCriterion string
}

func (m *Manager) createAdmissionDecline(
	ctx context.Context,
	settings Settings,
	issue connector.Issue,
	classification admissionDeclineClassification,
	at time.Time,
) (admissionmodel.Decline, bool, error) {
	fingerprint := admissionDeclineFingerprint(issue, classification, settings.Criteria)
	decline := admissionmodel.Decline{
		ID:              admissionDeclineID(settings.ProjectID, issue.ID, fingerprint),
		ProjectID:       settings.ProjectID,
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		IssueURL:        issue.URL,
		Fingerprint:     fingerprint,
		Reason:          classification.reason,
		Detail:          classification.detail,
		Confidence:      classification.confidence,
		FailedDimension: classification.failedDimension,
		FailedCriterion: classification.failedCriterion,
		CreatedAt:       at,
	}
	created, err := m.store.CreateAdmissionDecline(ctx, decline)
	if err != nil {
		return admissionmodel.Decline{}, false, err
	}
	if created {
		return decline, true, nil
	}
	existing, found, err := m.store.AdmissionDecline(ctx, settings.ProjectID, issue.ID, fingerprint)
	if err != nil {
		return admissionmodel.Decline{}, false, err
	}
	if !found {
		return admissionmodel.Decline{}, false, errors.New("created backlog admission decline is unavailable")
	}
	return existing, false, nil
}

func (m *Manager) ensureAdmissionDeclineComment(
	ctx context.Context,
	issues IssueStore,
	issue connector.Issue,
	decline admissionmodel.Decline,
	created bool,
	allowed bool,
) (bool, error) {
	if !decline.CommentedAt.IsZero() || !allowed {
		return false, nil
	}
	if !created {
		reader, ok := issues.(connector.IssueCommentReader)
		if !ok {
			return false, errors.New("backlog admission issue comment reader is required")
		}
		comments, err := reader.FetchIssueComments(ctx, issue)
		if err != nil {
			return false, fmt.Errorf("read backlog admission decline comments: %w", err)
		}
		marker := admissionDeclineCommentMarker(decline.ID)
		for _, comment := range comments {
			if strings.Contains(comment.Body, marker) {
				if err := m.store.MarkAdmissionDeclineCommented(ctx, decline.ID, m.now().UTC()); err != nil {
					return false, fmt.Errorf("mark backlog admission decline commented: %w", err)
				}
				return false, nil
			}
		}
	}
	if err := issues.CreateComment(ctx, decline.IssueID, admissionDeclineComment(decline, issue.State)); err != nil {
		return false, fmt.Errorf("create backlog admission decline comment: %w", err)
	}
	if err := m.store.MarkAdmissionDeclineCommented(ctx, decline.ID, m.now().UTC()); err != nil {
		return false, fmt.Errorf("mark backlog admission decline commented: %w", err)
	}
	return true, nil
}

func classifyNonDeliverable(issue connector.Issue) (admissionDeclineClassification, bool) {
	body := strings.TrimSpace(issue.Description)
	if strings.Contains(strings.ToLower(body), admissionOptOutMarker) {
		return admissionDeclineClassification{
			reason: admissionDeclineExplicitOptOut,
			detail: "the issue contains the " + admissionOptOutMarker + " operator opt-out marker",
		}, true
	}
	if hasCompletionContract(body) {
		return admissionDeclineClassification{}, false
	}
	if kind := selfIdentifiedArtifact(issue.Title, body); kind != "" {
		return admissionDeclineClassification{
			reason: kind,
			detail: "the title or body identifies this as a " + kind + " artifact without an explicit completion contract",
		}, true
	}
	if predominantlyLinkedChecklist(body) {
		return admissionDeclineClassification{
			reason: admissionDeclineLinkedChecklist,
			detail: "the body is predominantly a checklist of links to other issues without an explicit completion contract",
		}, true
	}
	return admissionDeclineClassification{}, false
}

func hasCompletionContract(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		heading = strings.TrimSuffix(strings.ToLower(heading), ":")
		switch heading {
		case "acceptance criteria", "completion criteria", "definition of done", "deliverable", "expected behavior", "what good looks like":
			return true
		}
		if key, value, found := strings.Cut(line, ":"); found && strings.EqualFold(strings.TrimSpace(key), "deliverable") && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func selfIdentifiedArtifact(title string, body string) string {
	if match := artifactTitlePattern.FindStringSubmatch(strings.TrimSpace(title)); len(match) > 1 {
		for _, candidate := range match[1:] {
			if kind := normalizeArtifactKind(candidate); kind != "" {
				return kind
			}
		}
	}
	lowerBody := strings.ToLower(body)
	phrases := []struct {
		text string
		kind string
	}{
		{text: "this is a master tracker", kind: admissionDeclineTracker},
		{text: "this issue is a tracker", kind: admissionDeclineTracker},
		{text: "this issue serves as a tracker", kind: admissionDeclineTracker},
		{text: "this is an intake issue", kind: admissionDeclineIntake},
		{text: "this issue is an intake", kind: admissionDeclineIntake},
		{text: "this is a study artifact", kind: admissionDeclineStudy},
		{text: "this issue is a study artifact", kind: admissionDeclineStudy},
		{text: "this is a research artifact", kind: admissionDeclineResearch},
		{text: "this issue is a research artifact", kind: admissionDeclineResearch},
	}
	for _, phrase := range phrases {
		if strings.Contains(lowerBody, phrase.text) {
			return phrase.kind
		}
	}
	for _, line := range strings.Split(lowerBody, "\n") {
		line = strings.Trim(strings.TrimSpace(line), "*_`")
		key, value, found := strings.Cut(line, ":")
		if !found || (strings.TrimSpace(key) != "type" && strings.TrimSpace(key) != "artifact") {
			continue
		}
		if kind := normalizeArtifactKind(value); kind != "" {
			return kind
		}
	}
	return ""
}

func normalizeArtifactKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.Contains(value, "tracker") {
		return admissionDeclineTracker
	}
	switch value {
	case admissionDeclineIntake, admissionDeclineStudy, admissionDeclineResearch:
		return value
	default:
		return ""
	}
}

func predominantlyLinkedChecklist(body string) bool {
	total := 0
	linked := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 5 || (line[:5] != "- [ ]" && strings.ToLower(line[:5]) != "- [x]" && line[:5] != "* [ ]" && strings.ToLower(line[:5]) != "* [x]") {
			continue
		}
		total++
		if issueReferencePattern.MatchString(line[5:]) {
			linked++
		}
	}
	return total >= 3 && linked*3 >= total*2
}

func admissionDeclineID(projectID string, issueID string, fingerprint string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(projectID) + "\x00" + strings.TrimSpace(issueID) + "\x00" + fingerprint))
	return "admission-decline-" + hex.EncodeToString(sum[:12])
}

func admissionDeclineComment(decline admissionmodel.Decline, sourceState string) string {
	var b strings.Builder
	b.WriteString("## Detent backlog admission declined\n\n")
	b.WriteString("Detent did not propose this issue for dispatch because ")
	b.WriteString(decline.Detail)
	b.WriteString(". The issue remains")
	if sourceState = strings.TrimSpace(sourceState); sourceState != "" {
		b.WriteString(" in **")
		b.WriteString(sourceState)
		b.WriteString("**")
	} else {
		b.WriteString(" in its current untracked state")
	}
	b.WriteString(".\n\nDefine one bounded deliverable and an explicit completion contract in the title or body to make it eligible. Remove the `")
	b.WriteString(admissionOptOutMarker)
	b.WriteString("` marker first when the issue was deliberately opted out. Detent will re-evaluate only after the title or body changes.\n\n")
	b.WriteString(admissionDeclineCommentMarker(decline.ID))
	return b.String()
}

func admissionDeclineCommentMarker(id string) string {
	return "<!-- detent-backlog-admission-decline:" + strings.TrimSpace(id) + " -->"
}

func (m *Manager) executeEvaluations(
	ctx context.Context,
	settings Settings,
	candidates []connector.Issue,
	evaluations []AgentEvaluation,
	result Result,
	commentsRemaining int,
	autoAdmitsRemaining int,
	at time.Time,
) (Result, error) {
	if len(evaluations) != len(candidates) {
		return result, fmt.Errorf("%w: got %d evaluations for %d candidates", ErrInvalidOutput, len(evaluations), len(candidates))
	}
	ids := make([]string, 0, len(candidates))
	candidateByID := issueMap(candidates)
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	evaluationByID := make(map[string]AgentEvaluation, len(evaluations))
	for _, evaluation := range evaluations {
		issueID := strings.TrimSpace(evaluation.IssueID)
		if _, supplied := candidateByID[issueID]; !supplied {
			return result, fmt.Errorf("%w: evaluation references unknown candidate %q", ErrInvalidOutput, issueID)
		}
		if _, duplicate := evaluationByID[issueID]; duplicate {
			return result, fmt.Errorf("%w: duplicate evaluation for candidate %q", ErrInvalidOutput, issueID)
		}
		evaluation.Disposition = strings.TrimSpace(evaluation.Disposition)
		if evaluation.Disposition != admissionDispositionProposed && evaluation.Disposition != admissionDispositionDeclined {
			return result, fmt.Errorf("%w: invalid disposition for candidate %q", ErrInvalidOutput, issueID)
		}
		if err := validateAdmissionConfidence(evaluation.Confidence); err != nil {
			return result, fmt.Errorf("%w: invalid confidence for candidate %q", ErrInvalidOutput, issueID)
		}
		if evaluation.Disposition == admissionDispositionDeclined {
			failed, err := validateDeclineFindings(evaluation.Findings, settings.Criteria)
			if err != nil || strings.TrimSpace(evaluation.RecommendedEffort) != "" || strings.TrimSpace(evaluation.EffortRationale) != "" {
				return result, fmt.Errorf("%w: invalid decline for candidate %q", ErrInvalidOutput, issueID)
			}
			evaluation.Findings = []admissionmodel.Finding{failed}
		} else {
			findings, err := validateFindings(evaluation.Findings, settings.Criteria)
			if err != nil {
				return result, fmt.Errorf("%w: invalid proposal for candidate %q", ErrInvalidOutput, issueID)
			}
			evaluation.Findings = findings
			recommendedEffort, effortRationale, err := validateRecommendedEffort(
				evaluation.RecommendedEffort,
				evaluation.EffortRationale,
				settings.EffortRubric,
				settings.Config.RequireEffort,
			)
			if err != nil {
				return result, fmt.Errorf("%w: invalid effort for candidate %q", ErrInvalidOutput, issueID)
			}
			evaluation.RecommendedEffort = recommendedEffort
			evaluation.EffortRationale = effortRationale
		}
		evaluationByID[issueID] = evaluation
	}
	fresh, err := settings.Issues.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		return result, fmt.Errorf("revalidate backlog admission candidates: %w", err)
	}
	type evaluationPlan struct {
		issue          connector.Issue
		stale          bool
		classification *admissionDeclineClassification
		proposal       *admissionmodel.Proposal
	}
	freshByID := issueMap(fresh)
	plans := make([]evaluationPlan, 0, len(candidates))
	proposalCount := 0
	for _, original := range candidates {
		issueID := strings.TrimSpace(original.ID)
		evaluation := evaluationByID[issueID]
		current, currentFound := freshByID[issueID]
		if !currentFound || issueFingerprint(original) != issueFingerprint(current) ||
			!eligibleCandidate(current, settings.Config, settings.TerminalStates) {
			plans = append(plans, evaluationPlan{issue: original, stale: true})
			continue
		}
		if classification, declined := classifyNonDeliverable(current); declined {
			plans = append(plans, evaluationPlan{issue: current, classification: &classification})
			continue
		}
		if evaluation.Disposition == admissionDispositionDeclined {
			failed := evaluation.Findings[0]
			confidence := *evaluation.Confidence
			classification := admissionDeclineClassification{
				reason:          admissionDeclineCriteriaNotMet,
				detail:          failed.Rationale,
				confidence:      &confidence,
				failedDimension: failed.Dimension,
				failedCriterion: failed.CriterionQuote,
			}
			plans = append(plans, evaluationPlan{issue: current, classification: &classification})
			continue
		}
		proposal := admissionmodel.Proposal{
			ID:                proposalID(settings.ProjectID, current.ID, issueFingerprint(current), at),
			ProjectID:         settings.ProjectID,
			IssueID:           current.ID,
			IssueIdentifier:   current.Identifier,
			IssueURL:          current.URL,
			TargetState:       settings.Config.TargetState,
			Fingerprint:       issueFingerprint(current),
			CriteriaSection:   settings.Criteria.Section,
			CriteriaText:      settings.Criteria.Text,
			Findings:          evaluation.Findings,
			Confidence:        *evaluation.Confidence,
			RecommendedEffort: evaluation.RecommendedEffort,
			EffortRationale:   evaluation.EffortRationale,
			Status:            admissionmodel.ProposalOpen,
			CreatedAt:         at,
			ExpiresAt:         at.AddDate(0, 0, settings.Config.ProposalExpiryDays),
		}
		plans = append(plans, evaluationPlan{issue: current, proposal: &proposal})
		proposalCount++
	}
	open, err := m.store.CountOpenAdmissionProposals(ctx, settings.ProjectID)
	if err != nil {
		return result, err
	}
	if open+proposalCount > settings.Config.MaxOpenProposals {
		return result, errors.New("backlog admission proposal capacity changed during evaluation")
	}
	for _, plan := range plans {
		if plan.stale {
			result.Skipped["stale_or_ineligible"]++
			continue
		}
		if plan.classification != nil {
			decline, created, err := m.createAdmissionDecline(ctx, settings, plan.issue, *plan.classification, at)
			if err != nil {
				return result, err
			}
			if plan.classification.reason == admissionDeclineCriteriaNotMet {
				result.Skipped[admissionDeclineCriteriaNotMet]++
				continue
			}
			commented, err := m.ensureAdmissionDeclineComment(
				ctx,
				settings.Issues,
				plan.issue,
				decline,
				created,
				commentsRemaining > 0,
			)
			if err != nil {
				return result, err
			}
			if commented {
				commentsRemaining--
			}
			result.Skipped["non_deliverable"]++
			continue
		}
		proposal := *plan.proposal
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
			if err := m.commentProposal(ctx, settings, proposal, plan.issue); err != nil {
				return result, err
			}
			commentsRemaining--
		}
		if autoAdmitsRemaining > 0 && autoAdmitProposal(settings.Config, settings.Criteria, proposal, plan.issue.Labels) {
			if err := m.admitProposal(
				ctx,
				settings,
				plan.issue,
				proposal,
				automaticAdmissionDecision(settings.Issues, proposal, at),
			); err != nil {
				return result, err
			}
			autoAdmitsRemaining--
		}
	}
	if len(result.Proposals) == 0 {
		switch {
		case result.Skipped[admissionDeclineCriteriaNotMet] > 0:
			result.ProposalReason = admissionDeclineCriteriaNotMet
		case result.Skipped["unchanged_open_proposal"] > 0:
			result.ProposalReason = "unchanged_open_proposal"
		case result.Skipped["non_deliverable"] > 0:
			result.ProposalReason = "non_deliverable"
		case result.Skipped["stale_or_ineligible"] > 0:
			result.ProposalReason = "stale_or_ineligible"
		default:
			return result, errors.New("backlog admission completed without a candidate disposition")
		}
	}
	return result, nil
}

func (m *Manager) commentProposal(
	ctx context.Context,
	settings Settings,
	proposal admissionmodel.Proposal,
	issue connector.Issue,
) error {
	untracked := settings.Config.Sources.Untracked && strings.TrimSpace(issue.State) == ""
	if err := settings.Issues.CreateComment(
		ctx,
		proposal.IssueID,
		proposalComment(
			proposal,
			settings.Criteria,
			autoAdmitProposal(settings.Config, settings.Criteria, proposal, issue.Labels),
			untracked,
			issue.State,
		),
	); err != nil {
		return fmt.Errorf("create backlog admission audit comment: %w", err)
	}
	if err := m.store.MarkAdmissionProposalCommented(ctx, proposal.ID, m.now().UTC()); err != nil {
		return fmt.Errorf("mark backlog admission audit comment: %w", err)
	}
	return nil
}

func (m *Manager) admitProposal(
	ctx context.Context,
	settings Settings,
	issue connector.Issue,
	proposal admissionmodel.Proposal,
	decision admissionmodel.Decision,
) error {
	issues, err := settings.Issues.FetchIssueStatesByIDs(ctx, []string{proposal.IssueID})
	if err != nil {
		return fmt.Errorf("revalidate admitted backlog issue %s: %w", proposal.IssueIdentifier, err)
	}
	current, found := issueMap(issues)[proposal.IssueID]
	eligible := found && admissionSourceEligible(current, settings.Config)
	if decision.Automatic {
		eligible = found && autoAdmitCandidateEligible(current, settings.Config) &&
			autoAdmitProposal(settings.Config, settings.Criteria, proposal, current.Labels)
	}
	if !eligible {
		decision.Outcome = admissionmodel.ProposalSuperseded
		decision.Reason = admissionResolutionSourceStateChanged
		if decision.Automatic {
			decision.Reason = admissionResolutionAutoAdmitIneligible
		}
		if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
			return fmt.Errorf("resolve stale backlog proposal %s: %w", proposal.ID, err)
		}
		return nil
	}
	issue = current
	if settings.Config.RequireEffort {
		if err := m.writeRecommendedEffort(ctx, settings, issue, proposal, &decision); err != nil {
			return err
		}
		if decision.Outcome == admissionmodel.ProposalSuperseded {
			return nil
		}
	}
	if err := settings.Issues.UpdateIssueState(ctx, proposal.IssueID, proposal.TargetState); err != nil {
		return fmt.Errorf("admit backlog issue %s to %s: %w", proposal.IssueIdentifier, proposal.TargetState, err)
	}
	decision.TransitionAt = m.now().UTC()
	if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
		return fmt.Errorf("resolve admitted backlog proposal %s: %w", proposal.ID, err)
	}
	telemetry.LogLifecycleMessageContext(ctx, m.logger, slog.LevelInfo, telemetry.LifecycleAdmission, "backlog_admission_proposal_admitted", "backlog admission proposal admitted", telemetry.LifecycleCorrelation{
		ProjectID:       proposal.ProjectID,
		IssueID:         proposal.IssueID,
		IssueIdentifier: proposal.IssueIdentifier,
	},
		"from_state", issue.State,
		"target_state", proposal.TargetState,
		"proposal_id", proposal.ID,
		"decision_actor", decision.ActorLogin,
		"resolution_reason", decision.Reason,
	)
	return nil
}

func (m *Manager) writeRecommendedEffort(
	ctx context.Context,
	settings Settings,
	issue connector.Issue,
	proposal admissionmodel.Proposal,
	decision *admissionmodel.Decision,
) error {
	_, found, parseErr := agentoverride.FromIssueBody(issue.Description)
	if parseErr != nil {
		return m.supersedeProposalWithoutEffort(ctx, proposal, decision)
	}
	if found {
		return nil
	}
	effort, _, err := validateRecommendedEffort(
		proposal.RecommendedEffort,
		proposal.EffortRationale,
		settings.EffortRubric,
		true,
	)
	if err != nil {
		return m.supersedeProposalWithoutEffort(ctx, proposal, decision)
	}
	updater, ok := settings.Issues.(connector.IssueBodyUpdater)
	if !ok {
		return errors.New("backlog admission issue body updater is required")
	}
	body := appendRecommendedEffortBlock(issue.Description, effort)
	if err := updater.UpdateIssueBody(ctx, proposal.IssueID, body); err != nil {
		return fmt.Errorf("write recommended effort for backlog issue %s: %w", proposal.IssueIdentifier, err)
	}
	return nil
}

func (m *Manager) supersedeProposalWithoutEffort(
	ctx context.Context,
	proposal admissionmodel.Proposal,
	decision *admissionmodel.Decision,
) error {
	decision.Outcome = admissionmodel.ProposalSuperseded
	decision.Reason = admissionResolutionEffortUnavailable
	if err := m.store.ResolveAdmissionProposal(ctx, *decision); err != nil {
		return fmt.Errorf("resolve backlog proposal without required effort %s: %w", proposal.ID, err)
	}
	return nil
}

func (m *Manager) recoverAutomaticAdmission(
	ctx context.Context,
	settings Settings,
	issue connector.Issue,
	proposal admissionmodel.Proposal,
) (bool, error) {
	if !strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(proposal.TargetState)) {
		return false, nil
	}
	transition, found, err := admissionIssueTransition(ctx, settings.Issues, issue)
	if err != nil {
		return false, fmt.Errorf("read automatic backlog admission transition %s: %w", proposal.ID, err)
	}
	if !found || transition.EnteredAt.Before(proposal.CreatedAt) {
		return false, nil
	}
	decision := automaticAdmissionDecision(settings.Issues, proposal, transition.EnteredAt)
	decision.TransitionAt = transition.EnteredAt
	if err := m.store.ResolveAdmissionProposal(ctx, decision); err != nil {
		return false, fmt.Errorf("recover automatic backlog admission %s: %w", proposal.ID, err)
	}
	return true, nil
}

func proposalDecision(
	ctx context.Context,
	issues IssueStore,
	issue connector.Issue,
	proposal admissionmodel.Proposal,
	comments []connector.IssueComment,
) (admissionmodel.Decision, bool, error) {
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
		authorized := comment.AuthorAuthorized
		if authorizer, ok := issues.(connector.IssueCommentAuthorizer); ok {
			var err error
			authorized, err = authorizer.IsIssueCommentAuthorAuthorized(ctx, issue, comment)
			if err != nil {
				return admissionmodel.Decision{}, false, err
			}
		}
		if !authorized {
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
	return decision, !decision.DecidedAt.IsZero(), nil
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

func admissionSourceEligible(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return !issue.Closed && containsFold(cfg.Sources.States, issue.State)
}

func autoAdmitCandidateEligible(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return admissionSourceEligible(issue, cfg) && !excludedCandidate(issue, cfg)
}

func autoAdmitProposal(
	cfg config.BacklogAdmission,
	criteria config.AdmissionCriteria,
	proposal admissionmodel.Proposal,
	labels []string,
) bool {
	return cfg.AutoAdmitForLabels(labels) &&
		proposal.Confidence >= cfg.AutoAdmitMinConfidence &&
		len(failedAdmissionDimensions(proposal.Findings, criteria)) == 0
}

func automaticAdmissionDecision(
	issues IssueStore,
	proposal admissionmodel.Proposal,
	at time.Time,
) admissionmodel.Decision {
	actorLogin := "detent"
	if identity, ok := issues.(connector.InstanceIdentifier); ok {
		if login := strings.TrimSpace(identity.InstanceLogin()); login != "" {
			actorLogin = login
		}
	}
	return admissionmodel.Decision{
		ProposalID: proposal.ID,
		Outcome:    admissionmodel.ProposalAccepted,
		DecidedAt:  at.UTC(),
		ActorLogin: actorLogin,
		ActorKind:  "Bot",
		Reason:     admissionResolutionAutoAdmit,
		Automatic:  true,
	}
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
		settings.GlobalDispatchGate.MarkIdle(candidate)
		return func() error { return nil }, false, "fleet_capacity", releaseLocal()
	}
	return func() error {
		releaseGlobal := settings.GlobalDispatchGate.Release(slot)
		if releaseGlobal == nil {
			settings.GlobalDispatchGate.MarkIdle(candidate)
		}
		return errors.Join(releaseGlobal, releaseLocal())
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

func filterCandidates(
	issues []connector.Issue,
	cfg config.BacklogAdmission,
	terminalStates []string,
	skipped map[string]int,
	hasPushedAuthorRejections bool,
) []connector.Issue {
	out := make([]connector.Issue, 0, len(issues))
	for _, issue := range issues {
		switch {
		case issue.Closed:
			skipped["closed"]++
		case labelCandidateInTargetState(issue, cfg):
			skipped["label_target_state"]++
		case labelCandidateStateBlocked(issue, cfg):
			skipped["label_blocked_state"]++
		case labelCandidateStateTerminal(issue, cfg, terminalStates):
			skipped["label_terminal_state"]++
		case excludedByLabel(issue, cfg.ExcludeLabels):
			skipped["excluded_label"]++
		case !allowedAuthor(issue, cfg.Authors):
			if !hasPushedAuthorRejections || !containsFold(cfg.Sources.States, issue.State) {
				skipped["author"]++
			}
		default:
			out = append(out, issue)
		}
	}
	return out
}

func eligibleCandidate(issue connector.Issue, cfg config.BacklogAdmission, terminalStates []string) bool {
	if issue.Closed || excludedCandidate(issue, cfg) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(cfg.TargetState)) {
		return false
	}
	if cfg.Sources.Untracked && strings.TrimSpace(issue.State) == "" {
		return true
	}
	if containsFold(cfg.Sources.States, issue.State) {
		return true
	}
	return matchesAnyLabel(issue.Labels, cfg.Sources.Labels) &&
		!strings.EqualFold(strings.TrimSpace(issue.State), "Blocked") &&
		!containsFold(terminalStates, issue.State)
}

func labelCandidateInTargetState(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return matchesAnyLabel(issue.Labels, cfg.Sources.Labels) &&
		strings.EqualFold(strings.TrimSpace(issue.State), strings.TrimSpace(cfg.TargetState))
}

func labelCandidateStateBlocked(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return !containsFold(cfg.Sources.States, issue.State) &&
		matchesAnyLabel(issue.Labels, cfg.Sources.Labels) &&
		strings.EqualFold(strings.TrimSpace(issue.State), "Blocked")
}

func labelCandidateStateTerminal(issue connector.Issue, cfg config.BacklogAdmission, terminalStates []string) bool {
	return !containsFold(cfg.Sources.States, issue.State) &&
		matchesAnyLabel(issue.Labels, cfg.Sources.Labels) &&
		containsFold(terminalStates, issue.State)
}

func excludedCandidate(issue connector.Issue, cfg config.BacklogAdmission) bool {
	return excludedByLabel(issue, cfg.ExcludeLabels) || !allowedAuthor(issue, cfg.Authors)
}

func matchesAnyLabel(issueLabels []string, labels []string) bool {
	for _, issueLabel := range issueLabels {
		if containsFold(labels, issueLabel) {
			return true
		}
	}
	return false
}

func excludedByLabel(issue connector.Issue, labels []string) bool {
	for _, issueLabel := range issue.Labels {
		if containsFold(labels, issueLabel) {
			return true
		}
	}
	return false
}

func allowedAuthor(issue connector.Issue, authors config.BacklogAdmissionAuthors) bool {
	if len(authors.Allow) == 0 && len(authors.AllowAssociation) == 0 {
		return true
	}
	if containsFold(authors.Allow, strings.TrimPrefix(strings.TrimSpace(issue.AuthorID), "@")) {
		return true
	}
	return containsFold(authors.AllowAssociation, string(connector.NormalizeAuthorAssociation(string(issue.AuthorAssociation))))
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
	out, matched, err := validateEvaluationFindings(findings, criteria)
	if err != nil || !matched {
		return nil, ErrInvalidProposal
	}
	return out, nil
}

func validateDeclineFindings(
	findings []admissionmodel.Finding,
	criteria config.AdmissionCriteria,
) (admissionmodel.Finding, error) {
	out, matched, err := validateEvaluationFindings(findings, criteria)
	if err != nil || matched {
		return admissionmodel.Finding{}, ErrInvalidProposal
	}
	byDimension := make(map[string]admissionmodel.Finding, len(out))
	for _, finding := range out {
		byDimension[strings.ToLower(finding.Dimension)] = finding
	}
	for _, dimension := range criteria.Dimensions {
		if finding, ok := byDimension[strings.ToLower(strings.TrimSpace(dimension.Name))]; ok {
			return finding, nil
		}
	}
	return admissionmodel.Finding{}, ErrInvalidProposal
}

func validateEvaluationFindings(
	findings []admissionmodel.Finding,
	criteria config.AdmissionCriteria,
) ([]admissionmodel.Finding, bool, error) {
	if len(findings) == 0 || len(findings) != len(criteria.Dimensions) {
		return nil, false, ErrInvalidProposal
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
			return nil, false, ErrInvalidProposal
		}
		if _, ok := seen[key]; ok {
			return nil, false, ErrInvalidProposal
		}
		seen[key] = struct{}{}
		matched = matched || finding.Matched
		out = append(out, finding)
	}
	for key := range dimensions {
		if _, ok := seen[key]; !ok {
			return nil, false, ErrInvalidProposal
		}
	}
	return out, matched, nil
}

func validateAdmissionConfidence(confidence *float64) error {
	if confidence == nil || math.IsNaN(*confidence) || math.IsInf(*confidence, 0) ||
		*confidence < 0 || *confidence > 1 {
		return ErrInvalidProposal
	}
	return nil
}

func failedAdmissionDimensions(
	findings []admissionmodel.Finding,
	criteria config.AdmissionCriteria,
) []string {
	matched := make(map[string]bool, len(findings))
	for _, finding := range findings {
		key := strings.ToLower(strings.TrimSpace(finding.Dimension))
		if key == "" || !finding.Matched {
			continue
		}
		matched[key] = true
	}
	failed := make([]string, 0, len(criteria.Dimensions))
	for _, dimension := range criteria.Dimensions {
		if !matched[strings.ToLower(strings.TrimSpace(dimension.Name))] {
			failed = append(failed, strings.TrimSpace(dimension.Name))
		}
	}
	return failed
}

func validateRecommendedEffort(
	effort string,
	rationale string,
	rubric config.AdmissionEffortRubric,
	required bool,
) (string, string, error) {
	effort = strings.TrimSpace(effort)
	rationale = strings.TrimSpace(rationale)
	if effort == "" && rationale == "" && !required {
		return "", "", nil
	}
	if effort == "" || rationale == "" || len(rationale) > maxEffortRationaleSize || !config.ValidAdmissionEffortValue(effort) {
		return "", "", ErrInvalidProposal
	}
	if len(rubric.Efforts) == 0 {
		if required {
			return "", "", ErrInvalidProposal
		}
		return effort, rationale, nil
	}
	for _, allowed := range rubric.Efforts {
		if strings.EqualFold(strings.TrimSpace(allowed), effort) {
			return strings.TrimSpace(allowed), rationale, nil
		}
	}
	return "", "", ErrInvalidProposal
}

func appendRecommendedEffortBlock(body string, effort string) string {
	body = strings.TrimRight(body, " \t\r\n")
	if body != "" {
		body += "\n\n"
	}
	return body + "```detent-agent\nschema: 1\neffort: " + effort + "\n```\n"
}

func issueFingerprint(issue connector.Issue) string {
	sum := sha256.Sum256([]byte(
		strconv.Itoa(len(issue.Title)) + ":" + issue.Title +
			strconv.Itoa(len(issue.Description)) + ":" + issue.Description,
	))
	return hex.EncodeToString(sum[:])
}

func criteriaDeclineFingerprint(issue connector.Issue, criteria config.AdmissionCriteria) string {
	sum := sha256.Sum256([]byte(
		issueFingerprint(issue) + "\x00" +
			strconv.Itoa(len(criteria.Section)) + ":" + criteria.Section +
			strconv.Itoa(len(criteria.Text)) + ":" + criteria.Text,
	))
	return hex.EncodeToString(sum[:])
}

func admissionDeclineFingerprint(
	issue connector.Issue,
	classification admissionDeclineClassification,
	criteria config.AdmissionCriteria,
) string {
	if classification.reason == admissionDeclineCriteriaNotMet {
		return criteriaDeclineFingerprint(issue, criteria)
	}
	return issueFingerprint(issue)
}

func proposalID(projectID string, issueID string, fingerprint string, at time.Time) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(projectID) + "\x00" + strings.TrimSpace(issueID) + "\x00" + fingerprint + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return "admission-" + hex.EncodeToString(sum[:12])
}

func proposalComment(
	proposal admissionmodel.Proposal,
	criteria config.AdmissionCriteria,
	autoAdmit bool,
	untracked bool,
	sourceState string,
) string {
	var b strings.Builder
	failedDimensions := failedAdmissionDimensions(proposal.Findings, criteria)
	b.WriteString("## Detent backlog admission proposal\n\n")
	b.WriteString("Proposal `")
	b.WriteString(proposal.ID)
	b.WriteString("` recommends moving this issue to **")
	b.WriteString(proposal.TargetState)
	if len(failedDimensions) > 0 {
		b.WriteString("**. This proposal does not meet every required admission dimension, so Detent will not automatically move the issue. The issue remains")
		if sourceState = strings.TrimSpace(sourceState); sourceState != "" {
			b.WriteString(" in **")
			b.WriteString(sourceState)
			b.WriteString("**")
		} else {
			b.WriteString(" in its current untracked state")
		}
		b.WriteString(". To accept and have Detent move the issue, reply with `")
		b.WriteString(admissionAcceptCommand(proposal.ID))
		b.WriteString("`. To reject, reply with `")
		b.WriteString(admissionRejectCommand(proposal.ID))
		b.WriteString("`. Leaving it unactioned will expire the proposal without counting as rejection.\n\n")
	} else if autoAdmit {
		b.WriteString("** and meets the configured automatic-admission threshold. Detent will move the issue to that state and record this proposal as the provenance.\n\n")
	} else {
		b.WriteString("**. To accept and have Detent move the issue, reply with `")
		b.WriteString(admissionAcceptCommand(proposal.ID))
		b.WriteString("`. To reject, reply with `")
		b.WriteString(admissionRejectCommand(proposal.ID))
		b.WriteString("`. Leaving it unactioned will expire the proposal without counting as rejection.\n\n")
	}
	if untracked {
		b.WriteString("This issue has no configured status label. ")
		if autoAdmit {
			b.WriteString("Automatic admission is a two-part change: assigning **")
		} else {
			b.WriteString("Acceptance is a two-part change: assigning **")
		}
		b.WriteString(proposal.TargetState)
		b.WriteString("** status and admitting the work for dispatch.\n\n")
	}
	b.WriteString("Criteria section: **")
	b.WriteString(proposal.CriteriaSection)
	b.WriteString("**\n\n")
	for _, finding := range proposal.Findings {
		b.WriteString("- **")
		b.WriteString(finding.Dimension)
		b.WriteString("** — ")
		if !finding.Matched {
			b.WriteString("**Failed.** ")
		}
		b.WriteString(finding.Rationale)
		b.WriteString("\n  - Criterion: “")
		b.WriteString(finding.CriterionQuote)
		b.WriteString("”\n")
	}
	if len(failedDimensions) > 0 {
		b.WriteString("\nThe following required admission dimensions failed: **")
		b.WriteString(strings.Join(failedDimensions, "**, **"))
		b.WriteString("**.\n")
	}
	if proposal.RecommendedEffort != "" {
		b.WriteString("\nRecommended effort: `")
		b.WriteString(proposal.RecommendedEffort)
		b.WriteString("`")
		if proposal.EffortRationale != "" {
			b.WriteString(" — ")
			b.WriteString(proposal.EffortRationale)
		}
		b.WriteString("\n")
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
		EffortSection:   settings.EffortRubric.Section,
		EffortText:      settings.EffortRubric.Text,
		AllowedEfforts:  append([]string(nil), settings.EffortRubric.Efforts...),
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

func proposalTool(requireEffort bool) runner.AgentTool {
	effortRequirement := ""
	if requireEffort {
		effortRequirement = `,"allOf":[{"if":{"properties":{"disposition":{"const":"proposed"}}},"then":{"required":["recommended_effort","effort_rationale"]}}]`
	}
	return runner.AgentTool{
		Name:        ProposalToolName,
		Description: "Record the terminal admission evaluation for one supplied candidate using only project-owned criteria.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["issue_id","disposition","findings","confidence"],"properties":{"issue_id":{"type":"string","minLength":1},"disposition":{"type":"string","enum":["proposed","declined"]},"findings":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"required":["dimension","criterion_quote","matched","rationale"],"properties":{"dimension":{"type":"string","minLength":1},"criterion_quote":{"type":"string","minLength":1},"matched":{"type":"boolean"},"rationale":{"type":"string","minLength":1}}}},"confidence":{"type":"number","minimum":0,"maximum":1},"recommended_effort":{"type":"string","minLength":1},"effort_rationale":{"type":"string","minLength":1}}` + effortRequirement + `}`),
	}
}

type proposalCollector struct {
	mu          sync.Mutex
	evaluations []AgentEvaluation
	err         error
}

func (c *proposalCollector) handle(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	if call.Name != ProposalToolName {
		return runner.AgentToolResult{Content: "unsupported tool", Success: false}, nil
	}
	var evaluation AgentEvaluation
	if err := decodeStrictJSON(call.Arguments, &evaluation); err != nil {
		c.mu.Lock()
		c.err = errors.Join(c.err, fmt.Errorf("%w: %w", ErrInvalidOutput, err))
		c.mu.Unlock()
		return runner.AgentToolResult{Content: "invalid evaluation", Success: false}, nil
	}
	c.mu.Lock()
	c.evaluations = append(c.evaluations, evaluation)
	c.mu.Unlock()
	return runner.AgentToolResult{Content: "evaluation received", Success: true}, nil
}

func (c *proposalCollector) result() ([]AgentEvaluation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]AgentEvaluation(nil), c.evaluations...), c.err
}

func parseEvaluations(output string) ([]AgentEvaluation, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, nil
	}
	var envelope struct {
		Evaluations *[]AgentEvaluation `json:"evaluations"`
	}
	if err := decodeStrictJSON([]byte(output), &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOutput, err)
	}
	if envelope.Evaluations == nil {
		return nil, fmt.Errorf("%w: evaluations is required", ErrInvalidOutput)
	}
	return *envelope.Evaluations, nil
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

func admissionCandidateKey(issue connector.Issue) string {
	if value := strings.TrimSpace(issue.ID); value != "" {
		return "id:" + value
	}
	if value := strings.TrimSpace(issue.Identifier); value != "" {
		return "identifier:" + strings.ToLower(value)
	}
	if value := strings.TrimSpace(issue.URL); value != "" {
		return "url:" + strings.ToLower(value)
	}
	return issueFingerprint(issue)
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
	settings.Config.Sources.Labels = append([]string(nil), settings.Config.Sources.Labels...)
	settings.Config.ExcludeLabels = append([]string(nil), settings.Config.ExcludeLabels...)
	settings.Config.AutoAdmitByLabel = cloneLabelPolicies(settings.Config.AutoAdmitByLabel)
	settings.Config.Authors.Allow = append([]string(nil), settings.Config.Authors.Allow...)
	settings.Config.Authors.AllowAssociation = append([]string(nil), settings.Config.Authors.AllowAssociation...)
	settings.Criteria.Dimensions = append([]config.AdmissionDimension(nil), settings.Criteria.Dimensions...)
	settings.EffortRubric.Efforts = append([]string(nil), settings.EffortRubric.Efforts...)
	settings.DispatchStates = append([]string(nil), settings.DispatchStates...)
	settings.DispatchLabels = append([]string(nil), settings.DispatchLabels...)
	settings.TerminalStates = append([]string(nil), settings.TerminalStates...)
	settings.ProjectCandidate.ActiveHours = settings.ProjectCandidate.ActiveHours.Normalize()
	return settings
}

func cloneLabelPolicies(policies map[string]bool) map[string]bool {
	out := make(map[string]bool, len(policies))
	for label, enabled := range policies {
		out[label] = enabled
	}
	return out
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
	m.logScheduledCompletion(ctx, result)
}

func (m *Manager) logScheduledCompletion(ctx context.Context, result Result) {
	telemetry.LogLifecycleMessageContext(ctx, m.logger, slog.LevelInfo, telemetry.LifecycleAdmission, "scheduled_backlog_admission_completed", "scheduled backlog admission completed", telemetry.LifecycleCorrelation{ProjectID: result.ProjectID},
		"items_read", result.ItemsRead,
		"candidates_found", result.CandidatesFound,
		"candidates", result.Candidates,
		"proposals", len(result.Proposals),
		"truncated", len(result.Truncated) > 0,
		"truncations", result.Truncated,
		"deferred_reason", result.DeferredReason,
		"proposal_reason", result.ProposalReason,
	)
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}
