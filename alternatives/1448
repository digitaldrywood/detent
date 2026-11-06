package hubserver

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestPostgresAttachmentExpiryPreservesReferencesAndTenantQuota(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var diagnostics bytes.Buffer
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, config: Config{TenantRepository: repository, Logger: slog.New(slog.NewTextHandler(&diagnostics, nil)), now: func() time.Time { return now }}}
	cases := []struct {
		name                string
		age                 time.Duration
		retention           string
		referenced, expired bool
	}{
		{name: "orphan", age: attachment.OrphanTTL + time.Hour, expired: true},
		{name: "boundary", age: attachment.OrphanTTL, expired: true},
		{name: "fresh", age: attachment.OrphanTTL - time.Microsecond},
		{name: "referenced", age: attachment.OrphanTTL + time.Hour, referenced: true},
		{name: "retained", age: attachment.OrphanTTL + time.Hour, retention: "organization"},
		{name: "new reference", age: attachment.OrphanTTL + time.Hour},
	}
	ids := make([]string, len(cases))
	for i := range ids {
		ids[i] = conversation.NewAttachmentID()
	}
	for _, org := range []tracker.OrganizationID{"org_expiry_first", "org_expiry_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: "Example", Slug: string(org)})
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			for i := range 100 {
				id := fmt.Sprintf("att_%032d", i)
				if err := w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: org, Kind: "attachment", Metadata: attachment.Metadata{ID: id, ProjectID: "project", Uploader: "test_hub_admin", Name: "referenced.png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64), CreatedAt: now.Add(-attachment.OrphanTTL - 2*time.Hour), Width: 1, Height: 1}}); err != nil {
					return err
				}
			}
			for i, tc := range cases {
				if err := w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: org, Kind: "attachment", Retention: tc.retention, Metadata: attachment.Metadata{ID: ids[i], ProjectID: "project", Uploader: "test_hub_admin", Name: tc.name + ".png", ContentType: "image/png", Size: 100, SHA256: strings.Repeat("a", 64), CreatedAt: now.Add(-tc.age), Width: 1, Height: 1}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',$2) ON CONFLICT(organization_id,name) DO UPDATE SET value=excluded.value`, org, 100*(len(cases)+100)); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachment_item_references(organization_id,project_id,attachment_id,work_item_id) VALUES($1,'project',$2,'example-item')`, org, ids[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_expiry_first", "org_expiry_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attachment_item_references(organization_id,project_id,attachment_id,work_item_id) SELECT organization_id,project_id,id,'old-item' FROM attachments WHERE organization_id=$1 AND name='referenced.png'`, org); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org), func(t *testing.T) {
			if org == "org_expiry_second" {
				if _, err := admin.ExecContext(t.Context(), `UPDATE projects SET deleted_at=$2 WHERE organization_id=$1`, org, now); err != nil {
					t.Fatal(err)
				}
			}
			service.config.TenantRepository = repository
			call := func(kind, id string, finish bool, want int) *httptest.ResponseRecorder {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, "/internal/v1/attachments/expired", strings.NewReader(fmt.Sprintf(`{"id":%q}`, id)))
				request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := service.echo.NewContext(request, response)
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: kind, Audience: string(org)})
				var err error
				if finish {
					err = service.finishCloudAttachmentDeletion(c)
				} else {
					err = service.expiredCloudAttachments(c)
				}
				if err != nil {
					t.Fatal(err)
				}
				if response.Code != want {
					t.Log(diagnostics.String())
				}
				requireNativeStatus(t, response, want)
				return response
			}
			call("user", "", false, http.StatusNotFound)
			var expired []attachment.Metadata
			decodeHubResponse(t, call(cloudassert.KindService, "", false, http.StatusOK), &expired)
			if len(expired) != 0 {
				t.Fatalf("first batch exceeded 100 referenced candidates: %d deletions", len(expired))
			}
			referenced := false
			service.config.TenantRepository = &attachmentReadCounter{Repository: repository, beforeWrite: func() error {
				if referenced {
					return nil
				}
				referenced = true
				_, err := admin.ExecContext(t.Context(), `INSERT INTO attachment_item_references(organization_id,project_id,attachment_id,work_item_id) VALUES($1,'project',$2,'new-item')`, org, ids[5])
				return err
			}}
			decodeHubResponse(t, call(cloudassert.KindService, "", false, http.StatusOK), &expired)
			if len(expired) != 2 {
				t.Fatalf("expired %d files, want 2", len(expired))
			}
			for _, record := range expired {
				if record.ID != ids[0] && record.ID != ids[1] {
					t.Fatalf("expired protected attachment %s", record.ID)
				}
				call(cloudassert.KindService, record.ID, true, http.StatusNoContent)
			}
			decodeHubResponse(t, call(cloudassert.KindService, "", false, http.StatusOK), &expired)
			if len(expired) != 0 {
				t.Fatalf("completed deletions returned again: %v", expired)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				used, err := r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				if used != 10400 {
					t.Fatalf("quota after expiry=%d, want 10400", used)
				}
				for i, tc := range cases {
					record, err := r.Attachment(t.Context(), "project", ids[i])
					if err != nil {
						return err
					}
					if (record.Metadata.DeletedAt != nil) != tc.expired || (record.ObjectDeletedAt != nil) != tc.expired {
						t.Errorf("%s deletion state differs", tc.name)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
