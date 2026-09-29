package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			e.publishVersion(ctx, diff, change)
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

// publishVersion puts the run's head on the Change Request as an immutable
// version, the record a reviewer approves or sends back. A change whose
// current version already carries this head, as a rework run that changed
// nothing new leaves, is not published again. A version the hub refuses is
// reported on the change rather than failing the run: the Change Request
// exists and the item still reaches review, where the reason is shown, and
// the next successful run publishes again. The idempotency key is the
// attempt's, so a retried finish cannot publish two. e.mu is held by the
// caller.
func (e *nativeExecution) publishVersion(ctx context.Context, diff tracker.AttemptDiffRequest, change *runner.NativeChange) {
	id, err := e.publishChangeVersion(ctx, change.ChangeID, diff)
	if err != nil {
		slog.Default().Warn("native change version not published",
			"work_item", e.claim.lease.WorkItemID, "attempt", e.data.AttemptID, "change", change.ChangeID, "error", err)
		change.VersionID, change.VersionError = "", err.Error()
		return
	}
	change.VersionID, change.VersionError = id, ""
}

func (e *nativeExecution) publishChangeVersion(ctx context.Context, changeID string, diff tracker.AttemptDiffRequest) (string, error) {
	client, item := e.claim.source.client, e.claim.lease.WorkItemID
	detail, err := client.Change(ctx, item, changeID)
	if err != nil {
		return "", fmt.Errorf("read change: %w", err)
	}
	for _, version := range detail.Versions {
		if version.ID == detail.Change.CurrentVersion && version.HeadSHA == diff.HeadSHA {
			return version.ID, nil
		}
	}
	if e.repository == "" {
		return "", errors.New("the checkout's origin remote is not an https repository the version can name")
	}
	digest := sha256.Sum256([]byte(diff.HeadSHA))
	version, err := client.PublishChangeVersion(ctx, item, changeID, tracker.PublishChangeVersion{
		Mutation:          tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":version", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken},
		ExpectedVersionID: detail.Change.CurrentVersion,
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: diff.BaseSHA, HeadSHA: diff.HeadSHA, MergeBaseSHA: diff.BaseSHA,
			Repository: e.repository,
			Code:       tracker.ChangeArtifact{Kind: "code", URI: e.repository + "/commit/" + diff.HeadSHA, SHA256: hex.EncodeToString(digest[:]), Availability: "unverified"},
			Artifacts:  []tracker.ChangeArtifact{},
			RunID:      e.data.RunID, AttemptID: e.data.AttemptID, PolicyID: e.data.PolicyID,
		},
	})
	if err != nil {
		return "", fmt.Errorf("publish version: %w", err)
	}
	return version.ID, nil
}

// SetRepository names the repository a published version refers to.
func (e *nativeExecution) SetRepository(repository string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repository = repository
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
