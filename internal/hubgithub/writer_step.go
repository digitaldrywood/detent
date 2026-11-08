package hubgithub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func (w *Writer) Step(ctx context.Context, item hubserver.OutboxItem) (hubserver.OutboxStepResult, error) {
	ctx = scopedRequests(ctx, item.Profile, "projection")
	result := hubserver.OutboxStepResult{Progress: item.Progress}
	if w == nil || w.client == nil {
		return result, hubserver.Permanent(errors.New("GitHub outbound transport is unavailable"))
	}
	if err := validateRepository(item); err != nil {
		return result, err
	}
	if item.IdempotencyKey == "" || item.OrganizationID == "" {
		return result, hubserver.Permanent(errors.New("GitHub outbound identity is incomplete"))
	}
	if item.BeforeWrite == nil {
		return result, hubserver.Permanent(errors.New("GitHub outbound ownership is unavailable"))
	}
	var err error
	switch item.Kind {
	case hubserver.MutationWorkpad:
		result, err = w.stepWorkpad(ctx, item)
	case hubserver.MutationWorkflowLabel:
		err = w.updateWorkflowLabel(ctx, item)
		result.Done = err == nil
	case hubserver.MutationMergePullRequest:
		result, err = w.stepMerge(ctx, item)
	default:
		err = hubserver.Permanent(errors.New("GitHub outbound mutation is unsupported"))
	}
	return result, classifyError(err)
}

func (w *Writer) writeOutbound(ctx context.Context, item hubserver.OutboxItem, method, path string, body, output any) error {
	if item.BeforeWrite != nil {
		if err := item.BeforeWrite(ctx); err != nil {
			return hubserver.RejectedOutboxWrite(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return hubserver.RejectedOutboxWrite(err)
	}
	err := w.client.REST(ctx, method, path, body, output)
	var status *connectorgithub.StatusError
	if errors.As(err, &status) && status.StatusCode >= 400 && status.StatusCode < 500 && status.StatusCode != http.StatusRequestTimeout {
		return hubserver.RejectedOutboxWrite(err)
	}
	if errors.Is(err, connectorgithub.ErrMissingToken) || errors.Is(err, connectorgithub.ErrRESTBudgetReserved) {
		return hubserver.RejectedOutboxWrite(err)
	}
	return err
}

func (w *Writer) stepWorkpad(ctx context.Context, item hubserver.OutboxItem) (hubserver.OutboxStepResult, error) {
	result := hubserver.OutboxStepResult{Progress: item.Progress}
	if err := validateIssueTarget(item); err != nil {
		return result, err
	}
	var desired hubserver.WorkpadDesired
	if err := decodeDesired(item, &desired); err != nil {
		return result, err
	}
	if strings.TrimSpace(desired.Body) == "" || strings.TrimSpace(desired.Marker) == "" {
		return result, hubserver.Permanent(errors.New("GitHub workpad is incomplete"))
	}
	if result.Progress.Stage == "close" {
		if item.Uncertain {
			var issue restIssue
			if err := w.client.REST(ctx, http.MethodGet, issuePath(item), nil, &issue); err != nil {
				return result, err
			}
			if issue.State == "closed" {
				result.Done = true
				return result, nil
			}
		}
		body := map[string]any{"state": "closed"}
		if desired.StateReason != "" {
			body["state_reason"] = desired.StateReason
		}
		err := w.writeOutbound(ctx, item, http.MethodPatch, issuePath(item), body, nil)
		result.Done = err == nil
		return result, err
	}
	result.Progress.Stage = "comments"
	path := result.Progress.Cursor
	if path == "" {
		path = issuePath(item) + "/comments?per_page=100"
	}
	var comments []restComment
	next, err := w.client.RESTPage(ctx, path, &comments)
	if err != nil {
		return result, err
	}
	if len(comments) > 100 {
		return result, hubserver.Permanent(errors.New("GitHub comment page exceeded its budget"))
	}
	digest := sha256.Sum256([]byte(string(item.OrganizationID) + "\x00" + item.IdempotencyKey))
	marker := "<!-- detent-outbound:" + hex.EncodeToString(digest[:]) + " -->"
	for _, comment := range comments {
		if comment.ID <= 0 {
			continue
		}
		if strings.Contains(comment.Body, marker) || !item.Uncertain && desired.Summary && strings.Contains(comment.Body, desired.Marker) {
			result.Progress.Stage, result.Progress.Cursor = "close", ""
			result.Done = !desired.CloseSource
			return result, nil
		}
		if result.Progress.TargetID == 0 && strings.Contains(comment.Body, desired.Marker) {
			result.Progress.TargetID = comment.ID
		}
		if result.Progress.FallbackID == 0 && strings.HasPrefix(strings.TrimSpace(comment.Body), workpadHeading) {
			result.Progress.FallbackID = comment.ID
		}
	}
	result.Progress.Cursor = next
	if next != "" {
		return result, nil
	}
	if item.Uncertain {
		result.Progress = hubserver.OutboxProgress{}
		return result, hubserver.ErrOutboxUncertain
	}
	body := strings.TrimSpace(desired.Body)
	if !strings.Contains(body, desired.Marker) {
		body += "\n\n" + desired.Marker
	}
	if !strings.Contains(body, marker) {
		body += "\n\n" + marker
	}
	commentID := result.Progress.TargetID
	if commentID == 0 && !desired.Summary {
		commentID = result.Progress.FallbackID
	}
	if commentID == 0 {
		var created restComment
		err = w.writeOutbound(ctx, item, http.MethodPost, issuePath(item)+"/comments", map[string]any{"body": body}, &created)
		if err == nil && created.ID <= 0 {
			err = errors.New("GitHub comment write returned no identity")
		}
	} else {
		err = w.writeOutbound(ctx, item, http.MethodPatch, repositoryPath(item)+"/issues/comments/"+strconv.FormatInt(commentID, 10), map[string]any{"body": body}, nil)
	}
	if err != nil {
		result.Progress = hubserver.OutboxProgress{}
		return result, fmt.Errorf("publish GitHub workpad: %w", err)
	}
	result.Progress.Stage, result.Progress.Cursor = "close", ""
	result.Done = !desired.CloseSource
	return result, nil
}

var _ hubserver.OutboxStepBackend = (*Writer)(nil)
