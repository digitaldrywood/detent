package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

const heartbeatOperationTimeout = 30 * time.Second

type claimHeartbeatSource interface {
	RenewClaimHeartbeat(context.Context, string, time.Time) (Claimed, error)
}

type claimHeartbeatClock interface {
	ClaimHeartbeatDeadline(string) time.Time
}

type trackerClaimLeaseRenewer interface {
	CompareAndRenewClaimLease(ctx context.Context, observed connector.Issue, ownerField, leaseField string, renewedAt, expiresAt time.Time) (bool, error)
}

type heartbeatManager struct {
	mu           sync.Mutex
	settings     heartbeatSettings
	targets      map[string]heartbeatTarget
	nextSequence uint64
	wake         chan struct{}
	results      chan heartbeatResult
	running      atomic.Bool
	now          func() time.Time
	logger       *slog.Logger
}

type heartbeatSettings struct {
	connector    connector.Connector
	scheduling   SchedulingSource
	workAttempts store.WorkAttemptStore
	claiming     ClaimingConfig
	interval     time.Duration
	leaseTTL     time.Duration
}

type heartbeatTarget struct {
	progress             *workerProgress
	issueID              string
	claimOwner           string
	localLeaseDeadline   time.Time
	claimLost            bool
	completionCancel     context.CancelCauseFunc
	attemptDeadline      time.Time
	workAttemptHeartbeat store.WorkAttemptHeartbeat
	workerProcess        procgroup.Identity
	workspacePath        string
	workspaceModifiedAt  time.Time
	workspaceAdvanced    bool
	workerAlive          bool
	workerChecked        bool
	livenessError        error
	failures             int
	sequence             uint64
	nextDue              time.Time
	inFlight             bool
	flightDone           chan struct{}
}

type heartbeatResult struct {
	issueID             string
	sequence            uint64
	heartbeat           store.WorkAttemptHeartbeat
	workAttemptRenewed  bool
	claimRenewed        bool
	claimIssue          connector.Issue
	claim               Claimed
	claimOwnerLost      bool
	currentClaimOwner   string
	workerAlive         bool
	workerChecked       bool
	workspaceModifiedAt time.Time
	workspaceAdvanced   bool
	livenessError       error
	workAttemptError    error
	claimError          error
}

func newHeartbeatManager(cfg Config, connectorBackend connector.Connector, workAttempts store.WorkAttemptStore, now func() time.Time, logger *slog.Logger, scheduling ...SchedulingSource) *heartbeatManager {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	manager := &heartbeatManager{
		targets: make(map[string]heartbeatTarget),
		wake:    make(chan struct{}, 1),
		results: make(chan heartbeatResult, runUpdateBufferSize),
		now:     now,
		logger:  logger,
	}
	manager.configure(cfg, connectorBackend, workAttempts, scheduling...)
	return manager
}

func (m *heartbeatManager) configure(cfg Config, connectorBackend connector.Connector, workAttempts store.WorkAttemptStore, scheduling ...SchedulingSource) {
	if m == nil {
		return
	}
	settings := heartbeatSettings{
		connector:    connectorBackend,
		workAttempts: workAttempts,
		claiming:     cfg.Claiming,
		interval:     workHeartbeatInterval(cfg),
		leaseTTL:     cfg.Claiming.LeaseTTL,
	}
	if len(scheduling) > 0 {
		settings.scheduling = scheduling[0]
	} else {
		m.mu.Lock()
		settings.scheduling = m.settings.scheduling
		m.mu.Unlock()
	}
	if settings.scheduling != nil {
		interval := settings.scheduling.HeartbeatInterval()
		if interval > 0 && interval < settings.interval {
			settings.interval = interval
		}
	}
	if settings.leaseTTL <= 0 {
		settings.leaseTTL = defaultWorkAttemptLeaseTTL
	}
	m.mu.Lock()
	m.settings = settings
	now := m.now()
	for issueID, target := range m.targets {
		due := now.Add(target.renewalDelay(now, settings.interval))
		if target.nextDue.IsZero() || target.nextDue.After(due) {
			target.nextDue = due
			m.targets[issueID] = target
		}
	}
	m.mu.Unlock()
	m.notify()
}

func workHeartbeatInterval(cfg Config) time.Duration {
	interval := cfg.Claiming.HeartbeatInterval
	if interval <= 0 && cfg.Claiming.LeaseTTL > 0 {
		interval = cfg.Claiming.LeaseTTL / 2
	}
	leaseTTL := cfg.Claiming.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultWorkAttemptLeaseTTL
	}
	if maximum := leaseTTL / 3; maximum > 0 && (interval <= 0 || interval > maximum) {
		interval = maximum
	}
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return interval
}

func (m *heartbeatManager) upsert(target heartbeatTarget) {
	if m == nil || strings.TrimSpace(target.issueID) == "" {
		return
	}
	m.mu.Lock()
	current, ok := m.targets[target.issueID]
	if ok && current.workAttemptHeartbeat.AttemptID == target.workAttemptHeartbeat.AttemptID && current.progress == target.progress {
		if current.localLeaseDeadline.After(target.localLeaseDeadline) {
			target.localLeaseDeadline = current.localLeaseDeadline
		}
		target.claimLost = current.claimLost
		target.completionCancel = current.completionCancel
		if current.attemptDeadline.After(target.attemptDeadline) {
			target.attemptDeadline = current.attemptDeadline
		}
		target.failures = current.failures
		target.sequence = current.sequence
		target.nextDue = current.nextDue
		target.inFlight = current.inFlight
		target.flightDone = current.flightDone
		target.workspaceModifiedAt = current.workspaceModifiedAt
		target.workspaceAdvanced = current.workspaceAdvanced
		target.workerAlive = current.workerAlive
		target.workerChecked = current.workerChecked
		target.livenessError = current.livenessError
	} else {
		if current.completionCancel != nil {
			current.completionCancel(ErrSchedulingClaimLost)
		}
		m.nextSequence++
		target.sequence = m.nextSequence
		now := m.now()
		if target.workAttemptHeartbeat.AttemptID > 0 {
			target.attemptDeadline = target.workAttemptHeartbeat.LeaseExpiresAt
		}
		target.nextDue = now.Add(target.renewalDelay(now, m.settings.interval))
	}
	m.targets[target.issueID] = target
	m.mu.Unlock()
	m.notify()
}

func (target heartbeatTarget) renewalDelay(now time.Time, interval time.Duration) time.Duration {
	if !target.localLeaseDeadline.IsZero() {
		interval = min(interval, max(0, target.localLeaseDeadline.Sub(now)/3))
	}
	if deadline := target.progress.leaseExpiry(target.attemptDeadline); deadline.After(now) {
		interval = min(interval, deadline.Sub(now)/3)
	}
	return interval
}

func renewalBudget(now, deadline time.Time, maximum time.Duration) time.Duration {
	if deadline.IsZero() {
		return maximum
	}
	return min(maximum, deadline.Sub(now))
}

func claimRenewalDeadline(claim Claimed) time.Time {
	if !claim.LocalLeaseDeadline.IsZero() {
		return claim.LocalLeaseDeadline
	}
	return claim.LeaseExpiresAt
}

func (target heartbeatTarget) completionDeadline() time.Time {
	attempt := target.progress.leaseExpiry(target.attemptDeadline)
	if target.localLeaseDeadline.IsZero() || !attempt.IsZero() && attempt.Before(target.localLeaseDeadline) {
		return attempt
	}
	return target.localLeaseDeadline
}

func (target heartbeatTarget) completionLeaseLost(now time.Time) bool {
	deadline := target.completionDeadline()
	return target.claimLost ||
		!deadline.IsZero() && !now.Before(deadline)
}

func (m *heartbeatManager) completionActive(issueID string, attemptID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, found := m.targets[issueID]
	if !found || target.workAttemptHeartbeat.AttemptID != attemptID {
		return false
	}
	lost := target.completionLeaseLost(m.now())
	if target.completionCancel != nil && lost {
		target.completionCancel(ErrSchedulingClaimLost)
		target.claimLost = true
		m.targets[issueID] = target
	}
	return !lost
}

func (m *heartbeatManager) bindCompletion(ctx context.Context, issueID string, attemptID int64) (context.Context, func()) {
	if m == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	m.mu.Lock()
	target, found := m.targets[issueID]
	if !found || target.workAttemptHeartbeat.AttemptID != attemptID || target.completionLeaseLost(m.now()) {
		m.mu.Unlock()
		cancel(ErrSchedulingClaimLost)
		return ctx, func() {}
	}
	target.completionCancel = cancel
	m.targets[issueID] = target
	m.mu.Unlock()
	m.notify()
	return ctx, func() {
		m.mu.Lock()
		if current, ok := m.targets[issueID]; ok && current.sequence == target.sequence {
			current.completionCancel = nil
			m.targets[issueID] = current
		}
		m.mu.Unlock()
		cancel(nil)
	}
}

func (m *heartbeatManager) remove(issueID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	target := m.targets[strings.TrimSpace(issueID)]
	delete(m.targets, strings.TrimSpace(issueID))
	m.mu.Unlock()
	m.notify()
	target.progress.close()
	if target.flightDone != nil {
		<-target.flightDone
	}
}

func (m *heartbeatManager) protects(issueID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.targets[strings.TrimSpace(issueID)]
	if !ok || target.claimLost {
		return false
	}
	if !target.workerChecked || target.livenessError != nil {
		return true
	}
	return target.workerAlive
}

func (m *heartbeatManager) current(issueID string, sequence uint64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.targets[strings.TrimSpace(issueID)]
	return ok && target.sequence == sequence
}

func (m *heartbeatManager) Running() bool {
	return m != nil && m.running.Load()
}

func (m *heartbeatManager) Run(ctx context.Context) {
	if m == nil || !m.running.CompareAndSwap(false, true) {
		return
	}
	defer m.running.Store(false)
	var workers sync.WaitGroup
	defer workers.Wait()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		resetHeartbeatTimer(timer, m.nextDelay(m.now()))
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			continue
		case <-timer.C:
		}
		for _, target := range m.due(m.now()) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				m.execute(ctx, target)
			}()
		}
	}
}

func (m *heartbeatManager) nextDelay(now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next time.Time
	for _, target := range m.targets {
		if target.claimLost {
			continue
		}
		if deadline := target.completionDeadline(); target.completionCancel != nil && !deadline.IsZero() && (next.IsZero() || deadline.Before(next)) {
			next = deadline
		}
		if !target.inFlight && (next.IsZero() || target.nextDue.Before(next)) {
			next = target.nextDue
		}
	}
	if next.IsZero() {
		return time.Hour
	}
	return max(time.Millisecond, next.Sub(now))
}

func (m *heartbeatManager) due(now time.Time) []heartbeatTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	due := make([]heartbeatTarget, 0, len(m.targets))
	for issueID, target := range m.targets {
		if target.completionCancel != nil && target.completionLeaseLost(now) {
			target.completionCancel(ErrSchedulingClaimLost)
			target.claimLost = true
			m.targets[issueID] = target
		}
		if target.claimLost || target.inFlight || now.Before(target.nextDue) {
			continue
		}
		target.inFlight = true
		target.flightDone = make(chan struct{})
		m.targets[issueID] = target
		due = append(due, target)
	}
	return due
}

func (m *heartbeatManager) execute(ctx context.Context, target heartbeatTarget) {
	if target.flightDone != nil {
		defer close(target.flightDone)
	}
	settings := m.settingsSnapshot()
	result := heartbeatResult{issueID: target.issueID, sequence: target.sequence}
	now := m.now().UTC()
	if deadline := target.progress.leaseExpiry(target.attemptDeadline); !deadline.IsZero() && !now.Before(deadline) {
		result.workAttemptError = store.ErrNotFound
		m.finish(target, result)
		return
	}
	if source, ok := settings.scheduling.(claimHeartbeatClock); ok {
		target.localLeaseDeadline = source.ClaimHeartbeatDeadline(target.issueID)
	}
	if settings.scheduling != nil {
		budget := settings.leaseTTL
		if !target.localLeaseDeadline.IsZero() {
			budget = target.localLeaseDeadline.Sub(now)
		}
		if budget <= 0 {
			budget = settings.interval
		}
		claimCtx, cancel := context.WithTimeout(ctx, budget)
		if source, ok := settings.scheduling.(claimHeartbeatSource); ok {
			result.claim, result.claimError = source.RenewClaimHeartbeat(claimCtx, target.issueID, now)
		} else {
			result.claim, result.claimError = settings.scheduling.RenewClaim(claimCtx, target.issueID, now)
		}
		cancel()
		result.claimOwnerLost = errors.Is(result.claimError, ErrSchedulingClaimLost)
		result.claimRenewed = result.claimError == nil
		if result.claimRenewed {
			result.claimIssue = result.claim.Issue
			result.currentClaimOwner = result.claim.Owner
		}
	} else if settings.claiming.Enabled && strings.TrimSpace(settings.claiming.LeaseField) != "" && strings.TrimSpace(target.claimOwner) != "" {
		budget := renewalBudget(now, target.localLeaseDeadline, settings.leaseTTL)
		if budget <= 0 {
			result.claimOwnerLost = true
		} else {
			claimCtx, cancel := context.WithTimeout(ctx, budget)
			result.claimIssue, result.currentClaimOwner, result.claimOwnerLost, result.claimError = renewTrackerClaim(claimCtx, settings, target, now, m.now)
			cancel()
			result.claimRenewed = result.claimError == nil && !result.claimOwnerLost
			if result.claimRenewed {
				result.claim.LeaseExpiresAt = now.Add(settings.leaseTTL)
			}
		}
	}
	if result.claimRenewed {
		if deadline := claimRenewalDeadline(result.claim); !deadline.IsZero() {
			target.localLeaseDeadline = deadline
			m.mu.Lock()
			if current, ok := m.targets[target.issueID]; ok && current.sequence == target.sequence {
				if current.claimLost {
					result.claimRenewed = false
					result.claimOwnerLost = true
				} else {
					current.localLeaseDeadline = deadline
					m.targets[target.issueID] = current
				}
			}
			m.mu.Unlock()
			m.notify()
		}
	}
	if result.claimOwnerLost || result.claimError != nil {
		m.finish(target, result)
		return
	}
	if target.progress != nil {
		running := target.progress.latest.Load()
		target.workerProcess = running.WorkerProcess
		target.workspacePath = running.WorkspacePath
	}
	result.workerAlive, result.workerChecked, result.livenessError = heartbeatWorkerLiveness(target.workerProcess)
	result.workspaceModifiedAt, result.workspaceAdvanced = heartbeatWorkspaceActivity(target.workspacePath, target.workspaceModifiedAt)
	result.heartbeat, result.workAttemptError = m.persistHeartbeat(ctx, target, settings)
	result.workAttemptRenewed = result.workAttemptError == nil && settings.workAttempts != nil && result.heartbeat.AttemptID > 0
	m.finish(target, result)
}

func (m *heartbeatManager) persistHeartbeat(ctx context.Context, target heartbeatTarget, settings heartbeatSettings) (store.WorkAttemptHeartbeat, error) {
	if target.progress != nil {
		target.progress.mu.Lock()
		defer target.progress.mu.Unlock()
		if target.progress.closeErr != nil {
			return target.workAttemptHeartbeat, target.progress.closeErr
		}
	}
	m.mu.Lock()
	if current, ok := m.targets[target.issueID]; ok && current.sequence == target.sequence {
		target.workAttemptHeartbeat = current.workAttemptHeartbeat
	}
	m.mu.Unlock()
	now := m.now().UTC()
	heartbeat := target.workAttemptHeartbeat
	heartbeat.HeartbeatAt = now
	heartbeat.LeaseExpiresAt = now.Add(settings.leaseTTL)
	var acknowledgedSource *Running
	if target.progress != nil && heartbeat.Phase != deferredCompletionPhase {
		running := target.progress.latest.Load()
		if running.CompletionSourceStart.Captured && !running.CompletionSourceStart.Persisted {
			snapshot := *running
			snapshot.CompletionSourceStart.Persisted = true
			acknowledgedSource = &snapshot
			running = acknowledgedSource
		}
		heartbeat = target.progress.heartbeat(heartbeat, now, running)
	}
	if settings.workAttempts != nil && heartbeat.AttemptID > 0 {
		budget := renewalBudget(now, target.progress.leaseExpiry(target.attemptDeadline), settings.leaseTTL)
		budget = renewalBudget(now, target.localLeaseDeadline, budget)
		if budget <= 0 {
			return heartbeat, store.ErrNotFound
		}
		writeCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if err := writeCtx.Err(); err != nil {
			return heartbeat, err
		}
		if err := settings.workAttempts.RecordWorkAttemptHeartbeat(writeCtx, heartbeat); err != nil {
			return heartbeat, err
		}
		if target.progress != nil {
			target.progress.persisted.Store(&heartbeat)
			if acknowledgedSource != nil {
				target.progress.latest.Store(acknowledgedSource)
			}
		}
	}
	return heartbeat, nil
}

func (m *heartbeatManager) settingsSnapshot() heartbeatSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

func (m *heartbeatManager) finish(target heartbeatTarget, result heartbeatResult) {
	m.mu.Lock()
	current, ok := m.targets[target.issueID]
	if !ok || current.sequence != target.sequence {
		m.mu.Unlock()
		return
	}
	if current.claimLost || result.claimOwnerLost || workAttemptHeartbeatRetired(result.workAttemptError) {
		current.claimLost = true
		result.claimOwnerLost = true
		result.claimRenewed = false
		if current.completionCancel != nil {
			current.completionCancel(ErrSchedulingClaimLost)
		}
	}
	if result.workAttemptRenewed {
		current.attemptDeadline = result.heartbeat.LeaseExpiresAt
	}
	now := m.now()
	delay := m.settings.interval
	retry := !result.claimOwnerLost && (result.claimError != nil || result.workAttemptError != nil &&
		!errors.Is(result.workAttemptError, context.Canceled))
	if retry {
		current.failures++
		delay = min(delay, schedulingBackoffInterval(time.Second, current.failures-1))
	}
	if !result.claimOwnerLost {
		if remaining := current.renewalDelay(now, delay); remaining > 0 {
			delay = remaining
		}
	}
	if retry {
		delay = runpkg.JitteredFailureRetryDelay(delay)
	} else {
		current.failures = 0
	}
	current.nextDue = now.Add(delay)
	current.inFlight = false
	if result.claimError == nil && !result.claimOwnerLost {
		current.workerAlive = result.workerAlive
		current.workerChecked = result.workerChecked
		current.livenessError = result.livenessError
	}
	if !result.workspaceModifiedAt.IsZero() {
		current.workspaceModifiedAt = result.workspaceModifiedAt
	}
	current.workspaceAdvanced = result.workspaceAdvanced
	m.targets[target.issueID] = current
	m.mu.Unlock()
	if result.workAttemptError != nil && !workAttemptHeartbeatRetired(result.workAttemptError) {
		m.logger.Warn("dedicated work attempt heartbeat failed", "attempt_id", result.heartbeat.AttemptID, "issue_id", result.issueID, "error", result.workAttemptError)
	}
	if result.claimError != nil {
		m.logger.Warn("dedicated claim heartbeat failed", "issue_id", result.issueID, "error", result.claimError)
	}
	if result.livenessError != nil {
		m.logger.Warn("worker liveness inspection failed", "issue_id", result.issueID, "error", result.livenessError)
	}
	select {
	case m.results <- result:
	default:
	}
	m.notify()
}

func workAttemptHeartbeatRetired(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrWorkAttemptTerminal)
}

func (m *heartbeatManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func resetHeartbeatTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func heartbeatWorkerLiveness(identity procgroup.Identity) (bool, bool, error) {
	if identity.PID <= 0 || identity.StartedAt.IsZero() {
		return false, false, nil
	}
	alive, err := procgroup.Alive(identity)
	return alive, true, err
}

func heartbeatWorkspaceActivity(path string, previous time.Time) (time.Time, bool) {
	modifiedAt, err := latestHeartbeatWorkspaceModification(path)
	if err != nil {
		return time.Time{}, false
	}
	return modifiedAt, !previous.IsZero() && modifiedAt.After(previous)
}

func latestHeartbeatWorkspaceModification(path string) (time.Time, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return time.Time{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	latest := info.ModTime().UTC()
	for _, candidate := range []string{filepath.Join(path, ".detent"), filepath.Join(path, ".detent", "tmp")} {
		info, err := os.Stat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if modifiedAt := info.ModTime().UTC(); modifiedAt.After(latest) {
			latest = modifiedAt
		}
	}
	return latest, nil
}

func renewTrackerClaim(ctx context.Context, settings heartbeatSettings, target heartbeatTarget, now time.Time, clock func() time.Time) (connector.Issue, string, bool, error) {
	renewer, ok := settings.connector.(trackerClaimLeaseRenewer)
	if !ok {
		return connector.Issue{}, "", true, nil
	}
	issues, err := settings.connector.FetchIssueStatesByIDs(ctx, []string{target.issueID})
	if err != nil {
		return connector.Issue{}, "", false, err
	}
	for _, issue := range issues {
		if issue.ID != target.issueID {
			continue
		}
		owner := heartbeatClaimOwner(issue, settings.claiming)
		if !sameClaimOwner(owner, target.claimOwner) {
			return cloneIssue(issue), owner, true, nil
		}
		lease, ok := parseClaimTime(issue.Fields[settings.claiming.LeaseField])
		expiresAt := lease.Add(settings.leaseTTL)
		remaining := expiresAt.Sub(clock())
		if !ok || remaining <= 0 || !target.localLeaseDeadline.IsZero() && !target.localLeaseDeadline.Equal(expiresAt) {
			return cloneIssue(issue), owner, true, nil
		}
		ownerField := ""
		if settings.claiming.OwnershipMode == workflowconfig.IdentityOwnershipField {
			ownerField = settings.claiming.OwnerField
		}
		writeCtx, cancel := context.WithTimeout(ctx, remaining)
		defer cancel()
		renewed, err := renewer.CompareAndRenewClaimLease(writeCtx, issue, ownerField, settings.claiming.LeaseField, now, expiresAt)
		if err != nil || !renewed {
			return cloneIssue(issue), owner, err == nil, err
		}
		return issueWithLeaseField(issue, settings.claiming.LeaseField, now), owner, false, nil
	}
	return connector.Issue{}, "", false, fmt.Errorf("issue %s was not returned during claim heartbeat", target.issueID)
}

func heartbeatClaimOwner(issue connector.Issue, claiming ClaimingConfig) string {
	if claiming.OwnershipMode == workflowconfig.IdentityOwnershipField {
		if issue.Fields == nil {
			return ""
		}
		return strings.TrimSpace(issue.Fields[claiming.OwnerField])
	}
	owners := append([]string{issue.AssigneeID}, issue.Assignees...)
	normalized := make([]string, 0, len(owners))
	seen := make(map[string]struct{}, len(owners))
	for _, owner := range owners {
		owner = strings.TrimSpace(owner)
		key := normalizeClaimOwner(owner)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, owner)
	}
	sortClaimOwners(normalized)
	if len(normalized) == 0 {
		return ""
	}
	return normalized[0]
}

func (o *Orchestrator) persistedWorkerLive(ctx context.Context, issue connector.Issue) bool {
	processStore, ok := o.workAttempts.(interface {
		ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error)
	})
	if !ok {
		return false
	}
	processes, err := processStore.ListActiveWorkerProcesses(ctx)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("worker liveness registry lookup failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return true
	}
	for _, process := range processes {
		if !workerProcessMatchesIssue(process, issue) {
			continue
		}
		alive, err := procgroup.Alive(procgroup.Identity{PID: process.PID, GroupID: process.GroupID, StartedAt: process.StartedAt})
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("persisted worker liveness inspection failed", "issue_id", issue.ID, "identifier", issue.Identifier, "pid", process.PID, "error", err)
			}
			return true
		}
		if alive {
			return true
		}
	}
	return false
}

func workerProcessMatchesIssue(process store.WorkerProcess, issue connector.Issue) bool {
	if issue.ID != "" && process.IssueID == issue.ID {
		return true
	}
	if issue.Identifier != "" && strings.EqualFold(process.Identifier, issue.Identifier) {
		return true
	}
	return issue.URL != "" && strings.EqualFold(process.IssueURL, issue.URL)
}

func (o *Orchestrator) trackRunningHeartbeat(state *State, running Running, claimed Claimed, now time.Time) {
	if o == nil || o.heartbeats == nil || strings.TrimSpace(running.Issue.ID) == "" {
		return
	}
	running = running.withProgress()
	owner := strings.TrimSpace(claimed.Owner)
	if owner == "" {
		owner = o.claimOwner()
	}
	o.heartbeats.upsert(heartbeatTarget{
		progress:             running.progress,
		issueID:              running.Issue.ID,
		claimOwner:           owner,
		localLeaseDeadline:   claimRenewalDeadline(claimed),
		workAttemptHeartbeat: o.runningWorkAttemptHeartbeat(state, running, now),
		workerProcess:        running.WorkerProcess,
		workspacePath:        running.WorkspacePath,
	})
}

func (o *Orchestrator) handleHeartbeatResult(state *State, result heartbeatResult) {
	if o == nil || state == nil || o.heartbeats == nil || !o.heartbeats.current(result.issueID, result.sequence) {
		return
	}
	running, active := state.Running[result.issueID]
	deferred, waiting := state.deferredCompletions[result.issueID]
	if waiting {
		running = deferred.Running
	}
	if !active && !waiting {
		o.heartbeats.remove(result.issueID)
		return
	}
	if result.claimOwnerLost {
		if o.logger != nil {
			o.logger.Warn("claim heartbeat owner changed", "issue_id", result.issueID, "owner", o.claimOwner(), "current_owner", result.currentClaimOwner)
		}
		if waiting {
			o.heartbeats.remove(result.issueID)
			if source, ok := o.scheduling.(interface{ ForgetClaim(string) }); ok {
				source.ForgetClaim(result.issueID)
			}
			return
		}
		o.releaseClaim(state, result.issueID)
		return
	}
	if result.workAttemptRenewed && result.heartbeat.AttemptID == running.WorkAttemptID {
		o.applyWorkAttemptHeartbeatSnapshot(state, running.WorkAttemptID, result.heartbeat, running.LastMessageTruncation)
	}
	if !result.claimRenewed {
		return
	}
	claimed, ok := state.Claimed[result.issueID]
	if !ok {
		return
	}
	if result.claim.LeaseRenewedAt.IsZero() {
		claimed.Owner = result.currentClaimOwner
		claimed.LeaseRenewedAt, _ = parseClaimTime(result.claimIssue.Fields[o.cfg.Claiming.LeaseField])
		claimed.LeaseExpiresAt = result.claim.LeaseExpiresAt
		claimed.Issue = mergeIssueTrackerFields(claimed.Issue, result.claimIssue)
	} else {
		renewed := result.claim
		renewed.Issue = mergeIssueTrackerFields(claimed.Issue, renewed.Issue)
		claimed = renewed
	}
	state.Claimed[result.issueID] = claimed
	running.Issue = mergeIssueTrackerFields(running.Issue, result.claimIssue)
	if waiting {
		deferred.Running = running
		state.deferredCompletions[result.issueID] = deferred
	} else {
		state.Running[result.issueID] = running
	}
}
