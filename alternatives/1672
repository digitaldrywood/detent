package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudAttachmentSavedReferences(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"change-visual-path", "change-visual-short", "issue", "comment", "issue-path", "comment-path", "issue-path-other-item", "comment-path-other-item", "issue-visual-path", "comment-visual-path", "issue-visual-reference", "comment-visual-reference", "issue-visual-path-reference", "comment-visual-path-reference", "issue-visual-path-title"} {
		t.Run(source, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			send := func(method, suffix string, input any, want int) []byte {
				t.Helper()
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				response := f.serve(t, hostedSharedRequest{user: &owner, method: method, target: f.base + suffix, body: string(body), csrf: csrf})
				if response.Code != want {
					t.Fatalf("%s %s: %d %s", method, suffix, response.Code, response.Body.String())
				}
				return response.Body.Bytes()
			}
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "image.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: artifact.Digest([]byte("image"))}
			raw := send(http.MethodPost, "/attachment-metadata", record, http.StatusCreated)
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			foreign := conversation.NewAttachmentID()
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: "prj_foreign", Name: "foreign", Slug: "foreign", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: "org_security", Kind: "attachment", Metadata: attachment.Metadata{ID: foreign, ProjectID: "prj_foreign", Uploader: record.Uploader, Name: "file.txt", ContentType: "text/plain", Size: 10, SHA256: record.SHA256, CreatedAt: time.Now().UTC()}})
			})
			body := "![image](attachment:" + record.ID + ")\n[file](attachment:" + record.ID + ")\n![foreign](attachment:" + foreign + ")"
			if strings.HasPrefix(source, "change") {
				body = "![image](attachment:" + record.ID + ")"
			}
			if strings.Contains(source, "-path") {
				body = record.Markdown("org_security")
			}
			if strings.HasSuffix(source, "-reference") {
				destination := "attachment:" + record.ID
				if strings.Contains(source, "-path") {
					destination = "/organizations/org_security/api/v2/projects/" + string(f.project) + "/attachments/" + record.ID
				}
				body = "![image][shot]\n\n[shot]: " + destination
			}
			if strings.Contains(source, "-visual") {
				send(http.MethodPut, "/content-settings", cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: "enable-visual"}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}, http.StatusOK)
				body = "Screenshot: https://example.test/preview " + body
				if !strings.HasSuffix(source, "-reference") {
					body += ". Documentation: [API guide](https://example.test/docs) [Pull request](https://github.com/example-org/example-project/pull/1)"
				}
			}
			issue := tracker.NativeIssue{}
			comment := tracker.NativeComment{}
			if strings.HasPrefix(source, "change") {
				issue.WorkItemID = f.seedIssue(t, 1)
				issue.Revision = 1
				send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "save-image"}, Title: "Visual change", Body: body}, http.StatusOK)
			} else if strings.HasPrefix(source, "issue") {
				title, issueBody := "Image issue", body+issueContractTestSections
				if strings.HasSuffix(source, "-title") {
					title, issueBody = body, issueContractTestSections
				}
				raw = send(http.MethodPost, "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "save-image"}, Title: title, Body: issueBody, State: "Todo"}, http.StatusOK)
				if err := json.Unmarshal(raw, &issue); err != nil {
					t.Fatal(err)
				}
			} else {
				issue.WorkItemID = f.seedIssue(t, 1)
				raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "save-image"}, Body: body}, http.StatusOK)
				if err := json.Unmarshal(raw, &comment); err != nil {
					t.Fatal(err)
				}
			}
			assertReference := func(id, item, comment string) {
				t.Helper()
				project := f.project
				if id == foreign {
					project = "prj_foreign"
				}
				cloudAttachmentRead(t, f, func(r tenantstore.Reader) error {
					references, err := r.AttachmentReferences(t.Context(), project, id, 201)
					if err != nil {
						return err
					}
					want := 1
					if item == "" {
						want = 0
					}
					if len(references) != want || want == 1 && (string(references[0].WorkItemID) != item || references[0].CommentID != comment) {
						t.Fatalf("refs=%+v want %s/%s", references, item, comment)
					}
					return nil
				})
				if id != foreign {
					response := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + id})
					var metadata attachment.Metadata
					if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &metadata) != nil || metadata.WorkItemID != item || metadata.CommentID != comment {
						t.Fatalf("reference metadata=%d %s want %s/%s", response.Code, response.Body.String(), item, comment)
					}
				}
			}
			assertReference(record.ID, string(issue.WorkItemID), comment.ID)
			if strings.HasPrefix(source, "change") {
				replacement := "Replacement" + issueContractTestSections
				send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "replace-issue"}, ExpectedRevision: issue.Revision, Body: &replacement}, http.StatusOK)
				assertReference(record.ID, string(issue.WorkItemID), "")
				raw = send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "reuse-image"}, Body: body}, http.StatusOK)
				var reused tracker.NativeComment
				if err := json.Unmarshal(raw, &reused); err != nil {
					t.Fatal(err)
				}
				cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
					return w.DeleteComment(t.Context(), f.project, issue.WorkItemID, reused.ID)
				})
				assertReference(record.ID, string(issue.WorkItemID), "")
				response := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
				if response.Code != http.StatusOK {
					t.Fatalf("retained Change source read=%d", response.Code)
				}
				return
			}

			if strings.HasSuffix(source, "-reference") {
				if strings.HasPrefix(source, "issue") {
					changed := "Updated " + body + issueContractTestSections
					raw = send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-reference"}, ExpectedRevision: issue.Revision, Body: &changed}, http.StatusOK)
					if err := json.Unmarshal(raw, &issue); err != nil {
						t.Fatal(err)
					}
				} else {
					raw = send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID)+"/comments/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-reference"}, ExpectedRevision: comment.Revision, Body: "Updated " + body}, http.StatusOK)
					if err := json.Unmarshal(raw, &comment); err != nil {
						t.Fatal(err)
					}
				}
				assertReference(record.ID, string(issue.WorkItemID), comment.ID)
			}
			if strings.HasSuffix(source, "-title") {
				send(http.MethodPut, "/content-settings", cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: "disable-visual"}, ContentChange: projectsettings.ContentChange{ExpectedRevision: 1}}, http.StatusOK)
				raw = send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "priority-only"}, ExpectedRevision: issue.Revision, Priority: tracker.SetPriority(new(2))}, http.StatusOK)
				if err := json.Unmarshal(raw, &issue); err != nil {
					t.Fatal(err)
				}
				assertReference(record.ID, string(issue.WorkItemID), "")
				changed := "Changed content" + issueContractTestSections
				raw = send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "body-only"}, ExpectedRevision: issue.Revision, Body: &changed}, http.StatusOK)
				if err := json.Unmarshal(raw, &issue); err != nil {
					t.Fatal(err)
				}
				assertReference(record.ID, string(issue.WorkItemID), "")
			}
			if strings.Contains(source, "-visual") && !strings.HasSuffix(source, "-title") {
				textFile := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "diagnostic.log", ContentType: "text/plain", Size: 10, SHA256: record.SHA256}
				send(http.MethodPost, "/attachment-metadata", textFile, http.StatusCreated)
				const visual = "https://figma.com/design/example "
				for _, invalid := range []struct{ name, body string }{
					{"unrelated binding", visual},
					{"text file", visual + textFile.Markdown("org_security")},
					{"fabricated", visual + "![upload](attachment:att_0123456789abcdef0123456789abcdef)"},
					{"foreign", visual + "![upload](attachment:" + foreign + ")"},
					{"foreign canonical", visual + "![upload](/organizations/org_security/api/v2/projects/prj_foreign/attachments/" + foreign + ")"},
					{"wrong paragraph", visual + "\n\n" + record.Markdown("org_security")},
					{"multiple sources", visual + "https://example.test/image.png " + record.Markdown("org_security")},
				} {
					send(http.MethodPost, "/work-items/"+string(issue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "invalid-" + invalid.name}, Body: invalid.body}, http.StatusUnprocessableEntity)
				}
			}
			assertReference(foreign, "", "")
			cloudAttachmentRead(t, f, func(r tenantstore.Reader) error {
				references, err := r.AttachmentReferences(t.Context(), f.project, record.ID, 201)
				if err == nil && len(references) != 1 {
					t.Fatalf("references=%d", len(references))
				}
				return err
			})
			if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE attachments SET created_at=$1 WHERE organization_id=$2 AND project_id=$3 AND id=$4", time.Now().Add(-attachment.OrphanTTL-time.Hour), "org_security", f.project, record.ID); err != nil {
				t.Fatal(err)
			}
			sweep := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/expired", body: "{}"})
			if sweep.Code != http.StatusOK || strings.Contains(sweep.Body.String(), record.ID) {
				t.Fatalf("referenced image swept: %d %s", sweep.Code, sweep.Body.String())
			}
			copyItem := issue.WorkItemID
			if strings.HasSuffix(source, "-other-item") {
				copyItem = f.seedIssue(t, 2)
			}
			var other tracker.NativeComment
			raw = send(http.MethodPost, "/work-items/"+string(copyItem)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "copy-image"}, Body: body}, http.StatusOK)
			if err := json.Unmarshal(raw, &other); err != nil {
				t.Fatal(err)
			}
			var metadata attachment.Metadata
			response := f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusOK {
				t.Fatalf("metadata=%d", response.Code)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
				t.Fatal(err)
			}
			if len(metadata.ReferencedBy) != 2 {
				t.Fatalf("referenced_by=%v", metadata.ReferencedBy)
			}
			if strings.HasPrefix(source, "issue") {
				removed := "No image" + issueContractTestSections
				update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-image"}, ExpectedRevision: issue.Revision, Body: &removed}
				if strings.HasSuffix(source, "-title") {
					update.Title = new("Image issue")
				}
				send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID), update, http.StatusOK)
			} else {
				send(http.MethodPatch, "/work-items/"+string(issue.WorkItemID)+"/comments/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "remove-image"}, ExpectedRevision: comment.Revision, Body: "No image"}, http.StatusOK)
			}
			assertReference(record.ID, string(copyItem), other.ID)
			var last tracker.NativeComment
			raw = send(http.MethodPost, "/work-items/"+string(copyItem)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "retain-copy"}, Body: body}, http.StatusOK)
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatal(err)
			}
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error { return w.DeleteComment(t.Context(), f.project, copyItem, other.ID) })
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusOK {
				t.Fatalf("retained copy read=%d %s", response.Code, response.Body.String())
			}
			assertReference(record.ID, string(copyItem), last.ID)
			send(http.MethodPatch, "/work-items/"+string(copyItem)+"/comments/"+last.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "orphan-copy"}, ExpectedRevision: last.Revision, Body: "No attachment"}, http.StatusOK)
			assertReference(record.ID, "", "")
			raw = send(http.MethodPost, "/work-items/"+string(copyItem)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "last-copy"}, Body: body}, http.StatusOK)
			if err := json.Unmarshal(raw, &last); err != nil {
				t.Fatal(err)
			}
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error { return w.DeleteComment(t.Context(), f.project, copyItem, last.ID) })
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + record.ID})
			if response.Code != http.StatusNotFound {
				t.Fatalf("last source deletion read=%d", response.Code)
			}
		})
	}
	t.Run("unchanged deleted link retains binding without read access", func(t *testing.T) {
		v := newVerificationFixture(t)
		reporter, member := v.users["reporter"], v.users["member"]
		response := v.request(t, reporter, http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "unreadable-source"}, Type: "bug", Title: "Editable attachment report", Body: reporterBugSections})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "private-upload.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: artifact.Digest([]byte("private-image"))}
		response = v.request(t, member, http.MethodPost, v.base+"/attachment-metadata", record)
		requireNativeStatus(t, response, http.StatusCreated)
		decodeHubResponse(t, response, &record)
		metadataPath := v.base + "/attachment-metadata/" + record.ID
		requireNativeStatus(t, v.request(t, reporter, http.MethodGet, metadataPath, nil), http.StatusNotFound)
		path := v.base + "/work-items/" + string(issue.WorkItemID)
		linkedBody := reporterBugSections + issueContractTestSections + "\n![Image](attachment:" + record.ID + ")"
		response = v.request(t, member, http.MethodPatch, path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "member-links-image"}, ExpectedRevision: issue.Revision, Body: &linkedBody})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		response = v.request(t, reporter, http.MethodGet, metadataPath, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var visible attachment.Metadata
		decodeHubResponse(t, response, &visible)
		if visible.ID != record.ID || visible.WorkItemID != string(issue.WorkItemID) || len(visible.ReferencedBy) != 1 || visible.ReferencedBy[0].WorkItemID != string(issue.WorkItemID) {
			t.Fatalf("own bound other-uploader metadata=%+v", visible)
		}
		requireNativeStatus(t, v.request(t, member, http.MethodDelete, metadataPath, nil), http.StatusNoContent)
		requireNativeStatus(t, v.request(t, reporter, http.MethodGet, metadataPath, nil), http.StatusNotFound)
		var before []attachmentstore.Reference
		cloudAttachmentRead(t, v.hostedSharedFixture, func(r tenantstore.Reader) error {
			var err error
			before, err = r.AttachmentReferences(t.Context(), v.project, record.ID, 201)
			if err == nil && (len(before) != 1 || before[0].WorkItemID != issue.WorkItemID || before[0].CommentID != "" || before[0].RetainWithWorkItem) {
				t.Fatalf("deleted attachment lost its ordinary source binding before edit: %+v", before)
			}
			return err
		})
		editedBody := linkedBody + "\nReporter update"
		response = v.request(t, reporter, http.MethodPatch, path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reporter-retains-unreadable-link"}, ExpectedRevision: issue.Revision, Body: &editedBody})
		requireNativeStatus(t, response, http.StatusOK)
		var edited tracker.NativeIssue
		decodeHubResponse(t, response, &edited)
		if edited.Body != editedBody || edited.Revision != issue.Revision+1 {
			t.Fatalf("reporter edit body/revision=%+v", edited)
		}
		cloudAttachmentRead(t, v.hostedSharedFixture, func(r tenantstore.Reader) error {
			after, err := r.AttachmentReferences(t.Context(), v.project, record.ID, 201)
			if err != nil {
				return err
			}
			if !slices.Equal(before, after) {
				t.Fatalf("unchanged unreadable binding changed: before=%+v after=%+v", before, after)
			}
			row, err := r.Attachment(t.Context(), v.project, record.ID)
			if err == nil && (row.Metadata.DeletedAt == nil || row.Metadata.WorkItemID != string(issue.WorkItemID) || row.Metadata.CommentID != "") {
				t.Fatalf("edit changed deleted attachment authority: %+v", row.Metadata)
			}
			return err
		})
		response = v.request(t, reporter, http.MethodGet, metadataPath, nil)
		requireNativeStatus(t, response, http.StatusNotFound)
		for _, private := range []string{record.Name, record.SHA256, record.Uploader} {
			if private != "" && strings.Contains(response.Body.String(), private) {
				t.Fatalf("unreadable metadata response exposed private field: %s", response.Body.String())
			}
		}
	})

}

func TestCloudAttachmentQuota(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                      string
		allowance, apiLimit, used int64
		status                    int
		plan                      string
		retained                  int64
	}{
		{"Starter above Free storage", 25 << 30, 100, 0, http.StatusCreated, "starter", 2 << 30},
		{"Free over storage", 1 << 30, 100, 0, http.StatusTooManyRequests, "free", 2 << 30},
		{"allowed", 10000, 2, 0, http.StatusCreated, "", 0},
		{"exact preflight limit", 100, 100, 0, http.StatusCreated, "", 0},
		{"refused", 99, 100, 0, http.StatusTooManyRequests, "", 0},
		{"API exhausted", 10000, 1, 1, http.StatusTooManyRequests, "", 0},
		{"outer API limit", 10000, 1, 0, http.StatusTooManyRequests, "", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			plans := hostedplans.CapacityPlans()
			base := hostedplans.PlanReference{ID: test.plan, Version: 1}
			if test.plan == "" {
				plans = hostedplans.PilotPlans()
				plan := plans.Plans[0]
				plan.ID = "example-attachment-plan"
				plan.Allowances = maps.Clone(plan.Allowances)
				plan.Allowances["collaboration_bytes"], plan.Allowances["api_mutations"] = test.allowance, test.apiLimit
				plans.Plans, plans.Base, base = append([]hostedplans.HostedPlan{plan}, plans.Plans...), plan.PlanReference, plan.PlanReference
			}
			if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
				t.Fatal(err)
			}
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				assignment, err := w.BillingAssignment(t.Context())
				if err != nil {
					return err
				}
				assignment.Base = billingstore.PlanReference{ID: base.ID, Version: base.Version}
				return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
			})
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "file.txt", ContentType: "text/plain", Size: 100, SHA256: artifact.Digest([]byte("hello"))}
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
			now := time.Now().UTC()
			f.service.config.Clock = func() time.Time { return now }
			f.service.database.now = f.service.config.Clock
			window := now.Unix() / plans.WindowSeconds * plans.WindowSeconds
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				if test.used > 0 {
					result, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: "org_security", ID: "attachment-quota-seed", WindowStart: time.Unix(window, 0), Metric: "api_mutations", Amount: test.used}, math.MaxInt64)
					if err != nil {
						return err
					}
					if !result.Allowed || !result.Applied {
						t.Fatalf("seed usage=%+v", result)
					}
				}
				if test.retained > 0 {
					result, err := w.AdjustCollaborationBytes(t.Context(), test.retained, math.MaxInt64)
					if err != nil {
						return err
					}
					if !result.Allowed {
						t.Fatalf("seed retained=%+v", result)
					}
				}
				return nil
			})
			installReportsBudgetTrace(t, f.service)
			var preflightQueries []string
			timing := &requestTiming{Trace: func(query string, _ []driver.NamedValue) { preflightQueries = append(preflightQueries, query) }}
			request := httptest.NewRequest(http.MethodPost, f.base+"/attachment-metadata/check", strings.NewReader(`{"size":100}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CSRF-Token", csrf)
			signed, err := cloudassert.Sign(f.key, f.claims(&owner, cloudassert.KindBrowser, http.MethodPost, request.URL.RequestURI(), []byte(`{"size":100}`)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set(cloudassert.Header, signed)
			preflight := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(preflight, request.WithContext(withRequestTiming(t.Context(), timing)))
			counterRead := false
			for _, query := range preflightQueries {
				if strings.Contains(query, "hosted_usage_counters") {
					counterRead = true
				}
				if strings.Contains(query, "machines") || strings.Contains(query, "hosted_usage_windows") {
					t.Fatalf("preflight read unrelated population: %s", query)
				}
			}
			if !counterRead {
				t.Fatal("preflight did not read collaboration bytes")
			}
			wantPreflight := http.StatusOK
			if test.allowance < record.Size+test.retained {
				wantPreflight = http.StatusTooManyRequests
			}
			if preflight.Code != wantPreflight {
				t.Fatalf("preflight=%d %s", preflight.Code, preflight.Body.String())
			}

			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata", body: string(body), csrf: csrf})
			if response.Code != test.status {
				t.Fatalf("upload=%d %s", response.Code, response.Body.String())
			}
			count := 0
			cloudAttachmentRead(t, f, func(r tenantstore.Reader) error {
				_, err := r.Attachment(t.Context(), f.project, record.ID)
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err == nil {
					count = 1
				}
				return err
			})
			if test.status == http.StatusTooManyRequests {
				if count != 0 || !strings.Contains(response.Body.String(), "allowance_exhausted") {
					t.Fatal("quota refusal retained metadata or changed allowance error")
				}
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_command_states WHERE organization_id=$1 AND operation LIKE 'attachment.record %'", "org_security").Scan(&count); err != nil || count != 0 {
					t.Fatalf("quota refusal retained receipt: count=%d error=%v", count, err)
				}
				cloudAttachmentRead(t, f, func(r tenantstore.Reader) error {
					used, err := r.BillingUsage(t.Context(), time.Unix(window, 0), "api_mutations")
					if errors.Is(err, corestore.ErrNotFound) && test.used == 0 {
						err = nil
					}
					if err == nil && used != test.used {
						t.Fatalf("quota refusal changed API usage: used=%d", used)
					}
					return err
				})
				if test.name == "API exhausted" {
					var failure hostedplans.LimitError
					decodeHubResponse(t, response, &failure)
					if failure.Resource != "api_mutations" || failure.Consumption != test.used {
						t.Fatalf("inner quota refusal=%+v", failure)
					}
				}
				return
			}
			usage, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, time.Now(), "collaboration_bytes", "api_mutations"))
			if err != nil {
				t.Fatal(err)
			}
			if usage["collaboration_bytes"] != record.Size+test.retained {
				t.Fatalf("attachment bytes=%d", usage["collaboration_bytes"])
			}
			replay := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata", body: string(body), csrf: csrf})
			if replay.Code != http.StatusCreated || replay.Body.String() != response.Body.String() {
				t.Fatalf("metadata replay=%d %s", replay.Code, replay.Body.String())
			}
			replayedUsage, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, time.Now(), "collaboration_bytes", "api_mutations"))
			if err != nil || replayedUsage["collaboration_bytes"] != usage["collaboration_bytes"] || replayedUsage["api_mutations"] != 2 || usage["api_mutations"] != 2 {
				t.Fatalf("metadata replay consumption=%v err=%v", replayedUsage, err)
			}
			if test.retained == 0 {
				var stored int64
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT COALESCE(sum(size),0) FROM attachments WHERE organization_id=$1 AND object_deleted_at IS NULL AND NOT customer_owned", "org_security").Scan(&stored); err != nil || stored != record.Size {
					t.Fatalf("stored attachment bytes=%d error=%v", stored, err)
				}
				recounted, err := hubapi.FeatureResult(f.service.database.plansService().Consumption(t.Context(), nil, now, "collaboration_bytes"))
				if err != nil || recounted["collaboration_bytes"] != record.Size {
					t.Fatalf("metadata recount consumption=%v err=%v", recounted, err)
				}
			}
		})
	}
}

func TestCloudAttachmentRetention(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	principal := cloudAttachmentPrincipal(t, f, owner.identity.Subject)
	item := f.seedIssue(t, 1)
	now := time.Now().UTC()
	for _, test := range []struct {
		name                         string
		age                          time.Duration
		referenced, deleted, expired bool
	}{
		{name: "old orphan", age: attachment.OrphanTTL + time.Minute, expired: true},
		{name: "new orphan", age: time.Hour},
		{name: "archived reference", age: attachment.OrphanTTL + time.Hour, referenced: true},
		{name: "deleted reference", age: time.Hour, referenced: true, deleted: true, expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := conversation.NewAttachmentID()
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				metadata := attachment.Metadata{ID: id, ProjectID: string(f.project), Uploader: principal, Name: "file.txt", ContentType: "text/plain", Size: 10, SHA256: artifact.Digest([]byte("hello")), CreatedAt: now.Add(-test.age)}
				if test.referenced {
					metadata.WorkItemID = string(item)
				}
				if _, err := w.AdjustCollaborationBytes(t.Context(), metadata.Size, math.MaxInt64); err != nil {
					return err
				}
				if err := w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: "org_security", Kind: "attachment", Metadata: metadata}); err != nil {
					return err
				}
				if test.referenced {
					if err := w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: f.project, AttachmentID: id, WorkItemID: item}); err != nil {
						return err
					}
				}
				if test.deleted {
					return w.(attachmentstore.DeletionWriter).DeleteAttachment(t.Context(), f.project, id, now)
				}
				if test.referenced {
					row, err := w.WorkItem(t.Context(), f.project, item)
					if err != nil {
						return err
					}
					row.Archived = true
					_, err = w.UpdateWorkItem(t.Context(), row)
					return err
				}
				return nil
			})
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/expired", body: "{}"})
			if response.Code != http.StatusOK {
				t.Fatalf("sweep=%d %s", response.Code, response.Body.String())
			}
			var expired []attachment.Metadata
			if err := json.Unmarshal(response.Body.Bytes(), &expired); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range expired {
				if record.ID == id {
					found = true
				}
			}
			if found != test.expired {
				t.Fatalf("expired=%v want %v", found, test.expired)
			}
			if test.expired {
				response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/attachments/deleted", body: `{"id":"` + id + `"}`})
				if response.Code != 204 {
					t.Fatalf("deleted=%d", response.Code)
				}
			}
		})
	}
}

func TestCloudAttachmentReferenceDeletion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		comment  bool
		retained bool
	}{{name: "issue"}, {name: "retained work item", retained: true}, {name: "comment", comment: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			item := f.seedIssue(t, 1)
			principal := cloudAttachmentPrincipal(t, f, owner.identity.Subject)
			id := conversation.NewAttachmentID()
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: "org_security", Kind: "attachment", Metadata: attachment.Metadata{ID: id, ProjectID: string(f.project), Uploader: principal, Name: "file.txt", ContentType: "text/plain", Size: 10, SHA256: artifact.Digest([]byte("hello")), CreatedAt: time.Now().UTC()}})
			})
			var comment string
			if test.comment {
				payload, err := json.Marshal(tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "source-comment"}, Body: "body"})
				if err != nil {
					t.Fatal(err)
				}
				response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/work-items/" + string(item) + "/comments", body: string(payload), csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")})
				if response.Code != http.StatusOK {
					t.Fatalf("comment=%d %s", response.Code, response.Body.String())
				}
				var saved tracker.NativeComment
				if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
					t.Fatal(err)
				}
				comment = saved.ID
			}
			payload, err := json.Marshal(map[string]string{"work_item_id": string(item), "comment_id": comment})
			if err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/attachment-metadata/" + id + "/reference", body: string(payload), csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")})
			if response.Code != 204 {
				t.Fatalf("reference=%d %s", response.Code, response.Body.String())
			}
			cloudAttachmentWrite(t, f, func(w tenantstore.Writer) error {
				if test.retained {
					if err := w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: f.project, AttachmentID: id, WorkItemID: item, RetainWithWorkItem: true}); err != nil {
						return err
					}
				}
				if test.comment {
					return w.DeleteComment(t.Context(), f.project, item, comment)
				}
				return w.DeleteWorkItem(t.Context(), f.project, item)
			})
			response = f.serve(t, hostedSharedRequest{user: &owner, target: f.base + "/attachment-metadata/" + id})
			if response.Code != 404 {
				t.Fatalf("deleted reference read=%d", response.Code)
			}
		})
	}
}

func cloudAttachmentRead(t *testing.T, f hostedSharedFixture, read func(tenantstore.Reader) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, read); err != nil {
		t.Fatal(err)
	}
}
func cloudAttachmentWrite(t *testing.T, f hostedSharedFixture, write func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, write); err != nil {
		t.Fatal(err)
	}
}
func cloudAttachmentPrincipal(t *testing.T, f hostedSharedFixture, user string) string {
	t.Helper()
	var principal string
	cloudAttachmentRead(t, f, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(t.Context(), user)
		principal = member.PrincipalID
		return err
	})
	return principal
}
