package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeExecution struct {
	completionOwner   atomic.Bool
	stepReviews       *tracker.ChangeDetail
	stepDiscussion    []tracker.NativeComment
	workerMu          sync.Mutex
	workerCloseMu     sync.Mutex
	cancelMu          sync.Mutex
	workers           []func(context.Context) error
	workerLauncher    func(context.Context, runner.RunRequest) (runner.RunResult, error)
	reasonGenerator   runner.DependencyReasonGenerator
	diagnostic        runnerauth.DiagnosticAttempt
	diagnosticMu      sync.Mutex
	reconnecting      bool
	validation        *gate.CommandResult
	usageStartedAt    time.Time
	artifacts         nativeArtifacts
	scheduler         *Scheduler
	claim             nativeClaim
	mu                sync.Mutex
	data              tracker.NativeRunData
	evidenceSource    func(context.Context, string) (runner.ValidationEvidence, error)
	integrationSource func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)
	publicationSource func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
	publication       *tracker.NativePRPublication
	pending           *tracker.NativeRunEvent
	observations      []tracker.NativeRunEvent
	cancel            context.CancelCauseFunc
	// diffSource computes the stored attempt diff the execution posts before
	// every checkpoint and before the finish. It is
	// nil for a run with no worktree to describe.
	diffSource runner.AttemptDiffSource
	// lastDiff is the most recent diff the source produced. It stands in for
	// the finish generation when the worktree is gone by then: the agent has
	// stopped, so nothing changed after the last diff that could be read.
	lastDiff *tracker.AttemptDiffRequest
	// storedSeq is the run event sequence whose diff the hub last stored, so
	// the finish can tell its final diff was received.
	storedSeq int64
	// worktreeState is the last checkpoint's worktree state. Only a clean or
	// unpushed worktree is settled; dirty work takes the ordinary path.
	worktreeState            string
	worktreeHead             string
	role                     string
	conversationContinuation bool
	settled                  bool
	landed                   bool
	change                   *runner.NativeChange
	finalizationSource       *tracker.NativeChangeReference
	finalizationAttempt      string
	preparedOutcome          string
	preparedMessage          string
	preparedDisposition      *tracker.NativeDisposition
	preparedFailure          *tracker.NativeTerminalFailure
	runtimeDirty             bool
	runtimeSupported         *bool
	runtimePublishedAt       time.Time
	// repository is the https URL of the checkout's origin, which a published
	// version names; empty when the remote cannot be named that way.
	repository      string
	changeSource    func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
	retainedSource  *tracker.ChangeSourceCapture
	sourceRequired  bool
	recoveredSource *tracker.NativeChangeReference
	mergeResolution bool
	releaseDelivery *tracker.ReleaseDeliveryStatus
}

func (e *nativeExecution) RetainCompletion() {
	e.completionOwner.Store(true)
}

func (e *nativeExecution) KeepWorker(closeWorker func(context.Context) error, launch func(context.Context, runner.RunRequest) (runner.RunResult, error)) error {
	if closeWorker == nil {
		return errors.New("execution worker cleanup is required")
	}
	e.workerMu.Lock()
	defer e.workerMu.Unlock()
	e.workers = append(e.workers, closeWorker)
	if e.workerLauncher == nil {
		e.workerLauncher = launch
	}
	return nil
}

func (e *nativeExecution) WorkerLauncher() func(context.Context, runner.RunRequest) (runner.RunResult, error) {
	e.workerMu.Lock()
	defer e.workerMu.Unlock()
	return e.workerLauncher
}

func (e *nativeExecution) hasWorkers() bool {
	e.workerMu.Lock()
	defer e.workerMu.Unlock()
	return len(e.workers) > 0
}

func (e *nativeExecution) CloseWorkers(ctx context.Context) error {
	defer e.scheduler.cleanupNativeClaim(context.WithoutCancel(ctx), string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken)
	e.workerCloseMu.Lock()
	defer e.workerCloseMu.Unlock()
	e.workerMu.Lock()
	workers := append([]func(context.Context) error(nil), e.workers...)
	e.workerMu.Unlock()
	var pending []func(context.Context) error
	var failures []error
	for _, closeWorker := range workers {
		if err := closeWorker(ctx); err != nil {
			pending = append(pending, closeWorker)
			failures = append(failures, err)
		}
	}
	e.workerMu.Lock()
	e.workers = append(pending, e.workers[len(workers):]...)
	if len(e.workers) == 0 {
		e.workerLauncher = nil
	}
	e.workerMu.Unlock()
	return errors.Join(failures...)
}

type nativeMutationAuthorityKey struct{}

type nativeMutationAuthority struct {
	scope        string
	lease        tracker.NativeLease
	cancellation func() error
}

func nativeEffectError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority)
	if !ok || authority.cancellation == nil {
		return nil
	}
	cause := authority.cancellation()
	if errors.Is(cause, runner.ErrOperatorStopped) {
		return cause
	}
	return nil
}

func executionID(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(digest[:16])
}

func (c *NativeClient) fencedMutation(ctx context.Context, id tracker.NativeWorkItemID, mutation tracker.Mutation) tracker.Mutation {
	if mutation.LeaseID != "" {
		return mutation
	}
	if authority, ok := ctx.Value(nativeMutationAuthorityKey{}).(nativeMutationAuthority); ok && authority.scope == c.base() && authority.lease.WorkItemID == id {
		mutation.LeaseID, mutation.FencingToken = authority.lease.ID, authority.lease.FencingToken
		return mutation
	}
	if value, ok := c.client.nativeLeases.Load(c.base() + "/" + string(id)); ok {
		if lease, ok := value.(tracker.NativeLease); ok {
			mutation.LeaseID, mutation.FencingToken = lease.ID, lease.FencingToken
		}
	}
	return mutation
}

func (s *Scheduler) RunExecution(issueID string) runner.Execution {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.nativeClaims[issueID]
	if !ok {
		if strings.HasPrefix(issueID, "wi_") {
			return &nativeExecution{scheduler: s, claim: nativeClaim{lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(issueID)}}}
		}
		return nil
	}
	if claim.execution != nil {
		return claim.execution
	}
	execution := &nativeExecution{scheduler: s, claim: claim, data: tracker.NativeRunData{
		RunID: executionID("run", string(claim.lease.WorkItemID)), AttemptID: executionID("attempt", string(claim.lease.ID)),
		PolicyID: claim.lease.PolicyID, LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken,
	}}
	for _, attempt := range claim.recovery.Attempts {
		if attempt.Identity != nil && attempt.LeaseID == claim.lease.ID && attempt.FencingToken == claim.lease.FencingToken && (attempt.Status == "running" || attempt.Status == "interrupted") {
			execution.data = attempt.NativeRunData
			if attempt.Runtime != nil {
				execution.runtimePublishedAt = attempt.Runtime.HeartbeatAt
			}
			execution.usageStartedAt = attempt.StartedAt
			execution.role = attempt.Identity.Role
			execution.reconnecting = true
			if attempt.Checkpoint != nil {
				execution.worktreeState = attempt.Checkpoint.WorktreeState
				execution.worktreeHead = attempt.Checkpoint.HeadSHA
			}
			break
		}
	}
	claim.execution = execution
	s.nativeClaims[issueID] = claim
	return execution
}

func (e *nativeExecution) Recovery() tracker.NativeRecovery {
	recovery := e.claim.recovery
	recovery.Lease = e.claim.lease
	return recovery
}

func (e *nativeExecution) remaining() time.Duration {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	claim, ok := e.scheduler.nativeClaims[string(e.claim.lease.WorkItemID)]
	if !ok || claim.lease.FencingToken != e.claim.lease.FencingToken {
		return 0
	}
	margin := min(5*time.Second, e.scheduler.leaseTTL/10)
	return claim.deadline.Sub(e.scheduler.now()) - margin
}

func (e *nativeExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	if err := e.Validate(ctx); err != nil {
		return ctx, func() {}, err
	}
	bound := context.WithValue(ctx, nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: e.claim.source.client.base(), lease: e.claim.lease, cancellation: func() error { return context.Cause(ctx) }})
	guarded, cancel := context.WithCancelCause(bound)
	e.cancelMu.Lock()
	e.cancel = cancel
	e.cancelMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var routingChanged <-chan struct{}
		backoff := time.Second
		for {
			if source := e.scheduler.client.runner; source != nil {
				availability, changed, err := source.availabilityState()
				if err != nil {
					cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, err))
					return
				}
				if changed != routingChanged {
					deadline, err := availability.Deadline(e.scheduler.now())
					if err != nil {
						cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, err))
						return
					}
					e.scheduler.mu.Lock()
					if claim, ok := e.scheduler.nativeClaims[string(e.claim.lease.WorkItemID)]; ok && claim.lease.FencingToken == e.claim.lease.FencingToken {
						claim.availabilityDeadline = deadline
						e.scheduler.nativeClaims[string(e.claim.lease.WorkItemID)] = claim
					}
					e.scheduler.mu.Unlock()
					routingChanged = changed
				}
			}
			remaining := e.remaining()
			if remaining <= 0 {
				if err := e.renew(guarded, 0); err != nil {
					if !nativeTransportUnavailable(err) {
						cancel(errors.Join(runner.ErrExecutionAuthorityUnavailable, err))
						return
					}
					remaining = backoff
					backoff = min(30*time.Second, backoff*2)
				} else {
					remaining = e.remaining()
					backoff = time.Second
				}
			}
			if deadline := e.AvailabilityDeadline(); !deadline.IsZero() {
				untilDeadline := deadline.Sub(e.scheduler.now())
				if untilDeadline <= 0 {
					cancel(runner.NewCancellationCause(context.Canceled, "runner.availability"))
					return
				}
				remaining = min(remaining, untilDeadline)
			}
			timer := time.NewTimer(remaining)
			select {
			case <-guarded.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-routingChanged:
				timer.Stop()
			}
		}
	}()
	return guarded, func() {
		cancel(context.Canceled)
		<-done
		e.cancelMu.Lock()
		e.cancel = nil
		e.cancelMu.Unlock()
		e.scheduler.cleanupNativeClaim(context.WithoutCancel(ctx), string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken)
	}, nil
}

func (e *nativeExecution) unavailable(err error) error {
	cause := errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	e.cancelExecution(cause)
	return cause
}

func (e *nativeExecution) cancelExecution(cause error) {
	e.cancelMu.Lock()
	cancel := e.cancel
	e.cancelMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

func (e *nativeExecution) executionError(ctx context.Context, err error) error {
	if nativeAuthorityLost(err) {
		err = e.scheduler.nativeClaimError(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	}
	if errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	return err
}

func (e *nativeExecution) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return e.validationError(ctx, errors.Join(err, context.Cause(ctx)))
	}
	if e.remaining() <= 0 {
		if err := e.renew(ctx, 0); err != nil {
			return e.validationError(ctx, err)
		}
	}
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.PolicyID); err != nil {
		return e.validationError(ctx, err)
	}
	return nil
}

func (e *nativeExecution) renew(ctx context.Context, landingCapacity int) error {
	e.scheduler.mu.Lock()
	claim, ok := e.scheduler.nativeClaims[string(e.claim.lease.WorkItemID)]
	e.scheduler.mu.Unlock()
	if !ok || claim.lease.FencingToken != e.claim.lease.FencingToken {
		return runner.ErrExecutionAuthorityUnavailable
	}
	_, err := e.scheduler.renewNativeClaim(ctx, string(e.claim.lease.WorkItemID), claim, landingCapacity)
	return err
}

func (e *nativeExecution) validationError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	err = e.scheduler.nativeClaimError(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	err = e.executionError(ctx, err)
	if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		return e.unavailable(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !nativeTransportUnavailable(err) {
		return e.unavailable(err)
	}
	slog.Default().Warn("native validation unavailable", "work_item", e.claim.lease.WorkItemID, "error", err)
	return nil
}

func (e *nativeExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) (startErr error) {
	defer func() {
		if startErr == nil {
			e.refreshReasons(ctx, false)
		}
	}()
	e.mu.Lock()
	if e.data.Identity != nil && e.data.Sequence > 0 {
		if e.reconnecting {
			e.mu.Unlock()
			if err := e.Validate(ctx); err != nil {
				return err
			}
			e.mu.Lock()
		}
		defer e.mu.Unlock()
		if *e.data.Identity != identity {
			return errors.New("native execution identity changed during an attempt")
		}
		if e.reconnecting {
			e.data.Outcome = ""
			observation := &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: e.scheduler.now().UTC()}
			if e.data.Runtime != nil && e.data.Runtime.Landing != nil && e.data.Runtime.Landing.Waiting {
				observation.Landing = e.data.Runtime.Landing
			}
			e.data.Runtime = observation
			e.data.Finalization = nil
			e.data.TerminalFailure = nil
			e.data.Disposition = nil
			e.data.CompletionBody = ""
			if err := e.append(ctx, "run.observed", "", nil); err != nil {
				return err
			}
			e.reconnecting = false
		}
		return e.flush(ctx)
	}
	e.mu.Unlock()
	if err := e.Validate(ctx); err != nil {
		return err
	}
	if err := e.validateProviderStart(identity); err != nil {
		return err
	}
	issue, err := e.claim.source.client.Issue(ctx, e.claim.lease.WorkItemID)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.data.Identity != nil {
		if *e.data.Identity != identity {
			return errors.New("native execution identity changed during an attempt")
		}
		return e.flush(ctx)
	}
	e.data.Identity = &identity
	e.role = identity.Role
	e.claim.recovery.Issue.State = issue.State
	return e.append(ctx, "run.started", "", nil)
}

func (e *nativeExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	checkpoint.Publication = nil
	if e.data.Identity == nil {
		return nil
	}
	if previous := e.data.Handoff; previous.UncertainForgeEffect() && checkpoint.EffectState == "none" {
		checkpoint = *previous
	}
	e.worktreeState = checkpoint.WorktreeState
	e.worktreeHead = checkpoint.HeadSHA
	if err := e.flush(ctx); err != nil {
		if nativeTransportUnavailable(err) {
			if e.diffSource != nil {
				if captureErr := e.captureDiff(ctx); captureErr != nil {
					return captureErr
				}
			}
			slog.Default().Warn("native checkpoint publication unavailable", "work_item", e.claim.lease.WorkItemID, "error", err)
			return nil
		}
		return err
	}
	previous, err := json.Marshal(e.data.Handoff)
	if err != nil {
		return err
	}
	current, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if string(previous) == string(current) {
		if e.diffSource != nil {
			return e.captureDiff(ctx)
		}
		return nil
	}
	if err := e.append(ctx, "run.checkpointed", "", &checkpoint); err != nil {
		if nativeTransportUnavailable(err) {
			slog.Default().Warn("native checkpoint publication unavailable", "work_item", e.claim.lease.WorkItemID, "error", err)
			return nil
		}
		return err
	}
	return nil
}

func (e *nativeExecution) PrepareFinish(ctx context.Context, outcome, finalMessage string, failure *tracker.NativeTerminalFailure) (err error) {
	e.mu.Lock()
	defer func() {
		e.mu.Unlock()
		if err == nil {
			e.refreshReasons(ctx, false)
		}
	}()
	done := e.diagnosticOperation("native.prepare_finish")
	defer func() { done(err) }()
	if failure != nil {
		public := failure.Public()
		e.preparedFailure = &public
	}
	e.preparedOutcome = outcome
	e.preparedMessage = finalMessage
	if len(e.preparedMessage) > 64<<10 {
		e.preparedMessage = e.preparedMessage[:64<<10]
		for !utf8.ValidString(e.preparedMessage) {
			e.preparedMessage = e.preparedMessage[:len(e.preparedMessage)-1]
		}
	}
	e.preparedDisposition = nil
	planning := e.role == runner.RolePlan && !e.conversationContinuation
	if e.ownsIssueCompletion() || planning {
		e.preparedDisposition = runner.NativeDispositionFromMessage(finalMessage)
	}
	if e.role == runner.RoleMerge {
		if disposition := runner.NativeDispositionFromMessage(finalMessage); disposition != nil && disposition.Status == "blocked" && disposition.HumanAction {
			e.preparedDisposition = disposition
		}
	}
	if outcome == "succeeded" && planning && e.preparedDisposition == nil {
		e.preparedDisposition = &tracker.NativeDisposition{Status: "in_progress"}
	}
	if outcome == "succeeded" && e.data.Runtime != nil && e.data.Runtime.Landing != nil && e.data.Runtime.Landing.Waiting {
		e.preparedDisposition = &tracker.NativeDisposition{Status: "in_progress"}
	}
	if e.change != nil {
		e.applySecurityAuditHumanAction(e.change.SecurityAudit)
	}
	if err := e.prepareFinish(ctx, outcome); err != nil {
		err = e.executionError(ctx, err)
		err = errors.Join(err, nativeEffectError(ctx))
		if outcome == "succeeded" && e.ownsChangeCompletion() && nativeTransportUnavailable(err) && nativeEffectError(ctx) == nil {
			if e.change == nil {
				e.change = &runner.NativeChange{}
			}
			e.change.Error = err.Error()
			return nil
		}
		e.preparedOutcome = "failed"
		if errors.Is(err, runner.ErrOperatorStopped) {
			e.preparedOutcome = "interrupted"
		}
		if e.preparedFailure == nil && e.data.Identity != nil {
			e.preparedFailure = e.finalizationFailure(err)
		}
		return err
	}
	return nil
}

func (e *nativeExecution) prepareFinish(ctx context.Context, outcome string) error {
	if err := e.flush(ctx); err != nil {
		return err
	}
	if e.data.Identity == nil {
		return nil
	}
	if e.data.Outcome != "" {
		return nil
	}
	// A succeeded run is settled before run.finished is published, while the
	// lease still fences it: the finish diff is stored and the Change Request
	// opened first, so a runner that dies in between leaves the attempt
	// running and the lease's expiry re-offers the item. Opening the change
	// again is safe, because it reuses the item's change.
	finish := e.data.Sequence + 1
	if outcome == "succeeded" {
		if err := nativeEffectError(ctx); err != nil {
			return err
		}
		if e.storedSeq == finish && e.lastDiff != nil && e.worktreeHead != "" && e.lastDiff.HeadSHA != e.worktreeHead {
			if err := e.append(ctx, "run.observed", "", nil, true); err != nil {
				return err
			}
			finish = e.data.Sequence + 1
		}
		if e.storedSeq != finish && e.releaseDelivery == nil {
			if err := e.postDiff(ctx, finish); err != nil && e.ownsChangeCompletion() {
				err = e.executionError(ctx, err)
				if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
				}
				if nativeTransportUnavailable(err) {
					e.change = &runner.NativeChange{Error: err.Error()}
				} else {
					e.change = &runner.NativeChange{VersionError: err.Error(), VersionCode: hubErrorCode(err)}
					e.settled = true
				}
				return nil
			}
		}
		if err := e.settle(ctx, outcome, finish); err != nil {
			err = e.executionError(ctx, err)
			if nativeEffectError(ctx) != nil {
				return err
			}
			if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
			}
			if e.change == nil {
				e.change = &runner.NativeChange{}
			}
			if nativeTransportUnavailable(err) {
				e.change.VersionID, e.change.VersionError, e.change.VersionCode, e.change.Reviewed = "", "", "", false
				e.change.Error = err.Error()
			} else {
				e.change.Error = ""
				e.change.VersionError, e.change.VersionCode = err.Error(), publicationErrorCode(err)
				e.settled = true
			}
		}
		return nativeEffectError(ctx)
	}
	return nil
}

func (e *nativeExecution) preparedFinalization() *tracker.NativeFinalization {
	change := e.change
	if change == nil {
		return nil
	}
	finalization := (tracker.NativeFinalization{
		Publication: e.publication,
		ObservedAt:  e.scheduler.now().UTC(), Settled: e.settled,
		Changed: change.Changed, Files: change.Files, ChangeID: change.ChangeID,
		VersionID: change.VersionID, BaseSHA: change.BaseSHA, HeadSHA: change.HeadSHA,
		Reviewed: change.Reviewed, VersionError: change.VersionError, VersionCode: change.VersionCode,
		Error: change.Error, SourceVersion: e.finalizationSource, SourceAttemptID: e.finalizationAttempt,
	}).Public()
	return &finalization
}

func (e *nativeExecution) Finish(ctx context.Context, outcome string) error {
	e.mu.Lock()
	previous := e.data.Outcome
	err := e.finish(ctx, outcome)
	refresh := previous == "" && e.data.Outcome != ""
	e.mu.Unlock()
	if refresh {
		e.refreshReasons(ctx, true)
	}
	return err
}

func (e *nativeExecution) finish(ctx context.Context, outcome string) (err error) {
	done := e.diagnosticOperation("native.finish")
	defer func() { done(err) }()
	preparationErr := e.prepareFinish(ctx, outcome)
	if errors.Is(preparationErr, runner.ErrExecutionAuthorityUnavailable) {
		return preparationErr
	}
	if outcome == "succeeded" && e.change != nil && e.change.Error != "" {
		return fmt.Errorf("%w: %s", ErrUnavailable, e.change.Error)
	}
	if preparationErr != nil || e.change != nil && e.change.VersionError != "" {
		outcome = "failed"
	}
	if e.data.Outcome != "" {
		return preparationErr
	}
	if e.data.Identity == nil {
		if e.preparedFailure == nil || outcome == "succeeded" {
			return preparationErr
		}
		supported, err := e.claim.source.client.HubFeature(ctx, tracker.NativePreProviderFailureCapability)
		if err != nil || !supported {
			return errors.Join(preparationErr, err)
		}
		e.preparedFailure.Stage = "pre_provider"
		public := e.preparedFailure.Public()
		e.preparedFailure = &public
	}
	e.data.Disposition = e.preparedDisposition
	e.data.TerminalFailure = nil
	if outcome != "succeeded" {
		if e.preparedFailure == nil {
			e.preparedFailure = e.finalizationFailure(preparationErr)
		}
		e.data.TerminalFailure = e.preparedFailure
	}
	if finalization := e.preparedFinalization(); finalization != nil {
		e.data.Finalization = finalization
	}
	finishErr := e.append(ctx, "run.finished", outcome, nil)
	return errors.Join(preparationErr, finishErr)
}

func (e *nativeExecution) finalizationFailure(err error) *tracker.NativeTerminalFailure {
	stage := "finalization"
	if e.change != nil && (e.change.VersionError != "" || e.change.Error != "") {
		stage = "publication"
	}
	if e.role == runner.RoleMerge {
		stage = "landing"
	}
	failure := tracker.NativeTerminalFailure{ErrorClass: stage, Stage: stage, ObservedAt: e.scheduler.now().UTC()}
	var provider interface {
		NativeTerminalFailure() tracker.NativeTerminalFailure
	}
	if errors.As(err, &provider) {
		recorded := provider.NativeTerminalFailure()
		if recorded.Stage != "" {
			failure = recorded
			failure.ObservedAt = e.scheduler.now().UTC()
		}
	}
	switch change := e.change; {
	case change != nil && change.VersionError != "":
		failure.Error = strings.TrimPrefix(change.VersionCode+": "+change.VersionError, ": ")
	case err != nil:
		failure.Error = err.Error()
	case change != nil && change.Error != "":
		failure.Error = change.Error
	default:
		failure.ErrorClass, failure.Stage = "", ""
		failure.Error = "attempt ended without a recorded runner error"
	}
	public := failure.Public()
	return &public
}

func (e *nativeExecution) finishPrepared(ctx context.Context) error {
	e.mu.Lock()
	outcome := e.preparedOutcome
	finished := e.data.Outcome != ""
	interrupted := e.data.Outcome == "interrupted"
	expectedState := e.claim.recovery.Issue.State
	publicationUnavailable := outcome == "succeeded" && !e.settled && e.change != nil && e.change.Error != ""
	e.mu.Unlock()
	var finishErr error
	var rejection *nativeCompletionRejection
	if outcome != "" && !finished {
		if publicationUnavailable && expectedState != "" {
			issue, err := e.claim.source.client.Issue(ctx, e.claim.lease.WorkItemID)
			if err != nil {
				finishErr = e.executionError(ctx, err)
			} else if !strings.EqualFold(expectedState, issue.State) {
				outcome = "cancelled"
			}
		}
		if finishErr == nil && e.remaining() <= 0 {
			finishErr = e.renew(ctx, 0)
		}
		if finishErr == nil {
			finishErr = e.Finish(ctx, outcome)
		}
		if finishErr != nil {
			if errors.As(finishErr, &rejection) {
				finishErr = nil
			} else {
				if !claimLost(finishErr) && !errors.Is(finishErr, orchestrator.ErrSchedulingClaimLost) {
					return finishErr
				}
				finishErr = errors.Join(runner.ErrExecutionAuthorityUnavailable, finishErr)
			}
		}
	}
	if err := e.CloseWorkers(ctx); err != nil {
		return errors.Join(finishErr, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if failure := e.data.TerminalFailure; failure != nil && failure.Stage == "event_publication" {
		rejection = &nativeCompletionRejection{failure: *failure}
	}
	if e.data.Identity != nil && (finishErr != nil || interrupted) && rejection == nil {
		path, err := nativeItemPath(e.claim.lease.WorkItemID)
		if err != nil {
			return errors.Join(finishErr, err)
		}
		var attempt tracker.NativeAttempt
		if err := e.claim.source.client.client.request(ctx, http.MethodGet, e.claim.source.client.base()+path+"/attempts/"+e.data.AttemptID, nil, &attempt); err != nil {
			return errors.Join(finishErr, err)
		}
		if attempt.LeaseID != e.claim.lease.ID || attempt.FencingToken != e.claim.lease.FencingToken || attempt.RunID != e.data.RunID || attempt.PolicyID != e.data.PolicyID || attempt.AttemptID != e.data.AttemptID || attempt.Identity == nil || attempt.Identity.Role != e.data.Identity.Role || attempt.Identity.Backend != e.data.Identity.Backend {
			return errors.Join(finishErr, runner.ErrExecutionAuthorityUnavailable)
		}
		if attempt.ClaimReleasedAt != nil {
			if attempt.Outcome == "" || attempt.Outcome == "interrupted" {
				if err := cancelNativeReleasedAttempt(ctx, e.claim.source.client, e.claim.lease.WorkItemID, attempt); err != nil {
					return errors.Join(finishErr, err)
				}
				attempt.Sequence++
				attempt.Outcome = "cancelled"
			}
			attempt.Identity = e.data.Identity
			if attempt.Handoff == nil {
				attempt.Handoff = attempt.Checkpoint
			}
			e.data, e.pending = attempt.NativeRunData, nil
			e.preparedOutcome = attempt.Outcome
			finishErr = nil
		}
	}
	data := e.data
	finalization := data.Finalization
	completedSource := data.Disposition != nil && data.Disposition.Status == "complete" && finalization != nil &&
		finalization.Settled && finalization.Changed && finalization.ChangeID != "" && finalization.VersionID != "" && finalization.HeadSHA != "" &&
		finalization.VersionError == "" && finalization.Error == ""
	retain := data.Identity != nil && rejection == nil && e.scheduler.client.runner != nil && (data.Outcome == "" || data.Outcome == "interrupted" && !completedSource)
	if !retain {
		if err := e.claim.source.client.Release(ctx, e.claim.lease, "released"); err != nil && !claimLost(err) {
			return errors.Join(finishErr, err)
		}
	}
	if finishErr == nil && rejection != nil {
		return rejection
	}
	return finishErr
}

func cancelNativeReleasedAttempt(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, attempt tracker.NativeAttempt) error {
	data := tracker.NativeRunData{AttemptID: attempt.AttemptID, RunID: attempt.RunID, LeaseID: attempt.LeaseID,
		FencingToken: attempt.FencingToken, PolicyID: attempt.PolicyID, Identity: attempt.Identity,
		Sequence: attempt.Sequence + 1, Outcome: "cancelled"}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: attempt.AttemptID + ":terminated:" + strconv.FormatInt(data.Sequence, 10)}, Type: "run.finished", SchemaVersion: 1, Data: data}
	return client.AppendEvent(ctx, item, event)
}

// NativeChange reports what the finished run left for review.
func (e *nativeExecution) NativeChange() *runner.NativeChange {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.change == nil {
		return nil
	}
	change := *e.change
	return &change
}

// SetDiffSource installs the source the execution calls before every run event
// that references a stored diff.
func (e *nativeExecution) SetDiffSource(source runner.AttemptDiffSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diffSource = source
}

func (e *nativeExecution) captureDiff(ctx context.Context) error {
	if e.diffSource == nil && e.lastDiff == nil {
		return errors.New("the run's final attempt diff source is unavailable")
	}
	if e.diffSource == nil {
		return nil
	}
	request, ok := e.diffSource(ctx)
	switch {
	case ok:
		last := request
		e.lastDiff = &last
	case e.lastDiff == nil:
		return errors.New("the run's final attempt diff is unavailable")
	}
	if e.worktreeState == "clean" || e.worktreeState == "unpushed" {
		return e.retainChangeSource(ctx, *e.lastDiff)
	}
	return nil
}

// postDiff stores the worktree's diff for the event about to be appended. The
// producer tuple is this execution's own lease, because the attempt is still
// running and its lease is therefore the producer; the generation is the run
// event sequence the diff belongs to, which is why the post happens before the
// event and not after it.
func (e *nativeExecution) postDiff(ctx context.Context, sequence int64) (diagnosticErr error) {
	done := e.diagnosticOperation("native.postDiff")
	defer func() { done(diagnosticErr) }()
	if err := e.captureDiff(ctx); err != nil {
		return err
	}
	request := *e.lastDiff
	request.Producer = tracker.DiffProducer{
		Kind: tracker.DiffSourceAttempt, ID: e.data.AttemptID,
		LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken,
	}
	request.Generation = tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: sequence}
	if _, err := e.claim.source.client.PostAttemptDiff(ctx, e.data.AttemptID, request); err != nil {
		return err
	}
	e.storedSeq = sequence
	return nil
}

func (e *nativeExecution) append(ctx context.Context, kind, outcome string, checkpoint *tracker.NativeCheckpoint, immediate ...bool) error {
	if err := e.flush(ctx); err != nil {
		return err
	}
	at := e.scheduler.now().UTC()
	if kind == "run.started" {
		e.usageStartedAt = at
		e.activityBoundary(at, "")
	}
	if kind == "run.finished" && e.data.Identity != nil {
		e.activityBoundary(at, outcome)
	}
	data := e.data
	if kind == "run.finished" {
		data.Usage = slices.Clone(data.Usage)
		for i := range data.Usage {
			data.Usage[i].From = e.usageStartedAt
			data.Usage[i].To, data.Usage[i].ReportedAt = at, at
			data.Usage[i].Revision = data.Sequence + 1
		}
	}
	data.Sequence++
	data.Outcome = outcome
	if kind == "run.finished" {
		data.CompletionBody = e.preparedMessage
	}
	if (kind == "run.checkpointed" || kind == "run.finished") && e.storedSeq != data.Sequence && e.releaseDelivery == nil {
		if err := e.postDiff(ctx, data.Sequence); err != nil {
			slog.Default().Warn("attempt diff not stored", "work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID, "seq", data.Sequence, "error", err)
		}
	}
	data.Handoff = checkpoint
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)}, Type: kind, SchemaVersion: 1, Data: data}
	if len(immediate) == 0 && e.claim.lease.RunDetailBatching && kind == "run.observed" && len(e.observations) < 7 && !e.runtimeHeartbeatDue(data.Runtime) {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		var observation tracker.NativeRunEvent
		if err := json.Unmarshal(encoded, &observation); err != nil {
			return err
		}
		e.observations = append(e.observations, observation)
		checkpoint := e.data.Handoff
		e.data = data
		e.data.Handoff = checkpoint
		e.runtimeDirty = false
		return nil
	}
	event.PriorEvents = append([]tracker.NativeRunEvent(nil), e.observations...)
	e.pending = &event
	return e.flush(ctx)
}

func (e *nativeExecution) flush(ctx context.Context) (err error) {
	if e.pending == nil {
		return nil
	}
	done := e.diagnosticOperation("append." + e.pending.Type)
	defer func() { done(err) }()
	if e.remaining() <= 0 {
		if err := e.renew(ctx, 0); err != nil {
			return e.executionError(ctx, err)
		}
	}
	if failure := e.pending.Data.TerminalFailure; failure != nil && failure.Stage == "event_publication" && e.pending.Data.Runtime == nil && e.data.Sequence > 0 {
		path, err := nativeItemPath(e.claim.lease.WorkItemID)
		if err != nil {
			return err
		}
		var attempt tracker.NativeAttempt
		if err := e.claim.source.client.client.request(ctx, http.MethodGet, e.claim.source.client.base()+path+"/attempts/"+e.data.AttemptID, nil, &attempt); err != nil {
			return e.executionError(ctx, err)
		}
		if attempt.LeaseID != e.claim.lease.ID || attempt.FencingToken != e.claim.lease.FencingToken || attempt.RunID != e.data.RunID || attempt.PolicyID != e.data.PolicyID || attempt.Sequence != e.data.Sequence {
			return e.unavailable(runner.ErrExecutionAuthorityUnavailable)
		}
		e.pending.Data.Identity = attempt.Identity
		e.pending.Data.Steps = slices.Clone(attempt.Steps)
		e.pending.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: e.scheduler.now().UTC()}
		if r := attempt.Runtime; r != nil {
			e.pending.Data.Runtime.LocalAttemptID, e.pending.Data.Runtime.Generation, e.pending.Data.Runtime.WorkerLaunch = r.LocalAttemptID, r.Generation, r.WorkerLaunch
		}
	}
	events := append([]tracker.NativeRunEvent{*e.pending}, e.pending.PriorEvents...)
	for i := range events {
		for j, receipt := range events[i].Data.Steps {
			bounded, err := e.retainStep(ctx, receipt)
			if err != nil {
				return err
			}
			events[i].Data.Steps[j] = bounded
		}
	}
	e.pending.Data.Steps = events[0].Data.Steps
	e.pending.PriorEvents = events[1:]
	if err := e.claim.source.client.AppendEvent(ctx, e.claim.lease.WorkItemID, *e.pending); err != nil {
		var refusal *APIError
		if (e.pending.Type == "run.finished" || e.pending.Type == "run.checkpointed") && errors.As(err, &refusal) && refusal.permanentInvalidRequest() {
			if len(e.pending.PriorEvents) > 0 {
				e.data.Sequence = e.pending.PriorEvents[0].Data.Sequence - 1
				e.observations = nil
			}
			failure := refusal.NativeTerminalFailure()
			failure.Stage, failure.ErrorClass = "event_publication", "event_publication"
			failure.Error = "Hub rejected the completion request"
			failure.ObservedAt = e.scheduler.now().UTC()
			failure = failure.Public()
			data := tracker.NativeRunData{
				RunID: e.data.RunID, AttemptID: e.data.AttemptID, PolicyID: e.claim.lease.PolicyID,
				LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken,
				Identity: e.data.Identity, Sequence: e.data.Sequence + 1, Outcome: "failed", TerminalFailure: &failure,
			}
			if data.Identity == nil && e.data.Runtime != nil {
				data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", LocalAttemptID: e.data.Runtime.LocalAttemptID, Generation: e.data.Runtime.Generation}
			}
			key := data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10) + ":rejected"
			if e.pending.IdempotencyKey == key {
				failure = *e.pending.Data.TerminalFailure
				e.data.TerminalFailure, e.pending = &failure, nil
				e.preparedOutcome = ""
				return &nativeCompletionRejection{failure: failure}
			}
			e.preparedOutcome, e.preparedFailure = "failed", &failure
			e.pending = &tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: key}, Type: "run.finished", SchemaVersion: 1, Data: data}
			if err := e.flush(ctx); err != nil {
				return err
			}
			return &nativeCompletionRejection{failure: failure}
		}
		return e.executionError(ctx, err)
	}
	checkpoint := e.data.Handoff
	e.data = e.pending.Data
	if e.data.Runtime != nil {
		e.runtimePublishedAt = e.data.Runtime.HeartbeatAt
	}
	if e.data.Handoff == nil {
		e.data.Handoff = checkpoint
	}
	e.pending = nil
	e.observations = nil
	e.runtimeDirty = false
	return nil
}

type nativeCompletionRejection struct {
	failure tracker.NativeTerminalFailure
}

func (e *nativeCompletionRejection) Error() string {
	return e.failure.Error
}

func (e *nativeCompletionRejection) NativeTerminalFailure() tracker.NativeTerminalFailure {
	return e.failure
}

func (e *nativeExecution) AvailabilityDeadline() time.Time {
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	claim, ok := e.scheduler.nativeClaims[string(e.claim.lease.WorkItemID)]
	if !ok || claim.lease.FencingToken != e.claim.lease.FencingToken {
		return time.Time{}
	}
	return claim.availabilityDeadline
}
