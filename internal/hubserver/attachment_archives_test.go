package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestAttachmentArchiveQuotaAndRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		allowanceDelta int64
		status         int
	}{
		{"exact storage limit", 0, http.StatusCreated},
		{"storage exhausted", -1, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			items := []string{string(f.seedIssue(t, 1)), string(f.seedIssue(t, 2))}
			ref, _, err := attachment.PrepareArchive("org_security", 1, strings.NewReader("archive data"))
			if err != nil {
				t.Fatal(err)
			}
			var before int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&before); err != nil {
				t.Fatal(err)
			}
			hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": before + ref.CompressedSize + test.allowanceDelta, "api_mutations": 1000})
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			for _, item := range items {
				raw, err := json.Marshal(map[string]any{"reference": ref})
				if err != nil {
					t.Fatal(err)
				}
				preflight := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/archives/" + item + "/check", body: string(raw), csrf: csrf})
				want := http.StatusOK
				if test.status != http.StatusCreated {
					want = test.status
				}
				if preflight.Code != want {
					t.Fatalf("preflight=%d %s", preflight.Code, preflight.Body.String())
				}
				raw, err = json.Marshal(ref)
				if err != nil {
					t.Fatal(err)
				}
				response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/archives/" + item, body: string(raw), csrf: csrf})
				if response.Code != test.status {
					t.Fatalf("record=%d %s", response.Code, response.Body.String())
				}
			}
			var used int64
			var count int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE kind='archive'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusCreated {
				if used != before || count != 0 {
					t.Fatal("rejected archive committed bytes or metadata")
				}
				return
			}
			if count != 1 || used-before != ref.CompressedSize {
				t.Fatal("duplicate archive was charged twice")
			}
			ref.SchemaVersion++
			raw, err := json.Marshal(ref)
			if err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/archives/" + items[0], body: string(raw), csrf: csrf})
			if response.Code != http.StatusConflict {
				t.Fatalf("schema mutation=%d %s", response.Code, response.Body.String())
			}
			for _, item := range items {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM issues WHERE native_id=?", item); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.expireNativeRecords(t.Context(), time.Now().Add(365*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE kind='archive' AND deleted_at IS NULL AND object_deleted_at IS NULL").Scan(&count); err != nil || count != 1 {
				t.Fatalf("archive did not outlive its source: count=%d error=%v", count, err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_archive_references").Scan(&count); err != nil || count != 0 {
				t.Fatalf("deleted source left references: count=%d error=%v", count, err)
			}
		})
	}
}
