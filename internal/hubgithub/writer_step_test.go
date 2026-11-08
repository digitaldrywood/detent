package hubgithub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type pagedOutboundClient struct{ reads, writes int }

func (c *pagedOutboundClient) RESTPage(_ context.Context, _ string, output any) (string, error) {
	c.reads++
	page := []restComment{{ID: int64(c.reads), Body: "Example comment"}}
	if c.reads == 3 {
		page[0].Body = "<!-- example-workpad -->"
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, output); err != nil {
		return "", err
	}
	if c.reads < 3 {
		return fmt.Sprintf("/repos/example-org/example-project/issues/17/comments?per_page=100&page=%d", c.reads+1), nil
	}
	return "", nil
}

func (c *pagedOutboundClient) REST(_ context.Context, method, _ string, _ any, _ any) error {
	if method != http.MethodPatch {
		return fmt.Errorf("unexpected write method %s", method)
	}
	c.writes++
	return nil
}

func runOutboundWriterStep(ctx context.Context, writer *Writer, item hubserver.OutboxItem) (hubserver.OutboxStepResult, error) {
	if stepped, ok := any(writer).(hubserver.OutboxStepBackend); ok {
		return stepped.Step(ctx, item)
	}
	err := writer.Execute(ctx, item)
	return hubserver.OutboxStepResult{Done: err == nil}, err
}

func TestWriterBoundsOutboundCommentPages(t *testing.T) {
	client := &pagedOutboundClient{}
	writer := NewWriter(client)
	item := testOutboxItem(t, hubserver.MutationWorkpad, hubserver.WorkpadDesired{Body: "Example body", Marker: "<!-- example-workpad -->"})
	item.OrganizationID, item.RepositoryOwner, item.RepositoryName, item.IdempotencyKey = "org", "example-org", "example-project", "example-command"
	item.BeforeWrite = func(context.Context) error { return nil }
	for page := 1; page <= 3; page++ {
		result, err := runOutboundWriterStep(t.Context(), writer, item)
		if err != nil {
			t.Fatal(err)
		}
		if client.reads != page {
			t.Fatalf("one step read %d pages; expected total %d", client.reads, page)
		}
		if result.Done != (page == 3) || client.writes != map[bool]int{false: 0, true: 1}[page == 3] {
			t.Fatalf("page=%d done=%t writes=%d", page, result.Done, client.writes)
		}
		item.Progress = result.Progress
	}
}
