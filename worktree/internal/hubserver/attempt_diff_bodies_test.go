package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func diffBodyRequest(t *testing.T, f *attemptDiffFixture, handler echo.HandlerFunc, payload any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/diff-bodies", bytes.NewReader(raw)).WithContext(t.Context())
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	c := f.service.echo.NewContext(request, response)
	c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: cloudassert.KindService, Audience: string(f.project.OrganizationID)})
	if err := handler(c); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestAttemptDiffBodyMigrationAndRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                           string
		currentChange, running, latest bool
		expired                        bool
	}{
		{name: "superseded", expired: true},
		{name: "current Change", currentChange: true},
		{name: "running", running: true},
		{name: "latest work item", latest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAttemptDiffFixture(t)
			response := f.post(t, f.request(2, tracker.AttemptDiffFile{Path: "legacy.go", Status: tracker.DiffStatusModified, Additions: 7, Deletions: 3, Patch: "@@ legacy\n+preserved\n"}))
			requireNativeStatus(t, response, http.StatusAccepted)
			var receipt tracker.AttemptDiffReceipt
			decodeHubResponse(t, response, &receipt)
			if !test.latest {
				requireNativeStatus(t, f.post(t, f.request(3, tracker.AttemptDiffFile{Path: "new.go", Status: tracker.DiffStatusAdded, Patch: "+new"})), http.StatusAccepted)
			}
			attempt := f.attempt
			if !test.running {
				attempt = "attempt_old"
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE attempt_diffs SET attempt_id=?,created_at=? WHERE id=?`, attempt, formatHubTime(time.Now().Add(-40*24*time.Hour)), receipt.DiffID); err != nil {
				t.Fatal(err)
			}
			if test.currentChange {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES ('change_current',?,?,?,'{"current_version_id":"version_current"}')`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json) VALUES ('version_current','change_current',1,'{"attempt_id":"attempt_old"}')`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE attempt_diff_body_migration SET vacuum_pending=1`); err != nil {
				t.Fatal(err)
			}
			var migrated diffbody.File
			for range 3 {
				response := diffBodyRequest(t, f, f.service.attemptDiffBodyBatch, struct{}{})
				requireNativeStatus(t, response, http.StatusOK)
				var batch diffbody.Batch
				decodeHubResponse(t, response, &batch)
				inline := false
				for _, file := range batch.Files {
					if file.Patch == "" {
						continue
					}
					inline = true
					if file.DiffID == receipt.DiffID {
						migrated = file
					}
					wrong := file
					wrong.Body = diffbody.New(file.Organization, file.Project, file.DiffID, file.Position, strings.Repeat("x", len(file.Patch)))
					wrong.Patch = ""
					requireNativeStatus(t, diffBodyRequest(t, f, f.service.attemptDiffBodyStored, wrong), http.StatusUnprocessableEntity)
					file.Patch = ""
					requireNativeStatus(t, diffBodyRequest(t, f, f.service.attemptDiffBodyStored, file), http.StatusNoContent)
					requireNativeStatus(t, diffBodyRequest(t, f, f.service.attemptDiffBodyStored, file), http.StatusNoContent)
				}
				if !inline {
					break
				}
			}
			if migrated.Patch != "@@ legacy\n+preserved\n" {
				t.Fatal("migration did not return the original patch")
			}
			files, err := readAttemptDiffFiles(t.Context(), f.service.database.db, receipt.DiffID)
			if err != nil {
				t.Fatal(err)
			}
			file := files[0]
			if file.Path != "legacy.go" || file.Additions != 7 || file.Deletions != 3 || file.Patch != "" || file.PatchExpired != test.expired {
				t.Fatalf("migrated/expired file=%+v", file)
			}
			if test.expired {
				if file.PatchBody != nil {
					t.Fatal("expired body remains readable")
				}
				requireNativeStatus(t, diffBodyRequest(t, f, f.service.attemptDiffBodyDeleted, map[string]string{"key": migrated.Body.Key}), http.StatusNoContent)
			} else if file.PatchBody == nil || *file.PatchBody != migrated.Body {
				t.Fatal("protected diff lost its object reference")
			}
			response = diffBodyRequest(t, f, f.service.vacuumAttemptDiffBodies, struct{}{})
			requireNativeStatus(t, response, http.StatusOK)
			var size struct {
				Table  int64 `json:"attempt_diff_files_bytes"`
				Inline int64 `json:"inline_body_bytes"`
			}
			decodeHubResponse(t, response, &size)
			if size.Table <= 0 || size.Inline != 0 {
				t.Fatalf("vacuum size=%+v", size)
			}
		})
	}
}
