package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	FinalStateSessionDurationExceeded = "session_duration_exceeded"
	FinalStateTurnLimitExceeded       = "turn_limit_exceeded"
	FinalStateNoProgress              = "no_progress"

	SessionBrakeReasonDuration  = "session_duration_exceeded"
	SessionBrakeReasonTurnLimit = "session_turn_limit_exceeded"
	SessionBrakeReasonMemory    = FinalStateMemoryCeilingExceeded
)

var (
	ErrSessionTurnLimitExceeded = errors.New("agent session turn limit exceeded")
)

type SessionBrakeError struct {
	Reason            string
	CauseFingerprint  string
	Limit             time.Duration
	MaxTurns          int
	Elapsed           time.Duration
	Turns             int
	Tokens            int64
	RSSBytes          uint64
	RSSCeilingBytes   uint64
	LastProgressAt    time.Time
	WorkspaceProgress string
	WorkpadProgress   string
	FilesChanged      int
	UnpushedCommits   int
	Resumable         bool
	Checkpoint        *workspace.CheckpointRecord
	cause             error
}

func (e *SessionBrakeError) Error() string {
	if e == nil {
		return "agent session brake exceeded"
	}
	cause := ""
	if e.cause != nil {
		cause = ": " + e.cause.Error()
	}
	return fmt.Sprintf(
		"%s%s; elapsed=%s turns=%d tokens=%d cause_fingerprint=%s",
		e.Reason,
		cause,
		e.Elapsed,
		e.Turns,
		e.Tokens,
		e.CauseFingerprint,
	)
}

func (e *SessionBrakeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *SessionBrakeError) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrSessionDurationExceeded:
		return e.Reason == SessionBrakeReasonDuration
	case ErrSessionTurnLimitExceeded:
		return e.Reason == SessionBrakeReasonTurnLimit
	case ErrSessionMemoryCeilingExceeded:
		return e.Reason == SessionBrakeReasonMemory
	default:
		return errors.Is(e.cause, target)
	}
}

type sessionProgressSnapshot struct {
	LocalProgress        *workspace.LocalProgress
	DiffStats            DiffStats
	HeadSHA              string
	WorkspaceFingerprint string
	WorkpadFingerprint   string
	UnpushedCommits      int
}

func (s sessionProgressSnapshot) fingerprint() string {
	return strings.Join([]string{
		strings.TrimSpace(s.WorkspaceFingerprint),
		strings.TrimSpace(s.WorkpadFingerprint),
		strconv.Itoa(s.DiffStats.FilesChanged),
		strconv.Itoa(s.DiffStats.AddedLines),
		strconv.Itoa(s.DiffStats.RemovedLines),
		strconv.Itoa(s.UnpushedCommits),
	}, "\x00")
}

func (s sessionProgressSnapshot) resumable() bool {
	return s.UnpushedCommits > 0 ||
		s.DiffStats.FilesChanged > 0 ||
		s.DiffStats.AddedLines > 0 ||
		s.DiffStats.RemovedLines > 0
}

type sessionBrakeController struct {
	mu                  sync.Mutex
	startedAt           time.Time
	lastProgressAt      time.Time
	maxTurns            int
	turns               int
	tokens              int64
	initial             sessionProgressSnapshot
	current             sessionProgressSnapshot
	workProductProgress bool
	breach              *SessionBrakeError
	cancelSession       context.CancelCauseFunc
	probe               func(context.Context) (sessionProgressSnapshot, error)
	now                 func() time.Time
	logger              *slog.Logger
	issue               connector.Issue
	journal             *sessionProgressJournal
	observation         *store.SessionProgress
}

func newSessionBrakeController(
	ctx context.Context,
	startedAt time.Time,
	sessionDuration time.Duration,
	maxTurns int,
	cancelSession context.CancelCauseFunc,
	probe func(context.Context) (sessionProgressSnapshot, error),
	now func() time.Time,
	logger *slog.Logger,
	issue connector.Issue,
	journals ...*sessionProgressJournal,
) *sessionBrakeController {
	if sessionDuration <= 0 && maxTurns <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	if startedAt.IsZero() {
		startedAt = now()
	}
	controller := &sessionBrakeController{
		startedAt:      startedAt,
		lastProgressAt: startedAt,
		maxTurns:       maxTurns,
		cancelSession:  cancelSession,
		probe:          probe,
		now:            now,
		logger:         logger,
		issue:          issue,
	}
	if len(journals) > 0 {
		controller.journal = journals[0]
	}
	if probe == nil {
		return controller
	}
	snapshot, err := probe(ctx)
	if err != nil {
		controller.logProbeFailure(err)
	} else {
		controller.initial = snapshot
		controller.current = snapshot
		if err := controller.observeSnapshotLocked(ctx, snapshot, startedAt); err != nil {
			controller.logProbeFailure(err)
		}
	}
	return controller
}

func (c *sessionBrakeController) observe(ctx context.Context, turns int, tokens int64) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if turns > c.turns {
		c.turns = turns
	}
	if tokens > c.tokens {
		c.tokens = tokens
	}
	if c.breach != nil {
		err := c.breach
		c.mu.Unlock()
		return err
	}
	if c.maxTurns <= 0 || c.turns <= c.maxTurns {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	c.refreshSnapshot(ctx)
	at := c.now().UTC()
	c.mu.Lock()
	if c.breach == nil {
		c.breach = c.newErrorLocked(SessionBrakeReasonTurnLimit, ErrSessionTurnLimitExceeded, 0, at)
	}
	err := c.breach
	c.mu.Unlock()
	c.cancelSession(err)
	return err
}

func (c *sessionBrakeController) refreshSnapshot(ctx context.Context) {
	if c == nil || c.probe == nil {
		return
	}
	snapshot, err := c.probe(ctx)
	if err != nil {
		c.logProbeFailure(err)
		return
	}
	c.mu.Lock()
	if err := c.observeSnapshotLocked(ctx, snapshot, c.now().UTC()); err != nil {
		c.logProbeFailure(err)
	}
	c.current = snapshot
	c.mu.Unlock()
}

func (c *sessionBrakeController) wrapDuration(ctx context.Context, err error, limit time.Duration) error {
	if c == nil || !errors.Is(err, ErrSessionDurationExceeded) {
		return err
	}
	var existing *SessionBrakeError
	if errors.As(err, &existing) {
		return err
	}
	c.refreshSnapshot(ctx)
	at := c.now().UTC()
	c.mu.Lock()
	if c.breach == nil {
		c.breach = c.newErrorLocked(SessionBrakeReasonDuration, err, limit, at)
	}
	breach := c.breach
	c.mu.Unlock()
	return breach
}

func (c *sessionBrakeController) wrapTurnLimit(ctx context.Context, err error) error {
	if c == nil || !errors.Is(err, ErrSessionTurnLimitExceeded) {
		return err
	}
	var existing *SessionBrakeError
	if errors.As(err, &existing) {
		return err
	}
	c.refreshSnapshot(ctx)
	at := c.now().UTC()
	c.mu.Lock()
	if c.turns < c.maxTurns {
		c.turns = c.maxTurns
	}
	if c.breach == nil {
		c.breach = c.newErrorLocked(SessionBrakeReasonTurnLimit, err, 0, at)
	}
	breach := c.breach
	c.mu.Unlock()
	return breach
}

func (c *sessionBrakeController) newErrorLocked(reason string, cause error, limit time.Duration, at time.Time) *SessionBrakeError {
	elapsed := at.Sub(c.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	resumable := c.current.resumable() ||
		c.workProductProgress ||
		(c.current.WorkpadFingerprint != "" && c.current.WorkpadFingerprint != c.initial.WorkpadFingerprint)
	brake := &SessionBrakeError{
		Reason:            reason,
		Limit:             limit,
		MaxTurns:          c.maxTurns,
		Elapsed:           elapsed,
		Turns:             c.turns,
		Tokens:            c.tokens,
		LastProgressAt:    c.lastProgressAt,
		WorkspaceProgress: strings.TrimSpace(c.current.WorkspaceFingerprint),
		WorkpadProgress:   strings.TrimSpace(c.current.WorkpadFingerprint),
		FilesChanged:      c.current.DiffStats.FilesChanged,
		UnpushedCommits:   c.current.UnpushedCommits,
		Resumable:         resumable,
		cause:             cause,
	}
	brake.CauseFingerprint = sessionBrakeFingerprint(brake)
	return brake
}

func (c *sessionBrakeController) memoryCeiling(err *SessionMemoryCeilingError, at time.Time) *SessionBrakeError {
	if c == nil || err == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	brake := c.newErrorLocked(SessionBrakeReasonMemory, err, 0, at)
	brake.RSSBytes = err.RSSBytes
	brake.RSSCeilingBytes = err.CeilingBytes
	brake.CauseFingerprint = sessionBrakeFingerprint(brake)
	return brake
}

func (c *sessionBrakeController) resultDiffStats() DiffStats {
	if c == nil {
		return DiffStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := c.current.DiffStats
	result.UnpushedCommits = c.current.UnpushedCommits
	result.HeadSHA = strings.TrimSpace(c.current.HeadSHA)
	return result
}

func (c *sessionBrakeController) logProbeFailure(err error) {
	if c == nil || c.logger == nil || err == nil {
		return
	}
	c.logger.Warn(
		"session progress heartbeat failed",
		"issue_id", c.issue.ID,
		"identifier", c.issue.Identifier,
		"error", err,
	)
}

func sessionBrakeFingerprint(brake *SessionBrakeError) string {
	if brake == nil {
		return ""
	}
	value := strings.Join([]string{
		strings.TrimSpace(brake.Reason),
		brake.Limit.String(),
		strconv.Itoa(brake.MaxTurns),
		strconv.FormatUint(brake.RSSCeilingBytes, 10),
		strings.TrimSpace(brake.WorkspaceProgress),
		strings.TrimSpace(brake.WorkpadProgress),
		strconv.Itoa(brake.FilesChanged),
		strconv.Itoa(brake.UnpushedCommits),
		strconv.FormatBool(brake.Resumable),
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (r *Runner) sessionProgressSnapshot(
	ctx context.Context,
	backend workspace.Backend,
	info workspace.Info,
	issue workspace.Issue,
	external SessionProgressProbe,
) (sessionProgressSnapshot, error) {
	snapshot := sessionProgressSnapshot{}
	var workspaceErr error
	if provider, ok := backend.(workspace.RecoveryStateProvider); ok {
		recovery, err := provider.RecoveryState(ctx, info, issue)
		if err != nil {
			workspaceErr = err
		} else {
			snapshot.DiffStats = diffStatsFromWorkspace(recovery.DiffStat)
			applyRecoveryState(&snapshot.DiffStats, &recovery)
			snapshot.HeadSHA = strings.TrimSpace(recovery.HeadSHA)
			snapshot.WorkspaceFingerprint = strings.TrimSpace(recovery.WorkspaceFingerprint)
			snapshot.UnpushedCommits = recovery.UnpushedCommits
		}
	} else {
		stat, err := backend.DiffStat(ctx, info, issue)
		if err != nil {
			workspaceErr = err
		} else {
			snapshot.DiffStats = diffStatsFromWorkspace(stat)
			snapshot.WorkspaceFingerprint = strings.TrimSpace(stat.Fingerprint)
		}
	}
	var externalErr error
	if provider, ok := backend.(workspace.LocalProgressProvider); ok {
		local, err := provider.LocalProgress(ctx, info, issue)
		if err != nil {
			workspaceErr = errors.Join(workspaceErr, err)
		} else {
			snapshot.LocalProgress = &local
		}
	}
	if external != nil {
		snapshot.WorkpadFingerprint, externalErr = external(ctx)
		snapshot.WorkpadFingerprint = strings.TrimSpace(snapshot.WorkpadFingerprint)
	}
	if workspaceErr != nil || externalErr != nil {
		return snapshot, errors.Join(workspaceErr, externalErr)
	}
	return snapshot, nil
}
