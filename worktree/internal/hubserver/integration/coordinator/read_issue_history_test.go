package coordinator_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type relationshipHistory struct {
	Data struct {
		Related []struct {
			ID tracker.NativeWorkItemID `json:"work_item_id"`
		} `json:"related"`
		NextCursor string `json:"next_cursor"`
	} `json:"data"`
}

func decodeRelationshipHistory(t *testing.T, result runner.AgentToolResult) relationshipHistory {
	t.Helper()
	if !result.Success {
		t.Fatalf("read_issue_history failed: %s", result.Content)
	}
	var page relationshipHistory
	if err := json.Unmarshal([]byte(result.Content), &page); err != nil {
		t.Fatalf("decode %s: %v", result.Content, err)
	}
	return page
}

func TestReadIssueHistoryFollowsRelationshipCursor(t *testing.T) {
	t.Parallel()
	backend := newToolBackend()
	h := backend.open(t)
	p := hubfixture.NewNativeWith(t, h, h.Primary, hubtest.ProjectOptions{Name: "Example project", RequireDependencies: true})
	subject := p.Create(t, "Example subject with many links")
	want := []tracker.NativeWorkItemID{}
	for index := range operatortool.DefaultItemLimit + 5 {
		related := p.Create(t, "Example related item number "+strconv.Itoa(index))
		request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "relate-" + string(related.WorkItemID)}, ExpectedRevision: subject.Revision, RelatedWorkItemID: related.WorkItemID, Operation: "add", RelationshipType: "related"}
		response := h.Request(t, http.MethodPost, p.Base+"/work-items/"+string(subject.WorkItemID)+"/dependencies", h.AdminToken(), request)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		hubfixture.Decode(t, response, &subject)
		want = append(want, related.WorkItemID)
	}
	slices.Sort(want)

	arguments := func(cursor string) string {
		raw, err := json.Marshal(map[string]string{"work_item_id": string(subject.WorkItemID), "section": operatortool.WorkRelationships, "cursor": cursor})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	chat, firstResult := startConversation(t, backend, h, p, "read_issue_history", `{"work_item_id":"`+string(subject.WorkItemID)+`","section":"work_relationships"}`)
	first := decodeRelationshipHistory(t, firstResult)
	if first.Data.NextCursor == "" || len(first.Data.Related) != operatortool.DefaultItemLimit {
		t.Fatalf("first page has %d links and cursor %q, want %d and a continuation", len(first.Data.Related), first.Data.NextCursor, operatortool.DefaultItemLimit)
	}
	second := decodeRelationshipHistory(t, chat.ask(t, "read_issue_history", arguments(first.Data.NextCursor)))
	if second.Data.NextCursor != "" {
		t.Fatalf("second page continues past %d links: %q", len(want), second.Data.NextCursor)
	}
	var got []tracker.NativeWorkItemID
	for _, page := range []relationshipHistory{first, second} {
		for _, link := range page.Data.Related {
			got = append(got, link.ID)
		}
	}
	if slices.Sort(got); !slices.Equal(got, want) {
		t.Fatalf("pages returned %d links, want every one of %d exactly once", len(got), len(want))
	}
}
