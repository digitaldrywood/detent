package itemread_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func openWithoutBackgroundPages(t *testing.T) *hubfixture.Hub {
	t.Helper()
	return hubfixture.OpenDeps(t, hubfixture.Config{}, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
}

type itemReadOrganization struct {
	visible, private hubfixture.Native
	subject, blocker tracker.NativeIssue
	dependent        tracker.NativeIssue
	hiddenOnly       tracker.NativeIssue
	plain, related   tracker.NativeIssue
	linked           tracker.NativeIssue
	committed        tracker.NativeIssue
}

func link(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, item tracker.NativeIssue, related tracker.NativeWorkItemID, kind string) tracker.NativeIssue {
	t.Helper()
	path := p.Base + "/work-items/" + string(item.WorkItemID)
	var current tracker.NativeIssue
	response := h.Request(t, http.MethodGet, path, h.AdminToken(), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &current)
	request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "link-" + string(item.WorkItemID) + "-" + string(related) + "-" + kind}, ExpectedRevision: current.Revision, RelatedWorkItemID: related, Operation: "add", RelationshipType: kind}
	response = h.Request(t, http.MethodPost, path+"/dependencies", h.AdminToken(), request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var committed tracker.NativeIssue
	hubfixture.Decode(t, response, &committed)
	return committed
}

func seedItemReadOrganization(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID) itemReadOrganization {
	t.Helper()
	o := itemReadOrganization{visible: hubfixture.NewNative(t, h, organization, "Example visible project"), private: hubfixture.NewNative(t, h, organization, "Example private project")}
	o.subject = o.visible.Create(t, "Example subject")
	o.blocker = o.visible.Create(t, "Example visible blocker")
	o.dependent = o.visible.Create(t, "Example dependent")
	o.hiddenOnly = o.visible.Create(t, "Example hidden-only blocked item")
	o.plain = o.visible.Create(t, "Example unlinked item")
	o.related = o.visible.Create(t, "Example related item")
	hidden := o.private.Create(t, "Example private blocker")
	link(t, h, o.visible, o.subject, o.blocker.WorkItemID, "dependency")
	link(t, h, o.visible, o.subject, hidden.WorkItemID, "dependency")
	o.committed = link(t, h, o.visible, o.subject, o.related.WorkItemID, "related")
	link(t, h, o.visible, o.dependent, o.subject.WorkItemID, "dependency")
	link(t, h, o.visible, o.hiddenOnly, hidden.WorkItemID, "dependency")
	comment := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-" + string(o.subject.WorkItemID)}, Body: "Example discussion."}
	hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, o.visible.Base+"/work-items/"+string(o.subject.WorkItemID)+"/comments", h.AdminToken(), comment), http.StatusOK)
	o.linked = o.visible.Create(t, "Example linked item")
	if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateLinkedSource(t.Context(), eventstore.LinkedSource{OrganizationID: organization, ProjectID: o.visible.Project.ID, WorkItemID: o.linked.WorkItemID, URL: "https://github.com/example-org/example-project/issues/1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return o
}

func readItem(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, token string, item tracker.NativeWorkItemID) []byte {
	t.Helper()
	response := h.Request(t, http.MethodGet, p.Base+"/work-items/"+string(item), token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	return response.Body.Bytes()
}

func pageReady(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, item tracker.NativeWorkItemID, kind string) bool {
	t.Helper()
	var ready bool
	if err := h.Admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM native_work_item_page_progress WHERE organization_id=$1 AND backfill_phase=2)
 AND NOT EXISTS(SELECT 1 FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind=$4 AND NOT cleaning)
 AND CASE WHEN $4='item' THEN EXISTS(SELECT 1 FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)
 WHEN $4='relationships' THEN EXISTS(SELECT 1 FROM native_relationship_page_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)
 ELSE true END`, p.Project.OrganizationID, p.Project.ID, item, kind).Scan(&ready); err != nil {
		t.Fatal(err)
	}
	return ready
}

func workRead(t *testing.T, h *hubfixture.Hub, p hubfixture.Native, token, tool string, item tracker.NativeWorkItemID) []byte {
	t.Helper()
	reply := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(p.Project.OrganizationID), token)("tools/call", tool, map[string]any{"project_id": string(p.Project.ID), "reference": string(item)})
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(hubfixture.MCPData(t, reply, false), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func differingFields(t *testing.T, before, after []byte) []string {
	t.Helper()
	var left, right map[string]any
	if err := json.Unmarshal(before, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &right); err != nil {
		t.Fatal(err)
	}
	fields := []string{}
	for key, value := range left {
		if !reflect.DeepEqual(value, right[key]) {
			fields = append(fields, key)
		}
	}
	for key := range right {
		if _, ok := left[key]; !ok {
			fields = append(fields, key)
		}
	}
	slices.Sort(fields)
	return fields
}

func relationshipView(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	view := map[string]any{}
	for _, key := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason", "omitted_fields"} {
		view[key] = body[key]
	}
	return view
}

func TestWorkItemReadIsTheSameBeforeAndAfterPageRefresh(t *testing.T) {
	t.Parallel()
	h := openWithoutBackgroundPages(t)
	organizations := []tracker.OrganizationID{h.Primary, h.CreateOrganization(t, "Example second organization")}
	for _, organization := range organizations {
		o := seedItemReadOrganization(t, h, organization)
		viewers := []struct {
			name, token string
		}{
			{"administrator", h.AdminToken()},
			{"project member without the private project", o.visible.Token},
		}
		items := []struct {
			name string
			item tracker.NativeIssue
		}{
			{"subject with visible and hidden blockers", o.subject},
			{"dependent of subject", o.dependent},
			{"blocked only by a hidden item", o.hiddenOnly},
			{"related item", o.related},
			{"unlinked item", o.plain},
			{"item linked to a GitHub source", o.linked},
		}
		reads := []struct {
			name string
			read func(*testing.T, string, tracker.NativeWorkItemID) []byte
		}{
			{"GET work item", func(t *testing.T, token string, item tracker.NativeWorkItemID) []byte {
				return readItem(t, h, o.visible, token, item)
			}},
			{"work_item", func(t *testing.T, token string, item tracker.NativeWorkItemID) []byte {
				return workRead(t, h, o.visible, token, "work_item", item)
			}},
			{"work_export", func(t *testing.T, token string, item tracker.NativeWorkItemID) []byte {
				return workRead(t, h, o.visible, token, "work_export", item)
			}},
			{"work_relationships", func(t *testing.T, token string, item tracker.NativeWorkItemID) []byte {
				return workRead(t, h, o.visible, token, "work_relationships", item)
			}},
			{"work_comments", func(t *testing.T, token string, item tracker.NativeWorkItemID) []byte {
				return workRead(t, h, o.visible, token, "work_comments", item)
			}},
		}
		if organization != h.Primary {
			reads = reads[:1]
		}
		kinds := []string{"item", "relationships", "comment"}
		before := map[string][]byte{}
		for _, item := range items {
			for _, kind := range kinds {
				if pageReady(t, h, o.visible, item.item.WorkItemID, kind) {
					t.Fatalf("%s %s: %s page is ready before refresh; the read would not exercise the pending path", organization, item.name, kind)
				}
			}
			for _, viewer := range viewers {
				for _, read := range reads {
					before[item.name+"/"+viewer.name+"/"+read.name] = read.read(t, viewer.token, item.item.WorkItemID)
				}
			}
		}
		h.RefreshPages(t, organization)
		for _, item := range items {
			for _, kind := range kinds {
				if !pageReady(t, h, o.visible, item.item.WorkItemID, kind) {
					t.Fatalf("%s %s: %s page is not ready after refresh; the read would not exercise the stored page", organization, item.name, kind)
				}
			}
			for _, viewer := range viewers {
				for _, read := range reads {
					t.Run(string(organization)+"/"+item.name+"/"+viewer.name+"/"+read.name, func(t *testing.T) {
						after := read.read(t, viewer.token, item.item.WorkItemID)
						first := before[item.name+"/"+viewer.name+"/"+read.name]
						if string(first) != string(after) {
							t.Fatalf("read changed when its page view was refreshed; fields %v\npending: %s\nstored:  %s", differingFields(t, first, after), first, after)
						}
						if read.name == "GET work item" && item.item.WorkItemID == o.subject.WorkItemID {
							var value tracker.NativeIssue
							if err := json.Unmarshal(after, &value); err != nil {
								t.Fatal(err)
							}
							if !value.UpdatedAt.Equal(o.committed.UpdatedAt) || value.Revision != o.committed.Revision {
								t.Fatalf("item read updated_at=%s revision=%d, committed representation updated_at=%s revision=%d", value.UpdatedAt, value.Revision, o.committed.UpdatedAt, o.committed.Revision)
							}
						}
					})
				}
			}
		}
		t.Run(string(organization)+"/hidden blocker leaves no trace", func(t *testing.T) {
			hiddenOnly := relationshipView(t, readItem(t, h, o.visible, o.visible.Token, o.hiddenOnly.WorkItemID))
			unlinked := relationshipView(t, readItem(t, h, o.visible, o.visible.Token, o.plain.WorkItemID))
			if !reflect.DeepEqual(hiddenOnly, unlinked) {
				t.Fatalf("an item whose only blocker is hidden differs from an unlinked item:\nhidden-only: %v\nunlinked:    %v", hiddenOnly, unlinked)
			}
		})
	}
}
