package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentReferenceBound(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			seedTenantAccess(t, repository, organization)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachments (organization_id,project_id,id,uploader,name,content_type,size,sha256,created_at) VALUES ($1,'prj_shared','attachment_shared','first','example.txt','text/plain',1,$2,$3)`, organization, strings.Repeat("a", 64), historyAt()); err != nil {
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
	r, _ := repositoryFor(t, template.Database(t), smallPool())
	keys := map[tracker.OrganizationID]string{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, r, org)
		f := seedHistory(t, r, org, "lookup")
		keys[org] = f.event.PayloadObjectKey
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
