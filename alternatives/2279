package itemread_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type relationshipLinks struct {
	Related []struct {
		ID tracker.NativeWorkItemID `json:"work_item_id"`
	} `json:"related"`
	NextCursor string `json:"next_cursor"`
}

func relationshipCall(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, token string, item tracker.NativeWorkItemID, cursor string) hubfixture.MCPReply {
	t.Helper()
	arguments := map[string]any{"project_id": string(p.Project.ID), "reference": string(item)}
	if cursor != "" {
		arguments["cursor"] = cursor
	}
	return hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(p.Project.OrganizationID), token)("tools/call", operatortool.WorkRelationships, arguments)
}

func mcpRelationshipLinks(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, token string, item tracker.NativeWorkItemID, cursor string) relationshipLinks {
	t.Helper()
	var envelope struct {
		Data relationshipLinks `json:"data"`
	}
	if err := json.Unmarshal(hubfixture.MCPData(t, relationshipCall(t, h, p, token, item, cursor), false), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func walkRelationshipLinks(t *testing.T, read func(cursor string) relationshipLinks) ([]tracker.NativeWorkItemID, int, string) {
	t.Helper()
	var ids []tracker.NativeWorkItemID
	cursor, first, pages := "", "", 0
	for {
		page := read(cursor)
		pages++
		for _, link := range page.Related {
			ids = append(ids, link.ID)
		}
		if pages == 1 {
			first = page.NextCursor
		}
		if page.NextCursor == "" {
			return ids, pages, first
		}
		if page.NextCursor == cursor || pages > 10 {
			t.Fatalf("relationship pages do not advance after %d pages", pages)
		}
		cursor = page.NextCursor
	}
}

func TestRelationshipCursorPagesOverMCP(t *testing.T) {
	t.Parallel()
	h := openWithoutBackgroundPages(t)
	p := hubfixture.NewNativeWith(t, h, h.Primary, hubtest.ProjectOptions{Name: "Example relationship cursor project", RequireDependencies: true})
	colleague := hubtest.Credential(t, h.Repository, h.Primary, hubtest.CredentialOptions{Projects: []tracker.ProjectID{p.Project.ID}, Now: h.Now().UTC()})
	subject := p.Create(t, "Example subject with many links")
	sibling := p.Create(t, "Example sibling")
	want := []tracker.NativeWorkItemID{}
	for index := range operatortool.DefaultItemLimit + 25 {
		related := p.Create(t, "Example related item number "+strconv.Itoa(index))
		addRelationship(t, h, p, &subject, related.WorkItemID, "related")
		want = append(want, related.WorkItemID)
	}
	slices.Sort(want)

	viaMCP, pages, first := walkRelationshipLinks(t, func(cursor string) relationshipLinks {
		return mcpRelationshipLinks(t, h, p, p.Token, subject.WorkItemID, cursor)
	})
	if pages != 2 || first == "" {
		t.Fatalf("pages=%d first cursor=%q, want two pages past %d links", pages, first, operatortool.DefaultItemLimit)
	}
	if got := slices.Sorted(slices.Values(viaMCP)); !slices.Equal(got, want) {
		t.Fatalf("MCP pages returned %d links, want every one of %d exactly once", len(viaMCP), len(want))
	}

	for _, refused := range []struct {
		name  string
		token string
		item  tracker.NativeWorkItemID
	}{
		{"another viewer", colleague.Token, subject.WorkItemID},
		{"administrator", h.AdminToken(), subject.WorkItemID},
		{"another item", p.Token, sibling.WorkItemID},
	} {
		t.Run(refused.name, func(t *testing.T) {
			hubfixture.MCPData(t, relationshipCall(t, h, p, refused.token, refused.item, first), true)
		})
	}
}
