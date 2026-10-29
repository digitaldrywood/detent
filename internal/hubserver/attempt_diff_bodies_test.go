package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"modernc.org/sqlite"

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
		name                            string
		currentChange, running, latest  bool
		expired                         bool
		legacy                          bool
		recent, otherChange, noVersions bool
		newerOtherSource                bool
		currentVersion, versionAttempt  any
	}{
		{name: "superseded", expired: true, legacy: true},
		{name: "current Change", currentChange: true},
		{name: "converted current Change", currentChange: true, currentVersion: "version_current", versionAttempt: "attempt_old"},
		{name: "converted superseded version", currentChange: true, currentVersion: "version_other", versionAttempt: "attempt_old", expired: true},
		{name: "converted unrelated attempt", currentChange: true, currentVersion: "version_current", versionAttempt: "attempt_other", expired: true},
		{name: "converted current version with unknown attempt", currentChange: true, currentVersion: "version_current"},
		{name: "unknown current version with converted attempt", currentChange: true, versionAttempt: "attempt_old"},
		{name: "unknown claim with unrelated null attempt", currentChange: true, otherChange: true},
		{name: "unknown claim with unrelated matching attempt", currentChange: true, otherChange: true, versionAttempt: "attempt_old"},
		{name: "unknown claim without versions", currentChange: true, noVersions: true, expired: true},
		{name: "unknown claim with unrelated different attempt", currentChange: true, otherChange: true, versionAttempt: "attempt_other", expired: true},
		{name: "running", running: true},
		{name: "latest work item", latest: true},
		{name: "latest source with newer workspace diff", newerOtherSource: true},
		{name: "within retention", recent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAttemptDiffFixture(t)
			if test.legacy {
				for _, statement := range []string{"PRAGMA auto_vacuum=NONE", "VACUUM"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
				var mode int
				if err := f.service.database.db.QueryRowContext(t.Context(), "PRAGMA auto_vacuum").Scan(&mode); err != nil {
					t.Fatal(err)
				}
				if mode != 0 {
					t.Fatalf("legacy auto_vacuum=%d, want 0", mode)
				}
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(t, cfg)
				if err := f.service.database.db.QueryRowContext(t.Context(), "PRAGMA auto_vacuum").Scan(&mode); err != nil {
					t.Fatal(err)
				}
				if mode != 0 {
					t.Fatalf("startup rebuilt legacy database: auto_vacuum=%d", mode)
				}
			}
			response := f.post(t, f.request(2, tracker.AttemptDiffFile{Path: "legacy.go", Status: tracker.DiffStatusModified, Additions: 7, Deletions: 3, Patch: "@@ legacy\n+preserved\n"}))
			requireNativeStatus(t, response, http.StatusAccepted)
			var receipt tracker.AttemptDiffReceipt
			decodeHubResponse(t, response, &receipt)
			if !test.latest {
				requireNativeStatus(t, f.post(t, f.request(3, tracker.AttemptDiffFile{Path: "new.go", Status: tracker.DiffStatusAdded, Patch: "+new"})), http.StatusAccepted)
			}
			if test.newerOtherSource {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE attempt_diffs SET source='workspace' WHERE attempt_id=? AND seq=3`, f.attempt); err != nil {
					t.Fatal(err)
				}
			}
			attempt := f.attempt
			if !test.running {
				attempt = "attempt_old"
			}
			createdAt := time.Now().Add(-40 * 24 * time.Hour)
			if test.recent {
				createdAt = time.Now().Add(-24 * time.Hour)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE attempt_diffs SET attempt_id=?,created_at=? WHERE id=?`, attempt, formatHubTime(createdAt), receipt.DiffID); err != nil {
				t.Fatal(err)
			}
			if test.currentChange {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id) VALUES ('change_current',?,?,?,'{"current_version_id":"version_current"}',?)`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, test.currentVersion); err != nil {
					t.Fatal(err)
				}
				changeID := "change_current"
				if test.otherChange {
					changeID = "change_other"
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id) VALUES (?,?,?,?,'{}','')`, changeID, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if !test.noVersions {
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,attempt_id) VALUES ('version_current',?,1,'{"attempt_id":"attempt_old"}',?),('version_other',?,2,'{"attempt_id":"attempt_other"}','attempt_other')`, changeID, test.versionAttempt, changeID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `ANALYZE`); err != nil {
				t.Fatal(err)
			}
			cutoff := formatHubTime(time.Now().Add(-diffBodyRetention))
			for _, query := range []struct {
				statement string
				args      []any
			}{
				{expireDiffBodyManifest, []any{receipt.DiffID, cutoff}},
				{expireDiffBodyFile, []any{receipt.DiffID, 0, cutoff}},
			} {
				plan := explainQueryPlan(t, f.service.database.db, query.statement, query.args)
				for _, detail := range plan {
					if strings.Contains(detail, "SCAN v") || strings.Contains(detail, "SCAN change_versions") {
						t.Fatalf("per-candidate expiry scans change versions: %s", strings.Join(plan, "\n"))
					}
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE attempt_diff_body_migration SET vacuum_pending=?`, !test.legacy); err != nil {
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
					if !batch.VacuumPending {
						t.Fatal("migration does not report pending vacuum")
					}
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
			var mode int
			if err := f.service.database.db.QueryRowContext(t.Context(), "PRAGMA auto_vacuum").Scan(&mode); err != nil {
				t.Fatal(err)
			}
			if mode != 2 {
				t.Fatalf("vacuum auto_vacuum=%d, want 2", mode)
			}
			batchResponse := diffBodyRequest(t, f, f.service.attemptDiffBodyBatch, struct{}{})
			requireNativeStatus(t, batchResponse, http.StatusOK)
			var batch diffbody.Batch
			decodeHubResponse(t, batchResponse, &batch)
			if batch.VacuumPending {
				t.Fatal("completed vacuum remains pending")
			}
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

func TestAttemptDiffBodyExpiryCandidates(t *testing.T) {
	var examined atomic.Int64
	probe := fmt.Sprintf("diff_expiry_probe_%x", sha256.Sum256([]byte(t.TempDir())))
	if err := sqlite.RegisterScalarFunction(probe, 1, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		examined.Add(1)
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Parallel()
	for _, test := range []struct {
		name           string
		archive        bool
		claimAfterRead bool
		unrelated      int
		nullAttempts   int
		otherTenant    bool
		otherSource    int
	}{
		{name: "bounded manifest and file expiry", unrelated: 10},
		{name: "bounded expiry with project history", unrelated: 1000},
		{name: "ten null attempts in another project", nullAttempts: 10},
		{name: "ten thousand null attempts in another project", nullAttempts: 10000},
		{name: "ten null attempts in another tenant", nullAttempts: 10, otherTenant: true},
		{name: "ten thousand null attempts in another tenant", nullAttempts: 10000, otherTenant: true},
		{name: "ten newer diffs from another source", otherSource: 10},
		{name: "ten thousand newer diffs from another source", otherSource: 10000},
		{name: "archived file manifest expiry", archive: true},
		{name: "new claim protects selected bodies", claimAfterRead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAttemptDiffFixture(t)
			if test.nullAttempts > 0 {
				organization := f.project.OrganizationID
				if test.otherTenant {
					response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other tenant"})
					requireNativeStatus(t, response, http.StatusCreated)
					var other nativeOrganization
					decodeHubResponse(t, response, &other)
					organization = other.ID
				}
				other := newNativeFixture(t, f.service, organization, "other-project")
				issue := other.create(t, "other-work")
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id)
VALUES ('change_null_history',?,?,?,'{}','version_null_1')`, organization, other.project.ID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO change_versions(id,change_id,number,record_json,attempt_id)
SELECT 'version_null_'||i,'change_null_history',i,'{}',NULL FROM n`, test.nullAttempts); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id)
VALUES ('change_attempt_history',?,?,?,'{}','')`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,attempt_id)
VALUES ('version_attempt_history','change_attempt_history',1,'{}','attempt_old')`); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<40)
INSERT INTO attempt_diffs(id,attempt_id,organization_id,project_id,work_item_id,source,seq,base_sha,head_sha,
producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,created_at,object_key)
SELECT 'diff_legacy_'||i,'attempt_legacy',?,?,?,'attempt',i,'base','head','attempt','producer','runner','lease',1,?,'manifest' FROM n`,
					organization, other.project.ID, issue.WorkItemID, formatHubTime(time.Now().Add(-40*24*time.Hour))); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attempt_diff_files(diff_id,position,path,status,patch_object)
SELECT id,0,'legacy.go','modified',id||'/0' FROM attempt_diffs WHERE attempt_id='attempt_legacy'`); err != nil {
					t.Fatal(err)
				}
			}
			if test.unrelated > 0 {
				if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id)
SELECT 'change_history_'||i,?,?,?,'{}','version_history_'||i FROM n`, test.unrelated, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,attempt_id)
SELECT claim_version_id,id,1,'{}','attempt_history_'||id FROM change_requests WHERE id LIKE 'change_history_%'`); err != nil {
					t.Fatal(err)
				}
			}
			requireNativeStatus(t, f.post(t, f.request(2, tracker.AttemptDiffFile{Path: "latest.go", Status: tracker.DiffStatusModified})), http.StatusAccepted)
			manifestKey := "manifest"
			if test.archive {
				manifestKey = ""
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<40)
INSERT INTO attempt_diffs(id,attempt_id,organization_id,project_id,work_item_id,source,seq,base_sha,head_sha,
producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,created_at,object_key)
SELECT 'diff_old_'||i,'attempt_old',?,?,?,'attempt',i,'base','head','attempt','producer','runner','lease',1,?,? FROM n`,
				f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, formatHubTime(time.Now().Add(-40*24*time.Hour)), manifestKey); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attempt_diff_files(diff_id,position,path,status,patch_object)
SELECT d.id,p.position,'example.go','modified',d.id||'/'||p.position
FROM attempt_diffs d CROSS JOIN (SELECT 0 AS position UNION ALL SELECT 1) p WHERE d.attempt_id='attempt_old'`); err != nil {
				t.Fatal(err)
			}
			if test.archive {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attempt_diff_file_archives(diff_id,position,organization_id,project_id,object_key,object_sha256,object_bytes,content_bytes)
SELECT id,2,organization_id,project_id,'archive','sha256',1,1 FROM attempt_diffs WHERE attempt_id='attempt_old'`); err != nil {
					t.Fatal(err)
				}
			}
			if test.otherSource > 0 {
				if _, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO attempt_diffs(id,attempt_id,organization_id,project_id,work_item_id,source,seq,base_sha,head_sha,
producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,created_at)
SELECT 'diff_workspace_'||i,'attempt_workspace',?,?,?,'workspace',i,'base','head','workspace','producer','runner','lease',1,? FROM n`,
					test.otherSource, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, formatHubTime(time.Now().Add(time.Hour))); err != nil {
					t.Fatal(err)
				}
			}
			readCtx, cancelRead := context.WithTimeout(t.Context(), hubReadTimeout)
			defer cancelRead()
			candidates, err := f.service.selectDiffBodyExpiryCandidates(readCtx, time.Now().Add(-diffBodyRetention))
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates.manifests) != 32 || len(candidates.files) != 32 {
				t.Fatalf("candidate counts: manifests=%d files=%d, want 32 each", len(candidates.manifests), len(candidates.files))
			}
			if test.nullAttempts > 0 {
				for _, id := range candidates.manifests {
					if strings.HasPrefix(id, "diff_legacy_") {
						t.Fatalf("protected project's manifest selected for expiry: %s", id)
					}
				}
				for _, file := range candidates.files {
					if strings.HasPrefix(file.diffID, "diff_legacy_") {
						t.Fatalf("protected project's file selected for expiry: %s", file.diffID)
					}
				}
			}
			if test.nullAttempts > 0 || test.otherSource > 0 {
				if _, err := f.service.database.db.ExecContext(t.Context(), `ANALYZE`); err != nil {
					t.Fatal(err)
				}
				for _, query := range []struct {
					name      string
					statement string
					args      []any
				}{
					{"manifest", expireDiffBodyManifest, []any{candidates.manifests[0], formatHubTime(time.Now().Add(-diffBodyRetention))}},
					{"file", expireDiffBodyFile, []any{candidates.files[0].diffID, candidates.files[0].position, formatHubTime(time.Now().Add(-diffBodyRetention))}},
				} {
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					examined.Store(0)
					statement := strings.ReplaceAll(query.statement, "WHERE v.attempt_id", "WHERE "+probe+"(v.id) AND v.attempt_id")
					if test.otherSource > 0 {
						statement = strings.ReplaceAll(query.statement, "WHERE latest.organization_id", "WHERE "+probe+"(latest.id) AND latest.organization_id")
					}
					result, err := tx.ExecContext(t.Context(), statement, query.args...)
					if err != nil {
						t.Fatal(err)
					}
					if count, err := result.RowsAffected(); err != nil || count != 1 {
						t.Fatalf("%s expiry updated %d rows: %v", query.name, count, err)
					}
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
					t.Logf("%s writer examined %d rows with %d unrelated NULL attempts and %d other-source diffs", query.name, examined.Load(), test.nullAttempts, test.otherSource)
					if examined.Load() != 1 {
						t.Errorf("%s writer examined %d rows, want 1", query.name, examined.Load())
					}
				}
			}
			if test.unrelated > 0 || test.nullAttempts > 0 || test.otherSource > 0 {
				if _, err := f.service.database.db.ExecContext(t.Context(), `ANALYZE`); err != nil {
					t.Fatal(err)
				}
				cutoff := formatHubTime(time.Now().Add(-diffBodyRetention))
				for _, query := range []struct {
					name      string
					statement string
					args      []any
				}{
					{"manifest", expireDiffBodyManifest, []any{candidates.manifests[0], cutoff}},
					{"file", expireDiffBodyFile, []any{candidates.files[0].diffID, candidates.files[0].position, cutoff}},
				} {
					plan := explainQueryPlan(t, f.service.database.db, query.statement, query.args)
					for _, detail := range plan {
						if test.otherSource > 0 && (strings.HasPrefix(detail, "SCAN latest") ||
							strings.HasPrefix(detail, "SEARCH latest ") && !strings.Contains(detail, "source=?") ||
							strings.Contains(detail, "TEMP B-TREE")) {
							t.Errorf("%s latest lookup walks or sorts history with %d other-source diffs: %s", query.name, test.otherSource, strings.Join(plan, "\n"))
							break
						}
						if strings.HasPrefix(detail, "SCAN c") || strings.HasPrefix(detail, "SCAN v") ||
							strings.HasPrefix(detail, "SEARCH c ") && !strings.Contains(detail, "claim_version_id=?") ||
							strings.HasPrefix(detail, "SEARCH v ") && !strings.Contains(detail, "attempt_id=?") {
							t.Errorf("%s writer recheck walks project history with %d unrelated Changes: %s", query.name, test.unrelated, strings.Join(plan, "\n"))
							break
						}
					}
				}
			}
			if test.claimAfterRead {
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,claim_version_id)
VALUES ('change_current',?,?,?,'{}','version_current')`, f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,attempt_id)
VALUES ('version_current','change_current',1,'{}','attempt_old')`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.expireDiffBodyCandidates(t.Context(), candidates); err != nil {
				t.Fatal(err)
			}
			manifests := make(map[string]bool)
			files := make(map[diffBodyExpiryFile]bool)
			if !test.claimAfterRead {
				for _, id := range candidates.manifests {
					manifests[id] = true
				}
				for _, file := range candidates.files {
					files[file] = true
				}
			}
			rows, err := f.service.database.reader.QueryContext(t.Context(), `SELECT d.id,f.position,d.manifest_expired,f.patch_expired
FROM attempt_diffs d JOIN attempt_diff_files f ON f.diff_id=d.id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var file diffBodyExpiryFile
				var manifestExpired, patchExpired bool
				if err := rows.Scan(&file.diffID, &file.position, &manifestExpired, &patchExpired); err != nil {
					t.Fatal(err)
				}
				if manifestExpired != manifests[file.diffID] || patchExpired != files[file] {
					t.Errorf("diff=%s position=%d expired manifest=%v patch=%v, want manifest=%v patch=%v", file.diffID, file.position, manifestExpired, patchExpired, manifests[file.diffID], files[file])
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
