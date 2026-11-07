package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresBulkExport(t *testing.T) {
	repository, f, owner, expected, _ := newPostgresExportLinkedFixture(t)
	t.Run("export", func(t *testing.T) {
		seen := map[tracker.NativeWorkItemID]bool{}
		cursor := ""
		for range 2 {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export?limit=1&cursor="+url.QueryEscape(cursor), nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page operatortool.WorkBulkExportPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 {
				t.Fatalf("export items=%d", len(page.Items))
			}
			item := page.Items[0]
			if seen[item.WorkItemID] || expected[item.WorkItemID] != item.Title || len(item.Comments) != 1 || item.Comments[0].Body != item.Title+" discussion" {
				t.Fatalf("unexpected export: %+v", item)
			}
			seen[item.WorkItemID] = true
			cursor = page.NextCursor
		}
		if cursor != "" || len(seen) != 2 {
			t.Fatalf("pagination cursor=%q seen=%d", cursor, len(seen))
		}
	})
	t.Run("filters_and_access", func(t *testing.T) {
		for _, test := range []struct {
			query string
			count int
		}{{"state=Todo", 2}, {"state=Done", 0}, {"state=Todo&state=Done", 2}, {"label=missing", 0}, {"open=true", 2}, {"open=false", 0}, {"archived=true", 0}, {"archived=all", 2}} {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export?"+test.query, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page operatortool.WorkBulkExportPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != test.count {
				t.Fatalf("query=%s count=%d want=%d", test.query, len(page.Items), test.count)
			}
		}
		response := f.request(t, owner, http.MethodGet, strings.Replace(f.base, string(f.project), "project_ungranted", 1)+"/work-items/export", nil)
		if response.Code == http.StatusOK {
			t.Fatal("export crossed project scope")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/export", "", nil)
		if response.Code == http.StatusOK {
			t.Fatal("unauthenticated export succeeded")
		}
	})
	t.Run("later_edit", func(t *testing.T) {
		for id := range expected {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(id), nil)
			requireNativeStatus(t, response, http.StatusOK)
			var before tracker.NativeIssue
			decodeHubResponse(t, response, &before)
			body := workItemRecordBody + "\nUpdated export body"
			response = f.request(t, owner, http.MethodPatch, f.base+"/work-items/"+string(id), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-" + string(id)}, ExpectedRevision: before.Revision, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			response = f.request(t, owner, http.MethodGet, f.base+"/work-items/export", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page operatortool.WorkBulkExportPage
			decodeHubResponse(t, response, &page)
			found := false
			for _, item := range page.Items {
				if item.WorkItemID == id {
					found = true
					if item.Body != body || item.Revision <= before.Revision {
						t.Fatal("export failed to expose later edit")
					}
				}
			}
			if !found {
				t.Fatal("edited issue missing from export")
			}
		}
	})
	t.Run("missing_comment_payload", func(t *testing.T) {
		var key string
		for id := range expected {
			if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.Comments(t.Context(), f.project, id, 0, 1)
				if err != nil {
					return err
				}
				version, err := r.CollaborationVersion(t.Context(), rows[0].ID, rows[0].Revision)
				key = version.PayloadObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			break
		}
		store := f.details
		store.mu.Lock()
		payload := store.data[key]
		delete(store.data, key)
		store.mu.Unlock()
		defer func() { store.mu.Lock(); store.data[key] = payload; store.mu.Unlock() }()
		response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export", nil)
		requireNativeCode(t, response, http.StatusInternalServerError, "bulk_export_item_failed")
		if !strings.Contains(response.Body.String(), "work_item_id") || strings.Contains(response.Body.String(), "discussion") {
			t.Fatal("failure lost item identity or leaked discussion")
		}
	})

	t.Run("role_changed_during_hydration", func(t *testing.T) {
		var key string
		for id := range expected {
			if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.Comments(t.Context(), f.project, id, 0, 1)
				if err != nil {
					return err
				}
				version, err := r.CollaborationVersion(t.Context(), rows[0].ID, rows[0].Revision)
				key = version.PayloadObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			break
		}
		original := f.service.config.RunDetails
		defer func() { f.service.config.RunDetails = original }()
		changed := false
		f.service.config.RunDetails = &changeReadInterceptionStore{DetailStore: original, key: key, afterRead: func(ctx context.Context) error {
			return repository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(ctx, owner.identity.Subject)
				if err != nil {
					return err
				}
				member.Role = "reporter"
				changed = true
				return w.UpdateAccessMember(ctx, member)
			})
		}}
		defer func() {
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), owner.identity.Subject)
				if err != nil {
					return err
				}
				member.Role = "owner"
				return w.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Error(err)
			}
		}()
		response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export", nil)
		if !changed {
			t.Fatal("role hook did not run during comment version hydration")
		}
		if response.Code == http.StatusOK {
			t.Fatal("export accepted stale role during object hydration")
		}
	})
	t.Run("large_discussion", func(t *testing.T) {
		for id := range expected {
			for sequence := int64(2); sequence <= 202; sequence++ {
				body := "Stored discussion"
				if sequence == 202 {
					body = strings.Repeat("x", 9<<20)
				}
				seedExportComment(t, repository, f.service, f.project, id, sequence, body)
			}
		}
		seen := map[tracker.NativeWorkItemID]bool{}
		cursor := ""
		for range 2 {
			response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export?cursor="+url.QueryEscape(cursor), nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page operatortool.WorkBulkExportPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || len(response.Body.Bytes()) > operatortool.WorkBulkExportMaxBytes {
				t.Fatalf("page items=%d bytes=%d", len(page.Items), response.Body.Len())
			}
			item := page.Items[0]
			if seen[item.WorkItemID] || len(item.Comments) != 202 || len(item.Comments[201].Body) != 9<<20 {
				t.Fatalf("incomplete discussion for %s", item.WorkItemID)
			}
			seen[item.WorkItemID] = true
			cursor = page.NextCursor
		}
		if cursor != "" || len(seen) != 2 {
			t.Fatal("byte-limited cursor lost an issue")
		}
	})
	t.Run("oversized_item", func(t *testing.T) {
		for id := range expected {
			seedExportComment(t, repository, f.service, f.project, id, 203, strings.Repeat("private", 2<<20))
		}
		response := f.request(t, owner, http.MethodGet, f.base+"/work-items/export", nil)
		requireNativeCode(t, response, http.StatusInternalServerError, "bulk_export_item_failed")
		if strings.Contains(response.Body.String(), "private") || !strings.Contains(response.Body.String(), "work_item_id") {
			t.Fatal("failure leaked content or lost item identity")
		}
	})

}

func seedExportComment(t *testing.T, repository tenantstore.Repository, service *Service, project tracker.ProjectID, item tracker.NativeWorkItemID, sequence int64, body string) {
	t.Helper()
	sequence += 1000
	now := service.config.now()
	comment := tracker.NativeComment{OrganizationID: "org_security", ProjectID: project, WorkItemID: item, ID: fmt.Sprintf("comment_%s_%d", item, sequence), Revision: 1, Sequence: sequence, Body: body, CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(comment)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: comment.OrganizationID, ProjectID: project, WorkItemID: item, RecordID: comment.ID, Revision: 1}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), comment.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: comment.OrganizationID, ProjectID: project, WorkItemID: item, ID: comment.ID, Revision: 1, Sequence: sequence, BodyObjectKey: version.PayloadObjectKey, CreatedAt: now, UpdatedAt: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
