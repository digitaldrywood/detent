package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) FencedMutation(ctx context.Context, id tracker.NativeWorkItemID, mutation tracker.Mutation) tracker.Mutation {
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

type nativeMutationAuthorityKey struct{}

type executionEventAuthorityKey struct{}

func (c *NativeClient) AppendExecutionEvent(ctx context.Context, id tracker.NativeWorkItemID, event tracker.NativeRunEvent, authorize func(context.Context) error) error {
	return c.AppendEvent(context.WithValue(ctx, executionEventAuthorityKey{}, authorize), id, event)
}

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
	if errors.Is(cause, runner.ErrOperatorStopped) || errors.Is(cause, runner.ErrExecutionAuthorityUnavailable) {
		return cause
	}
	return nil
}
func ExecutionID(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "_" + hex.EncodeToString(digest[:16])
}

func CancelNativeReleasedAttempt(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, attempt tracker.NativeAttempt) error {
	if attempt.CleanupAcknowledgedAt != nil {
		return nil
	}
	data := tracker.NativeRunData{AttemptID: attempt.AttemptID, RunID: attempt.RunID, LeaseID: attempt.LeaseID,
		FencingToken: attempt.FencingToken, PolicyID: attempt.PolicyID, Identity: attempt.Identity,
		Sequence: attempt.Sequence + 1, Outcome: "cancelled"}
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: attempt.AttemptID + ":terminated:" + strconv.FormatInt(data.Sequence, 10)}, Type: "run.finished", SchemaVersion: 1, Data: data}
	return client.AppendEvent(ctx, item, event)
}

func (c *NativeClient) ExecutionAttempt(ctx context.Context, item tracker.NativeWorkItemID, attempt string) (tracker.NativeAttempt, error) {
	var result tracker.NativeAttempt
	path, err := nativeAttemptPath(item, attempt)
	if err != nil {
		return result, err
	}
	err = c.readExecutionJSON(ctx, c.base()+path+"?view=execution", 3*attachment.MaxSectionBytes, &result)
	return result, err
}

func (c *NativeClient) readExecutionJSON(ctx context.Context, path string, limit int64, value any) error {
	raw, _, err := c.client.download(ctx, path, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, value); err != nil {
		return fmt.Errorf("%w: decode execution response: %w", ErrUnavailable, err)
	}
	return nil
}
