package accessrecords_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentReferenceBound(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, admin)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			pgstoretest.SeedTenantAccess(t, repository, organization)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES ($1,'prj_shared','attachment_shared','first','example.txt','text/plain',1,$2,$3)`, organization, strings.Repeat("a", 64), pgstoretest.HistoryAt()); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) SELECT $1,'comment_' || n,'prj_shared','wi_shared',n+1,$1 || '/body/' || n,'human','second','source_' || n FROM generate_series(1,201) AS n`, organization); err != nil {
				t.Fatal(err)
			}
			bind := func(comment string, retain bool) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "attachment_shared", WorkItemID: "wi_shared", CommentID: comment, RetainWithWorkItem: retain})
				})
			}
			for n := 1; n <= 200; n++ {
				if err := bind(fmt.Sprintf("comment_%d", n), false); err != nil {
					t.Fatalf("reference %d: %v", n, err)
				}
			}
			for _, tc := range []struct {
				name, comment string
				retain        bool
				want          error
			}{
				{name: "reject 201st comment", comment: "comment_201", want: attachment.ErrInvalid},
				{name: "retry existing reference", comment: "comment_1"},
				{name: "retain existing reference", comment: "comment_1", retain: true},
				{name: "reject additional issue reference", want: attachment.ErrInvalid},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := bind(tc.comment, tc.retain); !errors.Is(err, tc.want) {
						t.Errorf("bind error = %v, want %v", err, tc.want)
					}
				})
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				refs, err := r.AttachmentReferences(t.Context(), "prj_shared", "attachment_shared", 201)
				if err != nil {
					return err
				}
				if len(refs) != 200 {
					t.Errorf("attachment has %d references; metadata read bound is 200", len(refs))
				}
				for _, ref := range refs {
					if ref.CommentID == "comment_1" && !ref.RetainWithWorkItem {
						t.Error("existing reference retention was not updated")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAttachmentObjectLookupProtectsTenantHistory(t *testing.T) {
	r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	keys := map[tracker.OrganizationID]string{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, r, org)
		f := pgstoretest.SeedHistory(t, r, org, "lookup")
		keys[org] = f.Event.PayloadObjectKey
	}
	for org, key := range keys {
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			for other, candidate := range keys {
				exists, err := reader.AttachmentObjectExists(t.Context(), "", candidate)
				if err != nil {
					return err
				}
				if exists != (other == org) {
					t.Errorf("history object ownership: tenant=%s object tenant=%s exists=%v", org, other, exists)
				}
			}
			orphan := strings.Replace(key, key[len(key)-64:], strings.Repeat("0", 64), 1)
			exists, err := reader.AttachmentObjectExists(t.Context(), "", orphan)
			if exists {
				t.Error("unreferenced object retained")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttachmentReferencesFollowTheirSourceText(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	pgstoretest.SeedCore(t, admin)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			pgstoretest.SeedTenantAccess(t, repository, organization)
			for _, id := range []string{"att_edit", "att_shared", "att_retained", "att_late"} {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES ($1,'prj_shared',$2,'first','example.png','image/png',1,$3,$4)`, organization, id, strings.Repeat("a", 64), pgstoretest.HistoryAt()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_comments (organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,source_key) VALUES ($1,'comment_peer','prj_shared','wi_shared',900,$1 || '/body/peer','human','second','source_peer')`, organization); err != nil {
				t.Fatal(err)
			}
			write := func(update func(w tenantstore.Writer) error) error {
				return repository.WriteTenant(t.Context(), organization, corestore.Serving, update)
			}
			replace := func(previous, current []string) error {
				return write(func(w tenantstore.Writer) error {
					return w.ReplaceAttachmentReferences(t.Context(), "prj_shared", "wi_shared", "", previous, current)
				})
			}
			bound := func(id string) []string {
				t.Helper()
				var sources []string
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					refs, err := r.AttachmentReferences(t.Context(), "prj_shared", id, 10)
					for _, ref := range refs {
						sources = append(sources, fmt.Sprintf("%s/%s/%t", ref.WorkItemID, ref.CommentID, ref.RetainWithWorkItem))
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return sources
			}
			if err := write(func(w tenantstore.Writer) error {
				if err := w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "att_shared", WorkItemID: "wi_shared", CommentID: "comment_peer"}); err != nil {
					return err
				}
				return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: "prj_shared", AttachmentID: "att_retained", WorkItemID: "wi_shared", RetainWithWorkItem: true})
			}); err != nil {
				t.Fatal(err)
			}
			for _, step := range []struct {
				name              string
				previous, current []string
				deleteFirst       string
				wantErr           error
				check             string
				want              []string
			}{
				{name: "edit adds a readable attachment", current: []string{"att_edit", "att_edit"}, check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "replayed edit keeps one binding", previous: []string{"att_edit"}, current: []string{"att_edit"}, check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "unchanged reference survives deletion without new access", previous: []string{"att_edit"}, current: []string{"att_edit"}, deleteFirst: "att_edit", check: "att_edit", want: []string{"wi_shared//false"}},
				{name: "removal unbinds only this source", previous: []string{"att_shared"}, current: nil, check: "att_shared", want: []string{"wi_shared/comment_peer/false"}},
				{name: "removal keeps explicit retention", previous: []string{"att_retained"}, current: nil, check: "att_retained", want: []string{"wi_shared//true"}},
				{name: "removing a deleted reference unbinds it", previous: []string{"att_edit"}, current: nil, check: "att_edit", want: nil},
				{name: "re-adding after deletion is refused", current: []string{"att_edit"}, wantErr: corestore.ErrNotFound, check: "att_edit", want: nil},
				{name: "deletion between preparation and commit is refused", current: []string{"att_late"}, deleteFirst: "att_late", wantErr: corestore.ErrNotFound, check: "att_late", want: nil},
			} {
				if step.deleteFirst != "" {
					if _, err := admin.ExecContext(t.Context(), `UPDATE attachments SET deleted_at=$3 WHERE organization_id=$1 AND project_id='prj_shared' AND id=$2`, organization, step.deleteFirst, pgstoretest.HistoryAt()); err != nil {
						t.Fatal(err)
					}
				}
				if err := replace(step.previous, step.current); !errors.Is(err, step.wantErr) {
					t.Fatalf("%s: error=%v want %v", step.name, err, step.wantErr)
				}
				if got := bound(step.check); fmt.Sprint(got) != fmt.Sprint(step.want) {
					t.Fatalf("%s: bindings=%v want %v", step.name, got, step.want)
				}
			}
			var deleted int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM attachments WHERE organization_id=$1 AND id IN ('att_shared','att_retained') AND deleted_at IS NOT NULL`, organization).Scan(&deleted); err != nil || deleted != 0 {
				t.Fatalf("unbinding deleted attachment rows=%d error=%v", deleted, err)
			}
		})
	}
}
