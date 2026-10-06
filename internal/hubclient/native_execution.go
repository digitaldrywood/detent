package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeExecution struct {
	usageStartedAt    time.Time
	artifacts         nativeArtifacts
	scheduler         *Scheduler
	claim             nativeClaim
	mu                sync.Mutex
	data              tracker.NativeRunData
	evidenceSource    func(context.Context, string) (runner.ValidationEvidence, error)
	integrationSource func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)
	pending           *tracker.NativeRunEvent
	cancel            context.CancelCauseFunc
	// diffSource computes the stored attempt diff the execution posts before
	// every checkpoint and before the finish (decisions section 18.5). It is
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
	change                   *runner.NativeChange
	preparedOutcome          string
	preparedMessage          string
	preparedDisposition      *tracker.NativeDisposition
	runtimeDirty             bool
	runtimeSupported         *bool
	// repository is the https URL of the checkout's origin, which a published
	// version names; empty when the remote cannot be named that way.
	repository string
}

type nativeMutationAuthorityKey struct{}

type nativeMutationAuthority struct {
	scope string
	lease tracker.NativeLease
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
	bound := context.WithValue(ctx, nativeMutationAuthorityKey{}, nativeMutationAuthority{scope: e.claim.source.client.base(), lease: e.claim.lease})
	guarded, cancel := context.WithCancelCause(bound)
	e.mu.Lock()
	e.cancel = cancel
	e.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var routingChanged <-chan struct{}
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
					e.mu.Lock()
					e.claim.availabilityDeadline = deadline
					e.mu.Unlock()
					routingChanged = changed
				}
			}
			remaining := e.remaining()
			if remaining <= 0 {
				cancel(runner.ErrExecutionAuthorityUnavailable)
				return
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
	return guarded, func() { cancel(context.Canceled); <-done }, nil
}

func (e *nativeExecution) unavailable(err error) error {
	cause := errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
	return cause
}

func (e *nativeExecution) executionError(err error) error {
	if nativeAuthorityLost(err) {
		err = e.scheduler.nativeClaimError(string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	}
	if e.remaining() <= 0 || errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
	}
	return err
}

func (e *nativeExecution) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return e.validationError(errors.Join(err, context.Cause(ctx)))
	}
	if e.remaining() <= 0 {
		return e.unavailable(nil)
	}
	if err := e.scheduler.checkClaimPolicy(ctx, string(e.claim.lease.WorkItemID), e.claim.lease.PolicyID); err != nil {
		return e.validationError(err)
	}
	if e.scheduler.client.runner == nil {
		_, err := e.claim.source.client.ValidateLease(ctx, e.claim.lease)
		return e.validationError(err)
	}
	return nil
}

func (e *nativeExecution) validationError(err error) error {
	if err == nil {
		return nil
	}
	err = e.scheduler.nativeClaimError(string(e.claim.lease.WorkItemID), e.claim.lease.FencingToken, err)
	err = e.executionError(err)
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

func (e *nativeExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	e.mu.Lock()
	if e.data.Identity != nil && e.data.Sequence > 0 {
		defer e.mu.Unlock()
		if *e.data.Identity != identity {
			return errors.New("native execution identity changed during an attempt")
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
	return e.append(ctx, "run.started", "", nil)
}

func (e *nativeExecution) Checkpoint(ctx context.Context, checkpoint tracker.NativeCheckpoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.data.Identity == nil {
		return nil
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

func (e *nativeExecution) PrepareFinish(ctx context.Context, outcome, finalMessage string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.preparedOutcome = outcome
	e.preparedMessage = finalMessage
	if len(e.preparedMessage) > 64<<10 {
		e.preparedMessage = e.preparedMessage[:64<<10]
		for !utf8.ValidString(e.preparedMessage) {
			e.preparedMessage = e.preparedMessage[:len(e.preparedMessage)-1]
		}
	}
	e.preparedDisposition = nil
	if signal, reported := workpad.SignalFromComment(finalMessage, "", ""); e.ownsChangeCompletion() && reported && signal != nil && signal.Invalid == nil {
		e.preparedDisposition = &tracker.NativeDisposition{Status: signal.Status, Blockers: len(signal.Blockers) != 0, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode, FinalSummary: workpad.FinalSummary(finalMessage), BlockerEvidence: signal.Blockers}
	}
	if err := e.prepareFinish(ctx, outcome); err != nil {
		err = e.executionError(err)
		if outcome == "succeeded" && e.ownsChangeCompletion() && nativeTransportUnavailable(err) {
			if e.change == nil {
				e.change = &runner.NativeChange{}
			}
			e.change.Error = err.Error()
			return nil
		}
		e.preparedOutcome = "failed"
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
		if e.storedSeq != finish {
			if err := e.postDiff(ctx, finish); err != nil && e.ownsChangeCompletion() {
				err = e.executionError(err)
				if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
				}
				e.change = &runner.NativeChange{Error: err.Error()}
				return nil
			}
		}
		if err := e.settle(ctx, outcome, finish); err != nil {
			err = e.executionError(err)
			if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				return errors.Join(runner.ErrExecutionAuthorityUnavailable, err)
			}
			if e.change == nil {
				e.change = &runner.NativeChange{}
			}
			if nativeTransportUnavailable(err) {
				e.change.VersionID, e.change.VersionError, e.change.VersionCode, e.change.Reviewed = "", "", "", false
				e.change.Error = err.Error()
			} else if e.change.VersionError == "" {
				e.change.Error = err.Error()
			}
		}
	}
	return nil
}

func (e *nativeExecution) Finish(ctx context.Context, outcome string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	preparationErr := e.prepareFinish(ctx, outcome)
	if preparationErr != nil {
		outcome = "failed"
	}
	if e.data.Identity == nil || e.data.Outcome != "" {
		return preparationErr
	}
	e.data.Disposition = e.preparedDisposition
	return errors.Join(preparationErr, e.append(ctx, "run.finished", outcome, nil))
}

func (e *nativeExecution) finishPrepared(ctx context.Context) error {
	e.mu.Lock()
	outcome := e.preparedOutcome
	finished := e.data.Outcome != ""
	e.mu.Unlock()
	// An acknowledged Finish stays settled when lease release is retried.
	if outcome == "" || finished {
		return nil
	}
	if e.remaining() <= 0 {
		return orchestrator.ErrSchedulingClaimLost
	}
	return e.Finish(ctx, outcome)
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
// that references a stored diff (decisions section 18.5).
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
	return nil
}

// postDiff stores the worktree's diff for the event about to be appended. The
// producer tuple is this execution's own lease, because the attempt is still
// running and its lease is therefore the producer; the generation is the run
// event sequence the diff belongs to, which is why the post happens before the
// event and not after it.
func (e *nativeExecution) postDiff(ctx context.Context, sequence int64) error {
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

func (e *nativeExecution) append(ctx context.Context, kind, outcome string, checkpoint *tracker.NativeCheckpoint) error {
	if err := e.flush(ctx); err != nil {
		return err
	}
	data := e.data
	at := e.scheduler.now().UTC()
	if kind == "run.started" {
		e.usageStartedAt = at
	}
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
	if (kind == "run.checkpointed" || kind == "run.finished") && e.storedSeq != data.Sequence {
		if err := e.postDiff(ctx, data.Sequence); err != nil {
			slog.Default().Warn("attempt diff not stored", "work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID, "seq", data.Sequence, "error", err)
		}
	}
	data.Handoff = checkpoint
	e.pending = &tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: data.AttemptID + ":" + strconv.FormatInt(data.Sequence, 10)}, Type: kind, SchemaVersion: 1, Data: data}
	return e.flush(ctx)
}

func (e *nativeExecution) flush(ctx context.Context) error {
	if e.pending == nil {
		return nil
	}
	if e.remaining() <= 0 {
		return runner.ErrExecutionAuthorityUnavailable
	}
	if err := e.claim.source.client.AppendEvent(ctx, e.claim.lease.WorkItemID, *e.pending); err != nil {
		return e.executionError(err)
	}
	e.data = e.pending.Data
	e.pending = nil
	e.runtimeDirty = false
	return nil
}

func (e *nativeExecution) AvailabilityDeadline() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.claim.availabilityDeadline
}
