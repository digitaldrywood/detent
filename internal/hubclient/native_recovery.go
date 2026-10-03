package hubclient

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) Attempts(ctx context.Context, id tracker.NativeWorkItemID, cursor string) (tracker.Page[tracker.NativeAttempt], error) {
	return c.AttemptsPage(ctx, id, cursor, 100)
}

func (c *NativeClient) Recovery(ctx context.Context, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	var result tracker.NativeRecovery
	var err error
	result.Issue, err = c.Issue(ctx, id)
	if err != nil {
		return result, err
	}
	for cursor := ""; ; {
		page, err := c.Comments(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.Discussion = append(result.Discussion, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for cursor := ""; ; {
		page, err := c.Attempts(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.Attempts = append(result.Attempts, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	for cursor := ""; ; {
		page, err := c.History(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		result.History = append(result.History, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	detail, err := c.currentChange(ctx, id)
	if err != nil {
		return result, err
	}
	if detail.Change.ID == "" {
		return result, nil
	}
	result.ChangeDetail = &detail
	if detail.Change.CurrentVersion == "" {
		return result, nil
	}
	for _, version := range result.ChangeDetail.Versions {
		if version.ID == result.ChangeDetail.Change.CurrentVersion {
			result.Change = &tracker.NativeChangeReference{ChangeID: result.ChangeDetail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			return result, nil
		}
	}
	return result, fmt.Errorf("read change: current version %s is missing", result.ChangeDetail.Change.CurrentVersion)
}
