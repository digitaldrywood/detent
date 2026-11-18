package itemread_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type relationshipPage struct {
	body   string
	cursor map[string]any
	raw    string
}

func decodeRelationshipPage(t *testing.T, data json.RawMessage) relationshipPage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	if raw, ok := fields["next_cursor"]; ok {
		if err := json.Unmarshal(raw, &cursor); err != nil {
			t.Fatal(err)
		}
	}
	delete(fields, "next_cursor")
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	page := relationshipPage{body: string(body)}
	if cursor == "" {
		return page
	}
	payload, _, ok := strings.Cut(cursor, ".")
	if !ok {
		t.Fatalf("cursor %q has no signature separator", cursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode cursor payload: %v", err)
	}
	if err := json.Unmarshal(raw, &page.cursor); err != nil {
		t.Fatalf("cursor payload %s: %v", raw, err)
	}
	page.raw = string(raw)
	return page
}

func requireRelationshipPageMatches(t *testing.T, subject, twin relationshipPage, hidden []string) {
	t.Helper()
	for _, id := range hidden {
		if strings.Contains(subject.raw, id) {
			t.Fatalf("cursor discloses hidden identifier %s: %s", id, subject.raw)
		}
		if strings.Contains(subject.body, id) {
			t.Fatalf("page discloses hidden identifier %s: %s", id, subject.body)
		}
	}
	if subject.body != twin.body {
		t.Fatalf("page differs from the same item without hidden links\nwith:    %s\nwithout: %s", subject.body, twin.body)
	}
	if (subject.cursor == nil) != (twin.cursor == nil) {
		t.Fatalf("continuation differs from the same item without hidden links\nwith:    %s\nwithout: %s", subject.raw, twin.raw)
	}
	left, right := map[string]any{}, map[string]any{}
	for key, value := range subject.cursor {
		left[key] = value
	}
	for key, value := range twin.cursor {
		right[key] = value
	}
	for _, cursor := range []map[string]any{left, right} {
		delete(cursor, "expires")
		delete(cursor, "scope")
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("cursor differs from the same item without hidden links\nwith:    %s\nwithout: %s", subject.raw, twin.raw)
	}
}

func readRelationshipPage(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, token string, item tracker.NativeWorkItemID) relationshipPage {
	t.Helper()
	return decodeRelationshipPage(t, workRead(t, h, p, token, operatortool.WorkRelationships, item))
}

func addRelationship(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, item *tracker.NativeIssue, related tracker.NativeWorkItemID, kind string) {
	t.Helper()
	request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "relate-" + string(item.WorkItemID) + "-" + string(related) + "-" + kind}, ExpectedRevision: item.Revision, RelatedWorkItemID: related, Operation: "add", RelationshipType: kind}
	response := h.Request(t, http.MethodPost, p.Base+"/work-items/"+string(item.WorkItemID)+"/dependencies", h.AdminToken(), request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, item)
}

type relationshipOrganization struct {
	visible, hidden, removed hubfixture.Native
	removedBoundary          tracker.NativeIssue
	hiddenBoundary           tracker.NativeIssue
	twin                     tracker.NativeIssue
	removedIDs, hiddenIDs    []string
}

func relationshipProject(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, order, name string) hubfixture.Native {
	t.Helper()
	return hubfixture.NewNativeWith(t, h, organization, hubtest.ProjectOptions{ID: tracker.ProjectID("prj_" + order + "_" + string(organization)), Name: name, RequireDependencies: true})
}

func seedRelationshipOrganization(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID) relationshipOrganization {
	t.Helper()
	o := relationshipOrganization{
		removed: relationshipProject(t, h, organization, "a_removed", "Example removed relationship project"),
		hidden:  relationshipProject(t, h, organization, "b_hidden", "Example hidden relationship project"),
		visible: relationshipProject(t, h, organization, "c_visible", "Example relationship project"),
	}
	o.removedBoundary = o.visible.Create(t, "Example subject with a removed boundary link")
	o.hiddenBoundary = o.visible.Create(t, "Example subject with a hidden boundary link")
	o.twin = o.visible.Create(t, "Example subject without hidden links")
	subjects := []*tracker.NativeIssue{&o.removedBoundary, &o.hiddenBoundary, &o.twin}
	for index := range operatortool.DefaultItemLimit - 1 {
		dependent := o.visible.Create(t, "Example dependent number "+strconv.Itoa(index))
		for _, subject := range subjects {
			addRelationship(t, h, o.visible, &dependent, subject.WorkItemID, "dependency")
		}
	}
	removedItem := o.removed.Create(t, "Example item in a removed project")
	hiddenItem := o.hidden.Create(t, "Example item in a hidden project")
	addRelationship(t, h, o.visible, &o.removedBoundary, removedItem.WorkItemID, "related")
	addRelationship(t, h, o.visible, &o.hiddenBoundary, hiddenItem.WorkItemID, "related")
	for index := range 2 {
		related := o.visible.Create(t, "Example related item number "+strconv.Itoa(index))
		for _, subject := range subjects {
			addRelationship(t, h, o.visible, subject, related.WorkItemID, "related")
		}
	}
	if _, err := h.Admin.ExecContext(t.Context(), "UPDATE projects SET deleted_at=now() WHERE organization_id=$1 AND id=$2", organization, o.removed.Project.ID); err != nil {
		t.Fatal(err)
	}
	o.removedIDs = []string{string(o.removed.Project.ID), string(removedItem.WorkItemID)}
	o.hiddenIDs = []string{string(o.hidden.Project.ID), string(hiddenItem.WorkItemID)}
	return o
}

func TestRelationshipPageLeavesNoTraceOfHiddenLinks(t *testing.T) {
	t.Parallel()
	h := openWithoutBackgroundPages(t)
	for _, organization := range []tracker.OrganizationID{h.Primary, hubfixture.Secondary} {
		o := seedRelationshipOrganization(t, h, organization)
		cases := []struct {
			name    string
			token   string
			subject tracker.NativeIssue
			hidden  []string
		}{
			{"administrator/removed project at the boundary", h.AdminToken(), o.removedBoundary, o.removedIDs},
			{"project member/removed project at the boundary", o.visible.Token, o.removedBoundary, o.removedIDs},
			{"project member/hidden project at the boundary", o.visible.Token, o.hiddenBoundary, o.hiddenIDs},
		}
		for _, stage := range []string{"source", "stored page"} {
			if stage == "stored page" {
				h.RefreshPages(t, organization)
			}
			for _, test := range cases {
				t.Run(string(organization)+"/"+stage+"/"+test.name, func(t *testing.T) {
					subject := readRelationshipPage(t, h, o.visible, test.token, test.subject.WorkItemID)
					twin := readRelationshipPage(t, h, o.visible, test.token, o.twin.WorkItemID)
					if twin.cursor == nil {
						t.Fatalf("twin page has no continuation; the dataset does not reach the page boundary: %s", twin.body)
					}
					requireRelationshipPageMatches(t, subject, twin, test.hidden)
				})
			}
		}
	}
}
