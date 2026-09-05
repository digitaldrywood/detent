package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type graphQLClient interface {
	GraphQL(context.Context, string, map[string]any, any) error
}

const importEditsQuery = `query($id:ID!, $after:String) {
node(id:$id) {
  ... on Issue { userContentEdits(first:100, after:$after) { ...Edits } }
  ... on IssueComment { userContentEdits(first:100, after:$after) { ...Edits } }
}
}
fragment Edits on UserContentEditConnection {
  nodes { id createdAt updatedAt editedAt deletedAt diff editor { login ... on Node { id } } }
  pageInfo { hasNextPage endCursor }
}`

func (i *Importer) fetchEdits(ctx context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	var result hubserver.GitHubImportPage
	client, ok := i.client.(graphQLClient)
	if !ok {
		return result, errors.New("github GraphQL edit-history transport is unavailable")
	}
	var response struct {
		Node *struct {
			Edits struct {
				Nodes    []json.RawMessage `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"userContentEdits"`
		} `json:"node"`
	}
	var after any
	if request.Cursor != "" {
		after = request.Cursor
	}
	if err := client.GraphQL(ctx, importEditsQuery, map[string]any{"id": request.SourceID, "after": after}, &response); err != nil {
		return result, err
	}
	if response.Node == nil {
		return hubserver.GitHubImportPage{Gaps: []string{"Edit-history source is deleted or inaccessible: " + request.SourceID}}, nil
	}
	if response.Node.Edits.PageInfo.HasNextPage {
		result.NextCursor = response.Node.Edits.PageInfo.EndCursor
		if result.NextCursor == "" || result.NextCursor == request.Cursor {
			return result, errors.New("github edit-history pagination did not advance")
		}
	}
	for _, raw := range response.Node.Edits.Nodes {
		var edit struct {
			ID        string     `json:"id"`
			CreatedAt time.Time  `json:"createdAt"`
			UpdatedAt time.Time  `json:"updatedAt"`
			EditedAt  time.Time  `json:"editedAt"`
			DeletedAt *time.Time `json:"deletedAt"`
			Diff      *string    `json:"diff"`
			Editor    struct {
				ID    string `json:"id"`
				Login string `json:"login"`
			} `json:"editor"`
		}
		if err := json.Unmarshal(raw, &edit); err != nil {
			return result, err
		}
		if edit.ID == "" {
			return result, errors.New("github edit-history record lacks source identity")
		}
		if edit.Diff == nil || edit.DeletedAt != nil {
			result.Gaps = append(result.Gaps, "Edit diff was redacted or unavailable: "+edit.ID)
		}
		result.Records = append(result.Records, hubserver.GitHubImportRecord{SourceKey: "edit:" + edit.ID, Kind: "edit", Data: raw, Provenance: tracker.Provenance{Provider: "github", ExternalID: edit.ID, AuthorID: edit.Editor.ID, AuthorDisplayName: edit.Editor.Login, CreatedAt: edit.CreatedAt, UpdatedAt: edit.UpdatedAt, ObservedAt: time.Now().UTC()}})
	}
	return result, nil
}

func (t *Transport) GraphQL(ctx context.Context, query string, variables map[string]any, output any) error {
	client, ok := t.client.(graphQLClient)
	if !ok {
		return errors.New("github GraphQL transport is unavailable")
	}
	_, err := t.request(ctx, "GET", "/graphql/issues/edits", func() (string, error) { return "", client.GraphQL(ctx, query, variables, output) })
	return err
}
