package hubclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// A hub-native item has no pull request, so a successful work run's commits
// reach review only through the item's native Change Request. The agent cannot
// open it: every work item mutation is fenced by the run's lease, which only
// the runner holds. The execution therefore opens it when the run finishes,
// under that lease, and reports what it found; the orchestrator moves the item
// (INV-1), so a run that changed something and a run that changed nothing both
// leave the dispatchable set instead of being continued again.

const maxNativeChangeTitle = 512

// settle decides, before run.finished is published at the finish sequence,
// what the run left for review. It records nothing when the run is not work
// the runner owns the review of, when no diff was ever readable, when the worktree was left dirty
// or unread (the ordinary completion path resolves uncommitted work), or when
// the lease is gone: the next owner of the item decides then.
//
// A committed change is reported as reviewable only once the hub has stored
// the finish diff and the Change Request exists. A create the hub refuses is
// retried within the finish, under the same idempotency key, and a finish
// called again retries it too; a change that still cannot be opened is
// reported with its error so the orchestrator hands the item off instead of
// moving it to review. e.mu is held by the caller.
func (e *nativeExecution) settle(ctx context.Context, outcome string, finish int64) {
	if e.settled {
		return
	}
	if outcome != "succeeded" || e.conversation || e.lastDiff == nil || e.claim.source == nil || e.claim.source.client == nil ||
		e.role != runner.RoleCode && e.role != runner.RoleRework ||
		e.worktreeState != "clean" && e.worktreeState != "unpushed" {
		e.settled = true
		return
	}
	if e.remaining() <= 0 {
		e.settled = true
		return
	}
	diff := *e.lastDiff
	change := &runner.NativeChange{
		Changed: diff.HeadSHA != "" && diff.HeadSHA != diff.BaseSHA && len(diff.Files) > 0,
		BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, Files: len(diff.Files),
	}
	e.change = change
	if !change.Changed {
		e.settled = true
		return
	}
	if e.storedSeq != finish {
		change.Error = "the hub has not stored the run's final attempt diff"
		e.settled = true
		return
	}
	var err error
	for try := range nativeChangeCreateTries {
		if try > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(nativeChangeCreateBackoff):
			}
		}
		if ctx.Err() != nil {
			break
		}
		var id string
		if id, err = e.openChange(ctx, diff); err == nil {
			change.ChangeID, change.Error = id, ""
			e.settled = true
			return
		}
		if e.remaining() <= 0 || nativeLeaseLost(err) {
			slog.Default().Warn("native change not opened: lease lost",
				"work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID, "error", err)
			e.change = nil
			e.settled = true
			return
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	slog.Default().Warn("native change not opened",
		"work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID, "error", err)
	change.Error = errorText(err)
}

const (
	nativeChangeCreateTries   = 3
	nativeChangeCreateBackoff = 250 * time.Millisecond
)

func errorText(err error) string {
	if err == nil {
		return "the Change Request could not be opened"
	}
	return err.Error()
}

// openChange returns the item's Change Request, opening one when the item has
// none. A rework run's commits belong to the change the item already has.
// The idempotency key is the attempt's, so a retried open cannot open two.
func (e *nativeExecution) openChange(ctx context.Context, diff tracker.AttemptDiffRequest) (string, error) {
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	existing, err := client.Changes(ctx, item)
	if err != nil {
		return "", fmt.Errorf("list changes: %w", err)
	}
	if len(existing) > 0 {
		return existing[len(existing)-1].ID, nil
	}
	created, err := client.CreateChange(ctx, item, tracker.CreateChange{
		Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":change", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		Title:    nativeChangeTitle(e.claim.recovery.Issue.Title, item),
		Body:     nativeChangeBody(e.data.AttemptID, diff),
	})
	if err != nil {
		return "", fmt.Errorf("create change: %w", err)
	}
	return created.ID, nil
}

func nativeChangeTitle(title string, item tracker.NativeWorkItemID) string {
	if title == "" {
		title = string(item)
	}
	if len(title) <= maxNativeChangeTitle {
		return title
	}
	cut := maxNativeChangeTitle
	for cut > 0 && title[cut]&0xC0 == 0x80 {
		cut--
	}
	return title[:cut]
}

func nativeChangeBody(attempt string, diff tracker.AttemptDiffRequest) string {
	return fmt.Sprintf("Opened by the runner when attempt %s succeeded.\n\nBase: %s\nHead: %s\nFiles changed: %d\n\nThe change is the attempt's stored diff.",
		attempt, diff.BaseSHA, diff.HeadSHA, len(diff.Files))
}

// nativeLeaseLost reports whether the hub refused a mutation because the lease
// that fenced it is no longer the current one.
func nativeLeaseLost(err error) bool {
	if claimLost(err) {
		return true
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Code == "stale_execution"
}
