package operatortool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type IssueContext struct {
	Item          json.RawMessage        `json:"item"`
	Comments      []json.RawMessage      `json:"comments"`
	History       []json.RawMessage      `json:"history"`
	Attempts      []json.RawMessage      `json:"attempts"`
	LatestWorkpad *tracker.NativeComment `json:"latest_workpad"`
	Relationships json.RawMessage        `json:"relationships"`
	References    []json.RawMessage      `json:"references"`
}

func ReadIssueContext(ctx context.Context, reader WorkReader, request WorkReadRequest) (IssueContext, error) {
	result := IssueContext{Comments: []json.RawMessage{}, History: []json.RawMessage{}, Attempts: []json.RawMessage{}, References: []json.RawMessage{}}
	request.Cursor, request.Offset, request.Limit = "", 0, MaxItemLimit
	for _, section := range []struct {
		name   string
		single *json.RawMessage
		items  *[]json.RawMessage
	}{
		{WorkItem, &result.Item, nil}, {WorkComments, nil, &result.Comments}, {WorkHistory, nil, &result.History}, {WorkRuns, nil, &result.Attempts}, {WorkRelationships, &result.Relationships, nil}, {WorkReferences, nil, &result.References},
	} {
		pageRequest := request
		for {
			read, err := reader.ReadWork(ctx, section.name, pageRequest)
			if err != nil {
				return result, fmt.Errorf("read %s: %w", section.name, err)
			}
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(read.Content, &envelope); err != nil {
				return result, err
			}
			if section.single != nil {
				*section.single = envelope.Data
				break
			}
			var page struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor string            `json:"next_cursor"`
				NextOffset *int              `json:"next_offset"`
			}
			if err := json.Unmarshal(envelope.Data, &page); err != nil {
				return result, err
			}
			*section.items = append(*section.items, page.Items...)
			if page.NextCursor != "" {
				if page.NextCursor == pageRequest.Cursor {
					return result, fmt.Errorf("%s returned an unchanged cursor", section.name)
				}
				pageRequest.Cursor = page.NextCursor
			} else if page.NextOffset != nil {
				if *page.NextOffset <= pageRequest.Offset {
					return result, fmt.Errorf("%s returned an unchanged offset", section.name)
				}
				pageRequest.Offset = *page.NextOffset
			} else {
				break
			}
		}
	}
	for _, raw := range result.Comments {
		var comment tracker.NativeComment
		if err := json.Unmarshal(raw, &comment); err != nil {
			return result, err
		}
		if strings.Contains(comment.Body, "## Codex Workpad") && (result.LatestWorkpad == nil || comment.CreatedAt.After(result.LatestWorkpad.CreatedAt)) {
			result.LatestWorkpad = &comment
		}
	}
	return result, nil
}
