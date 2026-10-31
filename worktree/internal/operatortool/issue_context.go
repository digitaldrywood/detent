package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type IssueContext struct {
	Item          json.RawMessage              `json:"item"`
	Comments      []json.RawMessage            `json:"comments"`
	History       []json.RawMessage            `json:"history"`
	Attempts      []json.RawMessage            `json:"attempts"`
	LatestWorkpad *tracker.NativeComment       `json:"latest_workpad"`
	Relationships json.RawMessage              `json:"relationships"`
	References    []json.RawMessage            `json:"references"`
	Limits        map[string]IssueContextLimit `json:"limits,omitempty"`
}

type IssueContextLimit struct {
	Cursor string `json:"cursor"`
	Offset int    `json:"offset,omitempty"`
	Error  string `json:"error,omitempty"`
}

func ReadIssueContext(ctx context.Context, reader WorkReader, request WorkReadRequest) (IssueContext, error) {
	result := IssueContext{Comments: []json.RawMessage{}, History: []json.RawMessage{}, Attempts: []json.RawMessage{}, References: []json.RawMessage{}, Limits: map[string]IssueContextLimit{}}
	request.Cursor, request.Offset, request.Limit = "", 0, MaxItemLimit
	for _, section := range []string{WorkItem, WorkComments, WorkHistory, WorkRuns, WorkRelationships, WorkReferences} {
		pageRequest := request
		for {
			read, err := reader.ReadWork(ctx, section, pageRequest)
			if err != nil {
				if !errors.Is(err, ErrReadUnavailable) && !errors.Is(err, ErrResultTooLarge) {
					return result, fmt.Errorf("read %s: %w", section, err)
				}
				publicError := ErrReadUnavailable.Error()
				if errors.Is(err, ErrResultTooLarge) {
					publicError = ErrResultTooLarge.Error()
				}
				result.Limits[section] = IssueContextLimit{Cursor: pageRequest.Cursor, Offset: pageRequest.Offset, Error: publicError}
				break
			}
			var envelope struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(read.Content, &envelope); err != nil {
				return result, err
			}
			var page struct {
				Items      []json.RawMessage `json:"items"`
				NextCursor string            `json:"next_cursor"`
				NextOffset *int              `json:"next_offset"`
			}
			candidate := result
			switch section {
			case WorkItem:
				candidate.Item = envelope.Data
			case WorkRelationships:
				candidate.Relationships = envelope.Data
			default:
				if err := json.Unmarshal(envelope.Data, &page); err != nil {
					return result, err
				}
				switch section {
				case WorkComments:
					candidate.Comments = append(candidate.Comments, page.Items...)
					for _, raw := range page.Items {
						var comment tracker.NativeComment
						if err := json.Unmarshal(raw, &comment); err != nil {
							return result, err
						}
						if strings.Contains(comment.Body, "## Codex Workpad") && (candidate.LatestWorkpad == nil || comment.CreatedAt.After(candidate.LatestWorkpad.CreatedAt)) {
							candidate.LatestWorkpad = &comment
						}
					}
				case WorkHistory:
					candidate.History = append(candidate.History, page.Items...)
				case WorkRuns:
					candidate.Attempts = append(candidate.Attempts, page.Items...)
				case WorkReferences:
					candidate.References = append(candidate.References, page.Items...)
				}
			}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				return result, err
			}
			if len(encoded) > MaxResultBytes/2 {
				result.Limits[section] = IssueContextLimit{Cursor: pageRequest.Cursor, Offset: pageRequest.Offset}
				break
			}
			result = candidate
			if page.NextCursor != "" {
				if page.NextCursor == pageRequest.Cursor {
					return result, fmt.Errorf("%s returned an unchanged cursor", section)
				}
				pageRequest.Cursor = page.NextCursor
			} else if page.NextOffset != nil {
				if *page.NextOffset <= pageRequest.Offset {
					return result, fmt.Errorf("%s returned an unchanged offset", section)
				}
				pageRequest.Offset = *page.NextOffset
			} else {
				break
			}
		}
	}
	return result, nil
}
