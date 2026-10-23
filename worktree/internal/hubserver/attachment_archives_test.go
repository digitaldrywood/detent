package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestIssueArchiveViews(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	limited := f.user(t, "limited", "member", "limited@example.test", "", "")
	reporter := f.user(t, "reporter", "reporter", "reporter@example.test", "read", "")
	var reporterPrincipal string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", reporter.identity.Subject).Scan(&reporterPrincipal); err != nil {
		t.Fatal(err)
	}
	ref, _, err := attachment.PrepareArchive("org_security", 1, strings.NewReader(`{"schema_version":1,"section_id":"issue","data":{"body":"Archived body"}}`))
	if err != nil {
		t.Fatal(err)
	}
	csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
	for number := 1; number <= 2; number++ {
		item := string(f.seedIssue(t, number))
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_json=json_object('kind','human','principal_id',?) WHERE native_id=?", reporterPrincipal, item); err != nil {
			t.Fatal(err)
		}
		base := f.base + "/attachment-metadata/archives/" + item
		raw, err := json.Marshal(map[string]any{"reference": ref})
		if err != nil {
			t.Fatal(err)
		}
		preflight := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: base + "/check", body: string(raw), csrf: csrf})
		if preflight.Code != http.StatusOK {
			t.Fatalf("preflight: %d %s", preflight.Code, preflight.Body.String())
		}
		raw, err = json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		stored := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: base, body: string(raw), csrf: csrf})
		if stored.Code != http.StatusCreated {
			t.Fatalf("store: %d %s", stored.Code, stored.Body.String())
		}
		_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_archive_sections(organization_id,project_id,work_item_id,archive_revision,section_id,attachment_id) SELECT organization_id,project_id,work_item_id,?, ?,attachment_id FROM attachment_archive_references WHERE organization_id=? AND project_id=? AND work_item_id=?`, 1, "issue", "org_security", f.project, item)
		if err != nil {
			t.Fatal(err)
		}
		path := f.base + "/work-items/" + item
		response := f.serve(t, hostedSharedRequest{user: &owner, target: path, headers: map[string]string{"X-Detent-Archive-Read": "1"}})
		if response.Code != http.StatusOK {
			t.Fatalf("detail: %d %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Archive issueArchiveSection `json:"archive"`
			Current struct {
				Body string `json:"body"`
			} `json:"current"`
			Projection string `json:"projection"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Archive.WorkItemID != item || envelope.Archive.SHA256 != ref.SHA256 || envelope.Archive.Revision != 1 || envelope.Current.Body != "" || envelope.Projection != "issue" {
			t.Fatalf("unexpected descriptor: %s", response.Body.String())
		}
		for _, test := range []struct {
			name, suffix string
			user         *hostedSecurityUser
			status       int
		}{
			{"current section", "?section=issue&revision=1", &owner, http.StatusOK},
			{"superseded revision", "?section=issue&revision=2", &owner, http.StatusNotFound},
			{"missing section", "?section=attempts&revision=1", &owner, http.StatusNotFound},
			{"no section bypass", "", &owner, http.StatusNotFound},
			{"limited project grant", "?section=issue&revision=1", &limited, http.StatusNotFound},
			{"reporter private archive", "?section=issue&revision=1", &reporter, http.StatusNotFound},
		} {
			t.Run(test.name+item, func(t *testing.T) {
				read := f.serve(t, hostedSharedRequest{user: test.user, target: base + "/" + ref.SHA256 + test.suffix})
				if read.Code != test.status {
					t.Fatalf("read: %d %s", read.Code, read.Body.String())
				}
			})
		}
		report := f.serve(t, hostedSharedRequest{user: &reporter, target: path, headers: map[string]string{"X-Detent-Archive-Read": "1"}})
		if report.Code != http.StatusOK || strings.Contains(report.Body.String(), "access_path") || !strings.Contains(report.Body.String(), "private-body-sentinel") {
			t.Fatalf("reporter detail: %d %s", report.Code, report.Body.String())
		}
		access, err := url.Parse(envelope.Archive.AccessPath)
		if err != nil || access.Query().Get("section") != "issue" || access.Query().Get("revision") != "1" {
			t.Fatalf("invalid access path: %q", envelope.Archive.AccessPath)
		}
		missing := f.serve(t, hostedSharedRequest{user: &owner, target: path + "/attempts", headers: map[string]string{"X-Detent-Archive-Read": "1"}})
		if missing.Code != http.StatusServiceUnavailable || !strings.Contains(missing.Body.String(), "archive_unavailable") {
			t.Fatalf("missing history: %d %s", missing.Code, missing.Body.String())
		}
	}
}

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
				wantRecord := test.status
				if test.status != http.StatusCreated {
					wantRecord = http.StatusNotFound
				}
				if response.Code != wantRecord {
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

func TestAttachmentArchiveConcurrentReservations(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{false, true} {
		name := "distinct blobs compete for one allocation"
		if shared {
			name = "shared blob survives failed publication"
		}
		t.Run(name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			items := []string{string(f.seedIssue(t, 1)), string(f.seedIssue(t, 2))}
			refs := make([]attachment.ArchiveReference, len(items))
			for index := range refs {
				content := "first archive bytes"
				if index == 1 && !shared {
					content = "second archive bytes"
				}
				ref, _, err := attachment.PrepareArchive("org_security", 1, strings.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				refs[index] = ref
			}
			var before int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&before); err != nil {
				t.Fatal(err)
			}
			hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": before + max(refs[0].CompressedSize, refs[1].CompressedSize), "api_mutations": 1000})
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			requests := make([]*http.Request, len(items))
			responses := make([]*httptest.ResponseRecorder, len(items))
			for index, item := range items {
				raw, err := json.Marshal(map[string]attachment.ArchiveReference{"reference": refs[index]})
				if err != nil {
					t.Fatal(err)
				}
				target := f.base + "/attachment-metadata/archives/" + item + "/check"
				request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(raw)).WithContext(t.Context())
				claims := f.claims(&owner, cloudassert.KindBrowser, http.MethodPost, target, raw)
				assertion, err := cloudassert.Sign(f.key, claims)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set(cloudassert.Header, assertion)
				request.Header.Set("X-CSRF-Token", csrf)
				request.Header.Set("Content-Type", "application/json")
				requests[index], responses[index] = request, httptest.NewRecorder()
			}
			handler := f.service.Handler()
			start := make(chan struct{})
			var done sync.WaitGroup
			for index := range requests {
				done.Go(func() {
					<-start
					handler.ServeHTTP(responses[index], requests[index])
				})
			}
			close(start)
			done.Wait()
			accepted := 0
			var charged int64
			for index, response := range responses {
				switch response.Code {
				case http.StatusOK:
					accepted++
					charged = refs[index].CompressedSize
				case http.StatusTooManyRequests:
					if shared {
						t.Fatal("shared reservation was charged twice")
					}
				default:
					t.Fatalf("reservation=%d %s", response.Code, response.Body.String())
				}
			}
			wantAccepted := 1
			if shared {
				wantAccepted = 2
			}
			if accepted != wantAccepted {
				t.Fatalf("accepted %d uploads into one blob budget, want %d", accepted, wantAccepted)
			}
			var used int64
			var count int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil || used-before != charged {
				t.Fatalf("reserved bytes=%d want=%d error=%v", used-before, charged, err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_archive_references").Scan(&count); err != nil || count != 0 {
				t.Fatalf("reservation published references: count=%d error=%v", count, err)
			}
			var afterDeletion int64
			for index, item := range items {
				read := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/archives/" + item + "/" + refs[index].SHA256})
				if read.Code != http.StatusNotFound {
					t.Fatal("unverified reservation is readable")
				}
				if shared && index == 1 {
					if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM issues WHERE native_id=?", item); err != nil {
						t.Fatal(err)
					}
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&afterDeletion); err != nil {
						t.Fatal(err)
					}
				}
				raw, err := json.Marshal(refs[index])
				if err != nil {
					t.Fatal(err)
				}
				publication := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/archives/" + item, body: string(raw), csrf: csrf})
				want := http.StatusCreated
				if responses[index].Code != http.StatusOK || shared && index == 1 {
					want = http.StatusNotFound
				}
				if publication.Code != want {
					t.Fatalf("publication=%d want=%d %s", publication.Code, want, publication.Body.String())
				}
			}
			if shared {
				read := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/archives/" + items[0] + "/" + refs[0].SHA256})
				if read.Code != http.StatusOK {
					t.Fatalf("failed publication invalidated shared archive: %d %s", read.Code, read.Body.String())
				}
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil || used != afterDeletion {
					t.Fatalf("failed publication lost archive charge: used=%d error=%v", used, err)
				}
			}
		})
	}
}
