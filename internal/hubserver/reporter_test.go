package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const reporterBugSections = "## Steps to reproduce\nOpen the app and save.\n## Expected\nThe change saves.\n## Actual\nThe change disappears.\n## Where\nSettings screen.\n## Environment\nFirefox on Linux, build example."
const reporterChangeSections = "## Current behavior\nSaving requires opening settings.\n## Desired behavior\nSave from the current screen.\n## Why\nKeep the task in view."

func reporterTestBody(kind string) string {
	if kind == "change_request" {
		return reporterChangeSections
	}
	return reporterBugSections
}

func TestReporterNativeCommands(t *testing.T) {
	for _, test := range []struct {
		name, transport string
		upgraded        bool
		triage          *tracker.NativeState
	}{
		{"browser/new", "browser", false, nil},
		{"key/new", "key", false, nil},
		{"browser/upgraded", "browser", true, nil},
		{"key/upgraded", "key", true, nil},
		{"browser/terminal intake", "browser", true, &tracker.NativeState{Name: "Triage", Terminal: true, Transitions: []string{"Todo"}}},
		{"key/dispatchable intake", "key", true, &tracker.NativeState{Name: "Triage", Dispatchable: true, OperatorOnly: true, Transitions: []string{"Todo"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			u := f.user(t, "reporter", "reporter", "reporter@example.test", "write", "")
			member := f.user(t, "member", "member", "member@example.test", "write", "")
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			created := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: "/api/v2/organizations/org_security/projects", csrf: cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security"), body: `{"idempotency_key":"default-project","name":"Reporter default project","grant_access":true}`})
			requireNativeStatus(t, created, http.StatusCreated)
			var project tracker.NativeProject
			decodeHubResponse(t, created, &project)
			f.project = project.ID
			f.base = "/api/v2/organizations/org_security/projects/" + string(project.ID)
			seedNativeFixturePolicy(t, f.service, "org_security/"+string(project.ID))
			if test.upgraded {
				previousStates := reporterPreviousHostedStates()
				if test.triage != nil {
					previousStates = append([]tracker.NativeState{*test.triage}, previousStates...)
				}
				previous, err := json.Marshal(previousStates)
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET states_json=? WHERE id=?", string(previous), project.ID)
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM workflow_states WHERE project_id=? AND source_name IN ('Triage','Backlog','Cancelled')", project.ID)
				if test.triage != nil {
					operatorSQL(t, f.hostedSecurityFixture, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Triage','Triage',?,?,?,?)", project.ID, test.triage.Terminal, test.triage.Dispatchable, testTimestamp, testTimestamp)
				}
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hub_schema_version WHERE version_id IN (20261013050001,20261013085000)")
				if _, err := runMigrations(t.Context(), f.service.database.db, discardLogger()); err != nil {
					t.Fatal(err)
				}
			}

			f.grant(t, u, true, false)
			f.grant(t, member, true, false)
			request := func(user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := hostedSharedRequest{user: &user, method: method, target: path, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")}
				if body != nil {
					r.body = string(raw)
				}
				if test.transport == "key" {
					r.kind = cloudassert.KindKey
					r.mutate = func(c *cloudassert.Claims) {
						c.Subject, c.ProviderOrganization = user.identity.Subject, "org_provider"
						c.Role = "reporter"
						if user.identity.Subject == member.identity.Subject {
							c.Role = "member"
						}
						c.Key = &apikey.KeyAuthority{ID: "key_" + user.identity.Subject, Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{string(f.project)}}}
					}
				}
				return f.serve(t, r)
			}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{
				Mutation:            tracker.Mutation{IdempotencyKey: "scheduled-validation"},
				ScheduledValidation: &gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1/attempts/1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Conclusion: "success"},
			}), http.StatusForbidden)
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "screenshot.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: artifact.Digest([]byte("screenshot"))}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/attachment-metadata/check", map[string]any{"size": record.Size}), http.StatusOK)
			attachmentResponse := request(u, http.MethodPost, f.base+"/attachment-metadata", record)
			requireNativeStatus(t, attachmentResponse, http.StatusCreated)
			decodeHubResponse(t, attachmentResponse, &record)
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET require_visual_attachments=1 WHERE id=?", f.project)
			for _, issueType := range []string{"", "question"} {
				refused := request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "missing-type-" + issueType}, Title: "Tester report", Type: issueType})
				requireNativeStatus(t, refused, http.StatusUnprocessableEntity)
				var failure apiErrorResponse
				decodeHubResponse(t, refused, &failure)
				if failure.Code != "invalid_request" || !strings.Contains(failure.Message, "bug") || !strings.Contains(failure.Message, "change_request") {
					t.Fatalf("failure=%#v", failure)
				}
			}
			var own, sharedVisualIssue tracker.NativeIssue
			for i, state := range []string{"", "Todo", "Backlog", "Cancelled", "Triage", "Missing"} {
				draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("file-%d", i)}, Type: []string{"bug", "change_request"}[i%2], Title: "Tester report", Body: reporterTestBody([]string{"bug", "change_request"}[i%2]) + "\nScreenshot: https://example.test/preview ![Screenshot](attachment:" + record.ID + ")", State: state}
				r := request(u, http.MethodPost, f.base+"/work-items", draft)
				wantStatus := http.StatusOK
				if i > 0 {
					wantStatus = http.StatusConflict
				}
				requireNativeStatus(t, r, wantStatus)
				if i > 0 {
					var challenge duplicateChallenge
					decodeHubResponse(t, r, &challenge)
					if challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) == 0 {
						t.Fatalf("Reporter was not challenged: %s", r.Body.String())
					}
					draft.ChallengeToken = challenge.ChallengeToken
					for _, match := range challenge.PossibleDuplicates {
						draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
					}
					r = request(u, http.MethodPost, f.base+"/work-items", draft)
					requireNativeStatus(t, r, http.StatusOK)
				}
				decodeHubResponse(t, r, &own)
				if i == 0 {
					sharedVisualIssue = own
				}
				if own.State != "Triage" || own.Type != []string{"bug", "change_request"}[i%2] {
					t.Fatalf("requested %q: state=%s", state, own.State)
				}
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET duplicate_challenge=0 WHERE id=?", f.project)
			hiddenImage := record
			hiddenImage.ID = conversation.NewAttachmentID()
			requireNativeStatus(t, request(member, http.MethodPost, f.base+"/attachment-metadata", hiddenImage), http.StatusCreated)
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/work-items/"+string(own.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "unreadable-visual"}, Body: "https://figma.com/design/example " + hiddenImage.Markdown("org_security")}), http.StatusUnprocessableEntity)
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE issues SET actor_kind=NULL,actor_principal_id=NULL WHERE project_id=?", f.project)
			attachmentResponse = request(u, http.MethodGet, f.base+"/attachment-metadata/"+record.ID, nil)
			requireNativeStatus(t, attachmentResponse, http.StatusOK)
			var fetched attachment.Metadata
			decodeHubResponse(t, attachmentResponse, &fetched)
			if fetched.ContentType != "image/png" || len(fetched.ReferencedBy) != 6 {
				t.Fatalf("attachment=%#v", fetched)
			}
			requireNativeStatus(t, request(member, http.MethodPost, f.base+"/attachment-metadata/"+hiddenImage.ID+"/reference", map[string]any{"work_item_id": sharedVisualIssue.WorkItemID}), http.StatusNoContent)
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/work-items/"+string(sharedVisualIssue.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "readable-legacy-visual"}, Body: "https://figma.com/design/example " + hiddenImage.Markdown("org_security")}), http.StatusOK)
			attachmentResponse = request(u, http.MethodGet, f.base+"/attachment-metadata/"+hiddenImage.ID, nil)
			requireNativeStatus(t, attachmentResponse, http.StatusOK)
			decodeHubResponse(t, attachmentResponse, &fetched)
			if len(fetched.ReferencedBy) != 2 {
				t.Fatalf("shared legacy attachment references=%v", fetched.ReferencedBy)
			}
			requireNativeStatus(t, request(u, http.MethodPost, f.base+"/attachment-metadata/"+record.ID+"/reference", map[string]any{"work_item_id": own.WorkItemID}), http.StatusNoContent)
			requireNativeStatus(t, request(u, http.MethodDelete, f.base+"/attachment-metadata/"+record.ID, nil), http.StatusForbidden)
			for _, target := range []string{"Backlog", "Todo", "Cancelled"} {
				var report tracker.NativeIssue
				r := request(u, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "promote-" + target}, Type: "bug", Title: "Intake report", Body: reporterBugSections + issueContractTestSections})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &report)
				r = request(member, http.MethodPost, f.base+"/work-items/"+string(report.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "promotion-" + target}, ExpectedRevision: report.Revision, State: target, Reason: "user_requested"})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &report)
				if report.State != target {
					t.Fatalf("promoted state=%s", report.State)
				}
			}
			var other tracker.NativeIssue
			r := request(member, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "member-file"}, Title: "Member report", Body: nativeContractTestBody})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &other)
			if other.State != "Todo" {
				t.Fatalf("member default=%s", other.State)
			}
			r = request(member, http.MethodPatch, f.base+"/work-items/"+string(other.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "member-type-edit"}, ExpectedRevision: other.Revision, Type: new("change_request")})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &other)
			if other.Type != "change_request" {
				t.Fatalf("member edit type=%q", other.Type)
			}
			for _, test := range []struct {
				item   tracker.NativeIssue
				state  string
				status int
			}{
				{own, "Todo", http.StatusForbidden},
				{other, "Cancelled", http.StatusNotFound},
				{own, "Cancelled", http.StatusOK},
			} {
				target := f.base + "/work-items/" + string(test.item.WorkItemID) + "/workflow"
				r := request(u, http.MethodPost, target, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + string(test.item.WorkItemID) + test.state}, ExpectedRevision: test.item.Revision, State: test.state, Reason: "user_requested"})
				requireNativeStatus(t, r, test.status)
			}
			requireNativeStatus(t, request(u, http.MethodPatch, f.base+"/work-items/"+string(other.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reporter-type-edit"}, ExpectedRevision: other.Revision, Type: new("change_request")}), http.StatusNotFound)
			var comment tracker.NativeComment
			commentPath := f.base + "/work-items/" + string(own.WorkItemID) + "/comments"
			r = request(u, http.MethodPost, commentPath, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Tester observation"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &comment)
			r = request(u, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-own"}, ExpectedRevision: comment.Revision, Body: "Updated observation"})
			requireNativeStatus(t, r, http.StatusOK)
			original := comment
			decodeHubResponse(t, r, &comment)
			if comment.Body != "Updated observation" || comment.Actor != original.Actor || !comment.CreatedAt.Equal(original.CreatedAt) || comment.EditedBy == nil || comment.Revision != 2 {
				t.Fatalf("edited comment=%#v", comment)
			}
			for _, actor := range []hostedSecurityUser{member, owner} {
				want := http.StatusForbidden
				if test.transport == "key" && actor.identity.Subject == owner.identity.Subject {
					want = http.StatusNotFound
				}
				for _, deleted := range []bool{false, true} {
					r = request(actor, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("deny-%s-%t", actor.identity.Subject, deleted)}, ExpectedRevision: comment.Revision, Body: "Replacement", Deleted: deleted})
					requireNativeStatus(t, r, want)
				}
			}
			parent := comment
			parent.Body = "Updated observation\n![Screenshot](attachment:" + record.ID + ")"
			r = request(u, http.MethodPatch, commentPath+"/"+parent.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "attach-comment"}, ExpectedRevision: parent.Revision, Body: parent.Body})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &parent)
			r = request(member, http.MethodPost, commentPath, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "member-comment"}, Body: "Operator observation"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &comment)
			r = request(u, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-other"}, ExpectedRevision: comment.Revision, Body: "Replacement"})
			requireNativeStatus(t, r, http.StatusForbidden)
			r = request(u, http.MethodPatch, commentPath+"/"+comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "delete-other"}, ExpectedRevision: comment.Revision, Deleted: true})
			requireNativeStatus(t, r, http.StatusForbidden)
			r = request(u, http.MethodPatch, commentPath+"/"+parent.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "stale-delete"}, ExpectedRevision: 1, Deleted: true})
			requireNativeStatus(t, r, http.StatusConflict)
			deletion := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "delete-own"}, ExpectedRevision: parent.Revision, Deleted: true}
			for range 2 {
				r = request(u, http.MethodPatch, commentPath+"/"+parent.ID, deletion)
				requireNativeStatus(t, r, http.StatusOK)
				var deleted tracker.NativeComment
				decodeHubResponse(t, r, &deleted)
				if !deleted.Deleted || deleted.Body != "" || deleted.Actor != parent.Actor || !deleted.CreatedAt.Equal(parent.CreatedAt) || deleted.Revision != parent.Revision+1 {
					t.Fatalf("deleted comment=%#v", deleted)
				}
			}
			for _, actor := range []hostedSecurityUser{u, member} {
				r = request(actor, http.MethodGet, commentPath, nil)
				requireNativeStatus(t, r, http.StatusOK)
				var comments tracker.Page[tracker.NativeComment]
				decodeHubResponse(t, r, &comments)
				if len(comments.Items) != 2 || !comments.Items[0].Deleted || comments.Items[0].Body != "" || comments.Items[1].Body != "Operator observation" {
					t.Fatalf("discussion=%#v", comments.Items)
				}
			}
			r = request(u, http.MethodPatch, commentPath+"/"+parent.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-deleted"}, ExpectedRevision: parent.Revision + 1, Body: "Restored"})
			requireNativeStatus(t, r, http.StatusUnprocessableEntity)
			var snapshot tracker.NativeComment
			var encoded string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM collaboration_versions WHERE record_id=? AND revision=?", parent.ID, parent.Revision).Scan(&encoded); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil || snapshot.Body != parent.Body || snapshot.Deleted {
				t.Fatalf("revision=%#v, error=%v", snapshot, err)
			}
			var references int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_references WHERE attachment_id=? AND comment_id=?", record.ID, parent.ID).Scan(&references); err != nil || references != 1 {
				t.Fatalf("attachment references=%d, error=%v", references, err)
			}
			for _, suffix := range []string{"", "/comments", "/history"} {
				r := request(u, http.MethodGet, f.base+"/work-items/"+string(other.WorkItemID)+suffix, nil)
				requireNativeStatus(t, r, http.StatusNotFound)
			}
			for _, test := range []struct{ method, suffix string }{{http.MethodPost, "/dependencies"}, {http.MethodPost, "/restore"}, {http.MethodPost, "/archive"}} {
				r := request(u, test.method, f.base+"/work-items/"+string(other.WorkItemID)+test.suffix, map[string]any{"idempotency_key": "denied"})
				requireNativeStatus(t, r, http.StatusForbidden)
			}
			auditBytes, checkpoint, err := releaseaudit.SnapshotBytes(releaseaudit.Snapshot{
				Identity: releaseaudit.Identity{ProjectID: string(f.project), Repository: "example-org/example-project", Commit: strings.Repeat("a", 40), ReleaseSeries: "release/v1.0", RunID: "run", Attempt: "1", JobID: "audit", RunURL: "https://example.test/runs/1"},
				Sequence: 1, Status: "incomplete", BundleCommit: releaseaudit.BundleCommit, BundleDigest: releaseaudit.BundleDigest,
				Allowance: releaseaudit.Allowance{Invocations: 20}, Ledger: []byte(`[]`),
			})
			if err != nil {
				t.Fatal(err)
			}
			auditPart := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "audit.txt", ContentType: "text/plain", Size: int64(len(auditBytes)), SHA256: artifact.Digest(auditBytes)}
			requireNativeStatus(t, request(member, http.MethodPost, f.base+"/attachment-metadata", auditPart), http.StatusCreated)
			checkpoint.Parts = []releaseaudit.Part{{AttachmentID: auditPart.ID, Index: 0, Bytes: len(auditBytes), Digest: auditPart.SHA256}}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET checkout_repository=? WHERE id=?", checkpoint.Identity.Repository, f.project)
			auditRequest := struct {
				tracker.Mutation
				releaseaudit.Checkpoint
			}{tracker.Mutation{IdempotencyKey: "release-audit"}, checkpoint}
			requireNativeStatus(t, request(member, http.MethodPost, f.base+"/release-audits", auditRequest), http.StatusOK)
			for _, access := range []struct {
				method, suffix string
				body           any
			}{
				{http.MethodGet, "/release-audits", nil},
				{http.MethodGet, "/release-audits/" + checkpoint.ID, nil},
				{http.MethodPost, "/release-audits", auditRequest},
				{http.MethodGet, "/attachment-metadata/" + auditPart.ID, nil},
			} {
				t.Run(access.method+access.suffix, func(t *testing.T) {
					requireNativeStatus(t, request(u, access.method, f.base+access.suffix, access.body), http.StatusNotFound)
					requireNativeStatus(t, request(member, access.method, f.base+access.suffix, access.body), http.StatusOK)
				})
			}
			attachmentContexts := make(chan context.Context, 1)
			f.service.echo.GET("/release-audit-attachment-test", func(c echo.Context) error {
				attachmentContexts <- operatortool.BindConnection(c.Request().Context(), "audit-attachment", "test")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			requireNativeStatus(t, request(u, http.MethodGet, "/organizations/org_security/release-audit-attachment-test", nil), http.StatusOK)
			attachmentContext := <-attachmentContexts
			executor := hostedOperatorExecutor{f.service}
			if err := executor.OpenConnection(attachmentContext); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment} {
				raw, err := json.Marshal(map[string]string{"project_id": string(f.project), "attachment_id": auditPart.ID})
				if err != nil {
					t.Fatal(err)
				}
				_, err = executor.Execute(attachmentContext, operatortool.Call{Name: name, Arguments: raw})
				if !errors.Is(err, explain.ErrNotFound) {
					t.Fatalf("%s release audit evidence: %v", name, err)
				}
			}
			operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", u.identity.Subject)
			requireNativeStatus(t, request(u, http.MethodGet, f.base+"/work-items", nil), http.StatusNotFound)
		})
	}
}

func TestReporterMCPFilesIntoTriage(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: "org_security", project: f.project}, nativeFixtureStates(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			executor := hostedOperatorExecutor{f.service}
			definitions, err := executor.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, definition := range definitions {
				seen[definition.Name] = true
			}
			for _, name := range []string{operatortool.EditItem, operatortool.FileIssue, operatortool.AddComment, operatortool.EditComment, operatortool.MoveItem} {
				if !seen[name] {
					t.Fatalf("Reporter catalog omits %s", name)
				}
			}
			for _, name := range []string{operatortool.SetDependency, operatortool.RestoreItem, operatortool.DeleteAttachment} {
				if seen[name] {
					t.Fatalf("Reporter catalog advertises %s", name)
				}
			}
			for _, definition := range operatortool.AdministrationCatalog() {
				if seen[definition.Name] {
					t.Fatalf("Reporter catalog advertises administration tool %s", definition.Name)
				}
			}

			raw, err := json.Marshal(map[string]any{"project_id": f.project, "request_id": "missing-type", "title": "Tester report"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
			var refusal *operatortool.RequestError
			if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "bug") || !strings.Contains(refusal.Message, "change_request") {
				t.Fatalf("missing type error=%v", err)
			}
			withoutEnvironment := strings.ReplaceAll(reporterBugSections, "\n## Environment\nFirefox on Linux, build example.", "")
			for index, test := range []struct {
				kind, body, missing string
				context             map[string]any
			}{
				{"bug", strings.ReplaceAll(reporterBugSections, "## Actual\nThe change disappears.\n", ""), "Actual", nil},
				{"bug", strings.ReplaceAll(reporterBugSections, "The change disappears.", ""), "Actual", nil},
				{"change_request", strings.ReplaceAll(reporterChangeSections, "## Why\nKeep the task in view.", ""), "Why", nil},
				{"change_request", strings.ReplaceAll(reporterChangeSections, "Keep the task in view.", ""), "Why", nil},
				{"bug", withoutEnvironment, "Environment", nil},
				{"bug", withoutEnvironment, "Environment", map[string]any{"browser": "Chrome 130", "os": "macOS 15"}},
				{"bug", withoutEnvironment, "Environment", map[string]any{"app_build": "v1.4.2", "browser": " "}},
				{"bug", reporterBugSections, "report_context", map[string]any{"browser": "Chrome 130", "referrer": "secret-referrer"}},
				{"bug", reporterBugSections, "report_context", map[string]any{"url": "https://example.test/" + strings.Repeat("secret-path", 200)}},
				{"bug", reporterBugSections, "report_context", map[string]any{"browser": "secret-browser\nsecond line"}},
				{"bug", reporterBugSections, "report_context", map[string]any{"viewport": 1280}},
			} {
				arguments := map[string]any{"project_id": f.project, "request_id": fmt.Sprintf("incomplete-%d", index), "title": "Incomplete report", "type": test.kind, "description": test.body}
				if test.context != nil {
					arguments["report_context"] = test.context
				}
				args, err := json.Marshal(arguments)
				if err != nil {
					t.Fatal(err)
				}
				_, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: args})
				if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, test.missing) || strings.Contains(refusal.Message, "secret") {
					t.Fatalf("missing %s: %v", test.missing, err)
				}
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected filings created %d issues: %v", count, err)
			}
			for i, state := range []string{"", "Todo", "Cancelled", "Triage", "Missing"} {
				args := map[string]any{"project_id": f.project, "request_id": fmt.Sprintf("file-%d", i), "title": "Tester report", "type": []string{"bug", "change_request"}[i%2], "description": reporterTestBody([]string{"bug", "change_request"}[i%2]), "state": state}
				if i == 0 {
					args["description"] = withoutEnvironment
					args["report_context"] = map[string]any{"url": "https://app.example.test/settings", "app_build": "v1.4.2", "browser": "Chrome 130", "os": "macOS 15", "viewport": "1440x900", "captured_at": "2026-10-13T12:00:00Z"}
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				if i > 0 {
					var challenged struct {
						Data duplicateChallenge `json:"data"`
					}
					if err := json.Unmarshal(result.Content, &challenged); err != nil || challenged.Data.ChallengeToken == "" || len(challenged.Data.PossibleDuplicates) == 0 {
						t.Fatalf("Reporter was not challenged: %s", result.Content)
					}
					args["challenge_token"] = challenged.Data.ChallengeToken
					numbers := make([]int, 0, len(challenged.Data.PossibleDuplicates))
					for _, match := range challenged.Data.PossibleDuplicates {
						numbers = append(numbers, match.Number)
					}
					args["not_duplicate_of"] = numbers
					if raw, err = json.Marshal(args); err != nil {
						t.Fatal(err)
					}
					if result, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw}); err != nil {
						t.Fatal(err)
					}
				}
				var content struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.Data.State != "Triage" || content.Data.Type != []string{"bug", "change_request"}[i%2] {
					t.Fatalf("result=%s", result.Content)
				}
				wantContext := (*tracker.ReportContext)(nil)
				if i == 0 {
					wantContext = &tracker.ReportContext{URL: "https://app.example.test/settings", AppBuild: "v1.4.2", Browser: "Chrome 130", OS: "macOS 15", Viewport: "1440x900", CapturedAt: "2026-10-13T12:00:00Z"}
				}
				if !reflect.DeepEqual(content.Data.ReportContext, wantContext) {
					t.Fatalf("report context=%+v", content.Data.ReportContext)
				}
				if i == 0 {
					raw, err := json.Marshal(map[string]any{"project_id": f.project, "identifier": content.Data.WorkItemID, "request_id": "correct-report", "expected_revision": fmt.Sprint(content.Data.Revision), "title": "Tester report", "body": reporterBugSections + issueContractTestSections})
					if err != nil {
						t.Fatal(err)
					}
					result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.EditItem, Arguments: raw})
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(result.Content, &content); err != nil {
						t.Fatal(err)
					}
					if content.Data.ReporterStatus != "submitted" || content.Data.Body != reporterBugSections+issueContractTestSections || len(content.Data.IssueContract.ConfirmedSections) != 0 {
						t.Fatalf("MCP edit=%s", result.Content)
					}
				}
			}
		})
	}
}

func TestReporterPersonalKeyIntersection(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			catalog, err := operatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"first", "second"} {
				key, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: name, Scope: apikey.ScopeWrite, Days: 30, Projects: []string{string(f.project)}})
				if err != nil {
					t.Fatal(err)
				}
				send := func(method, path string, body any) *httptest.ResponseRecorder {
					t.Helper()
					if deployment == "shared" {
						raw, err := json.Marshal(body)
						if err != nil {
							t.Fatal(err)
						}
						shared := hostedSharedFixture{hostedSecurityFixture: f.hostedSecurityFixture, key: hostedSharedKey(7)}
						return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: method, target: path, body: string(raw), bearer: key.Token})
					}
					return performHubAPIRequest(t, f.service, method, path, key.Token, body)
				}
				r := send(http.MethodGet, f.base+"/work-items", nil)
				requireNativeStatus(t, r, http.StatusOK)
				r = send(http.MethodPatch, f.base+"/work-items/wi_other", map[string]any{"idempotency_key": "denied"})
				requireNativeStatus(t, r, http.StatusNotFound)
			}
			if _, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: "admin", Scope: apikey.ScopeAdmin, Days: 30}); err == nil {
				t.Fatal("reporter created admin key")
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET role='viewer' WHERE user_id=?", f.user.identity.Subject)
			_, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: string(f.project), Action: operatortool.FileIssue})
			if err == nil {
				t.Fatal("stale reporter authority survived local downgrade")
			}
		})
	}
}

func TestReporterRoleAssignment(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, _ := newHostedKeyMCPFixture(t, deployment, "owner")
			member := f.hostedSecurityFixture.user(t, "tester", "member", "tester@example.test", "write", "")
			var membership string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT membership_id FROM hosted_members WHERE user_id=?", member.identity.Subject).Scan(&membership); err != nil {
				t.Fatal(err)
			}
			call := func(name string, arguments map[string]any, target any) {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "Reporter assignment", "version": "1"}}}})
				if err != nil {
					t.Fatal(err)
				}
				response := reporterEntryRequest(t, f, deployment, "", "web-session", http.MethodPost, "/api/v2/organizations/org_security/mcp", raw, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": name})
				requireNativeStatus(t, response, http.StatusOK)
				var frame struct {
					Result struct {
						IsError    bool `json:"isError"`
						Structured struct {
							Action chatpkg.Action  `json:"action"`
							Output json.RawMessage `json:"output"`
						} `json:"structuredContent"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &frame)
				if frame.Result.IsError || frame.Result.Structured.Action.Status != chatpkg.ActionSucceeded {
					t.Fatalf("%s failed: %s", name, response.Body.String())
				}
				if err := json.Unmarshal(frame.Result.Structured.Output, target); err != nil {
					t.Fatal(err)
				}
			}
			var changed struct {
				Data hostedMemberView `json:"data"`
			}
			call(operatortool.MemberRole, map[string]any{"request_id": "assign-reporter", "member_id": membership, "role": "reporter"}, &changed)
			if changed.Data.Role != "reporter" {
				t.Fatalf("changed role=%s", changed.Data.Role)
			}
			for _, name := range []string{"first", "second"} {
				var invitation struct {
					Data hostedInvitationView `json:"data"`
				}
				call(operatortool.InvitationSend, map[string]any{"request_id": "invite-" + name, "email": name + "@example.test", "role": "reporter", "grants": []hostedMemberGrant{{ProjectID: string(f.project), Write: true}}}, &invitation)
				if invitation.Data.Role != "reporter" {
					t.Fatalf("invitation role=%s", invitation.Data.Role)
				}
			}
			var role string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT role FROM hosted_members WHERE membership_id=?", membership).Scan(&role); err != nil || role != "reporter" {
				t.Fatalf("persisted role=%s err=%v", role, err)
			}

		})
	}
}

func TestReporterCustomHostedProjectCreation(t *testing.T) {
	for _, test := range []struct {
		name         string
		states, want []tracker.NativeState
		promotion    string
	}{
		{
			name: "conventional destinations",
			states: []tracker.NativeState{
				{Name: "Planned", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}},
				{Name: "Planned", Transitions: []string{"Todo"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{}},
			},
			promotion: "Todo",
		},
		{
			name: "custom destinations",
			states: []tracker.NativeState{
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Ready"}},
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			promotion: "Ready",
		},
		{
			name: "declared custom intake",
			states: []tracker.NativeState{
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
				{Name: "Triage", OperatorOnly: true, Dispatchable: true, Transitions: []string{"Working"}},
			},
			want: []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Working"}},
				{Name: "Ready", Transitions: []string{"Working"}},
				{Name: "Working", Dispatchable: true, Transitions: []string{}},
			},
			promotion: "Working",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
			reporter := f.hostedSecurityFixture.user(t, "reporter", "reporter", "reporter@example.test", "", "")
			member := f.hostedSecurityFixture.user(t, "member", "member", "member@example.test", "", "")
			for _, entry := range []string{"JSON", "MCP"} {
				t.Run(entry, func(t *testing.T) {
					var project tracker.NativeProject
					if entry == "JSON" {
						response := f.request(t, f.user, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"idempotency_key": "create-json", "name": "JSON custom project", "grant_access": true, "states": test.states})
						requireNativeStatus(t, response, http.StatusCreated)
						decodeHubResponse(t, response, &project)
					} else {
						call := projectCall(t, "create_hosted_project", "", "create-mcp", operatortool.HostedProjectCreateInput{Name: "MCP custom project", GrantAccess: true, States: &test.states})
						rawResult, err := (hubProjectExecutor{service: f.service}).Execute(ctx, call)
						if err != nil {
							t.Fatalf("creation error=%T %+v", err, err)
						}
						var result chatpkg.Action
						if err := json.Unmarshal(rawResult.Content, &result); err != nil {
							t.Fatal(err)
						}
						if result.Status != chatpkg.ActionSucceeded {
							t.Fatalf("creation=%+v", result)
						}
						if err := json.Unmarshal([]byte(result.Result), &project); err != nil {
							t.Fatal(err)
						}
					}
					if !reflect.DeepEqual(project.States, test.want) {
						t.Fatalf("workflow=%+v, want %+v", project.States, test.want)
					}
					f.project = project.ID
					f.grantContext(ctx, t, reporter, true, false)
					f.grantContext(ctx, t, member, true, false)
					base := "/api/v2/organizations/org_security/projects/" + string(project.ID)
					for i, state := range []string{"", "Todo", "Planned", "Cancelled", "Triage", "Unknown"} {
						draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("file-%s-%d", entry, i)}, Type: "bug", Title: "Custom workflow report", Body: reporterBugSections + issueContractTestSections, State: state}
						response := f.request(t, reporter, http.MethodPost, base+"/work-items", draft)
						if i > 0 {
							requireNativeStatus(t, response, http.StatusConflict)
							var challenge duplicateChallenge
							decodeHubResponse(t, response, &challenge)
							if challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) == 0 {
								t.Fatalf("Reporter was not challenged: %s", response.Body.String())
							}
							draft.ChallengeToken = challenge.ChallengeToken
							for _, match := range challenge.PossibleDuplicates {
								draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
							}
							response = f.request(t, reporter, http.MethodPost, base+"/work-items", draft)
						}
						requireNativeStatus(t, response, http.StatusOK)
						var issue tracker.NativeIssue
						decodeHubResponse(t, response, &issue)
						if issue.State != "Triage" {
							t.Fatalf("requested=%q resulting=%q", state, issue.State)
						}
						seedNativeFixturePolicyContext(ctx, t, f.service, "org_security/"+string(issue.ProjectID))
						response = f.request(t, member, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("promote-%s-%d", entry, i)}, ExpectedRevision: issue.Revision, State: test.promotion, Reason: "user_requested"})
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &issue)
						if issue.State != test.promotion {
							t.Fatalf("promotion=%q, want %q", issue.State, test.promotion)
						}
					}
				})
			}
		})
	}
}

func TestNativeProjectCreationWorkflowBounds(t *testing.T) {
	for _, hosted := range []bool{false, true} {
		t.Run(strconv.FormatBool(hosted), func(t *testing.T) {
			var request func(any) *httptest.ResponseRecorder
			var createdStatus int
			var service *Service
			if hosted {
				f, _ := newHostedKeyMCPFixture(t, "dedicated", "owner")
				service = f.service
				request = func(body any) *httptest.ResponseRecorder {
					return f.request(t, f.user, http.MethodPost, "/api/v2/organizations/org_security/projects", body)
				}
				createdStatus = http.StatusCreated
			} else {
				f := newNativeFixture(t, nil, "", "bounds")
				service = f.service
				request = func(body any) *httptest.ResponseRecorder {
					return performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects", testHubAdminToken, body)
				}
				createdStatus = http.StatusOK
			}
			for _, test := range []struct {
				name    string
				count   int
				triage  bool
				allowed bool
			}{
				{"49 custom lanes", 49, false, true},
				{"50 custom lanes", 50, false, false},
				{"50 including Triage", 50, true, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					states := make([]tracker.NativeState, test.count)
					for i := range states {
						states[i] = tracker.NativeState{Name: fmt.Sprintf("Lane %d", i), Transitions: []string{}}
					}
					if test.triage {
						states[0].Name = "Triage"
					}
					body := map[string]any{"idempotency_key": test.name, "name": test.name, "states": states}
					if hosted {
						body["grant_access"] = true
					}
					var before, after int
					if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM projects").Scan(&before); err != nil {
						t.Fatal(err)
					}
					response := request(body)
					if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM projects").Scan(&after); err != nil {
						t.Fatal(err)
					}
					if !test.allowed {
						requireNativeStatus(t, response, http.StatusUnprocessableEntity)
						if after != before {
							t.Fatalf("rejected creation changed project count: %d to %d", before, after)
						}
						return
					}
					requireNativeStatus(t, response, createdStatus)
					if after != before+1 {
						t.Fatalf("creation changed project count: %d to %d", before, after)
					}
					var project tracker.NativeProject
					decodeHubResponse(t, response, &project)
					if len(project.States) != 50 || project.States[0].Name != "Triage" {
						t.Fatalf("workflow=%+v", project.States)
					}
				})
			}
		})
	}
}

func TestReporterWorkflowTemplates(t *testing.T) {
	const repairBody = "## Acceptance criteria\nRepair the verified failure.\n\n## Must not break\nPreserve authority.\n\n## How we know it worked\nRun make check-land.\n\n## Invariants touched\nPreserve INV-3.\n\n## Safety regression\nPreserve regression tables."
	const repairDefinition = "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports from `.github/workflows/verify.yaml` on `release/v2.0` enter Todo with Urgent priority.\n"
	for _, test := range []struct {
		name, prompt, body string
		want               config.ReporterContracts
		required           []string
		repair             bool
		shortContract      bool
	}{
		{"default", "", reporterBugSections, config.ReporterContracts{"bug": {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"}, "change_request": {"Current behavior", "Desired behavior", "Why"}}, nil, false, false},
		{"override", "## Issue Contract\n- Finish\n- Preserve\n- Prove\n\n## Reporter Contract\n### bug\n- Reproduction\n- Outcome\n### change_request\n- Motivation", "## Reproduction\nOpen settings.\n## Outcome\nSaving fails.", config.ReporterContracts{"bug": {"Reproduction", "Outcome"}, "change_request": {"Motivation"}}, []string{"Finish", "Preserve", "Prove"}, false, false},
		{"engineering overlap", "## Reporter Contract\n### bug\n- Acceptance criteria\n- Must not break\n- How we know it worked", issueContractTestSections, config.ReporterContracts{"bug": {"Acceptance criteria", "Must not break", "How we know it worked"}, "change_request": {"Current behavior", "Desired behavior", "Why"}}, nil, false, false},
		{"scheduled repair discovery", repairDefinition + strings.ReplaceAll(repairBody, "## ", "### "), reporterBugSections, config.ReporterContracts{"bug": {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"}, "change_request": {"Current behavior", "Desired behavior", "Why"}}, []string{"Acceptance criteria", "Must not break", "How we know it worked", "Invariants touched", "Safety regression"}, true, false},
		{"scheduled three-section release family", repairDefinition + strings.ReplaceAll(repairBody, "## ", "### "), reporterBugSections, config.ReporterContracts{"bug": {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"}, "change_request": {"Current behavior", "Desired behavior", "Why"}}, nil, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prompt := test.prompt
			expectedRepairBody, branch := repairBody, "release/v2.0"
			if test.shortContract {
				expectedRepairBody, _, _ = strings.Cut(repairBody, "\n\n## Invariants touched")
				prompt = strings.Replace(repairDefinition, "- Invariants touched\n- Safety regression\n", "", 1) + strings.ReplaceAll(expectedRepairBody, "## ", "### ")
				prompt = strings.Replace(prompt, "`release/v2.0`", "`release/v*`", 1)
				prompt = strings.Replace(prompt, "Urgent priority", "High priority", 1)
				branch = "release/v*"
			}
			required := test.required
			if required == nil {
				required = []string{"Acceptance criteria", "Must not break", "How we know it worked"}
			}
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", "reporter")
			definition, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
				Workflow:  []byte(prompt),
				Config:    []byte("schema: 1\ntracker:\n  kind: hub_native\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Blocked, role: holding}\n    - {name: Done, role: terminal}\n"),
				HasConfig: true, ConfigPath: "detent.yaml",
			})
			if err != nil {
				t.Fatal(err)
			}
			definition.Definition.Revision = strings.Repeat("a", 40)
			descriptor, err := config.ResolvePolicy(definition)
			if err != nil {
				t.Fatal(err)
			}
			if test.repair {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository='example-org/example-project', github_repository_enabled=0 WHERE id=?", f.project); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.approvePolicy(t.Context(), "org_security/"+string(f.project), "test-owner", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}); err != nil {
				t.Fatal(err)
			}
			executor := hostedOperatorExecutor{f.service}
			args, err := json.Marshal(map[string]any{"project_id": f.project})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkConfig, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			var read struct {
				Data struct {
					Templates     config.ReporterContracts `json:"reporter_templates"`
					IssueContract nativeWorkIssueContract  `json:"issue_contract"`
				} `json:"data"`
			}
			if err := json.Unmarshal(result.Content, &read); err != nil || !reflect.DeepEqual(read.Data.Templates, test.want) {
				t.Fatalf("work_config=%s error=%v", result.Content, err)
			}
			if !reflect.DeepEqual(read.Data.IssueContract.RequiredSections, required) {
				t.Fatalf("work_config required sections=%v, want %v", read.Data.IssueContract.RequiredSections, required)
			}
			if test.repair {
				priority := 0
				if test.shortContract {
					priority = 1
				}
				want := &nativeWorkScheduledRepair{PolicyID: descriptor.ID, Repository: "example-org/example-project", Workflow: ".github/workflows/verify.yaml", Branches: []string{branch}, Body: expectedRepairBody, EvidenceSchema: 1, Priority: priority}
				if !reflect.DeepEqual(read.Data.IssueContract.ScheduledRepair, want) {
					t.Fatalf("scheduled repair=%+v want=%+v", read.Data.IssueContract.ScheduledRepair, want)
				}
			} else if read.Data.IssueContract.ScheduledRepair != nil {
				t.Fatalf("unapproved scheduled repair=%+v", read.Data.IssueContract.ScheduledRepair)
			}
			response := f.request(t, f.user, http.MethodGet, f.base, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var project struct {
				Templates config.ReporterContracts `json:"reporter_templates"`
			}
			decodeHubResponse(t, response, &project)
			if !reflect.DeepEqual(project.Templates, test.want) {
				t.Fatalf("project templates=%+v", project.Templates)
			}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "report"}, Title: "Report", Type: "bug", Body: test.body}
			response = f.request(t, f.user, http.MethodPost, f.base+"/work-items", request)
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			if issue.State != "Triage" {
				t.Fatalf("state=%s", issue.State)
			}
			if len(issue.IssueContract.ConfirmedSections) != 0 {
				t.Fatalf("Reporter filing confirmed engineering sections: %+v", issue.IssueContract.ConfirmedSections)
			}
			scope := nativeScope{organization: "org_security", project: f.project}
			_, id, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			claimable, reason, err := nativeIssueContractClaimable(t.Context(), f.service.database.db, scope, id)
			if err != nil || !claimable || reason != "" {
				t.Fatalf("Triage contract exemption=%t, %s, %v", claimable, reason, err)
			}
			member := f.hostedSecurityFixture.user(t, "member", "member", "member@example.test", "write", "")
			response = f.request(t, member, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "promote"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
			if test.name != "engineering overlap" {
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			} else {
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
			}
			claimable, reason, err = nativeIssueContractClaimable(t.Context(), f.service.database.db, scope, id)
			wantReason := required[0]
			var bodyBuilder strings.Builder
			bodyBuilder.WriteString(test.body)
			for _, heading := range required {
				bodyBuilder.WriteString("\n## " + heading + "\nVerified project requirement.\n")
			}
			engineeredBody := bodyBuilder.String()
			if test.name == "engineering overlap" {
				wantReason = "A human must confirm"
				engineeredBody = test.body
			}
			if err != nil || test.name == "engineering overlap" && (claimable || !strings.Contains(reason, wantReason)) {
				t.Fatalf("promoted contract=%t, %s, %v", claimable, reason, err)
			}
			response = f.request(t, member, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "engineer"}, ExpectedRevision: issue.Revision, Body: new(engineeredBody)})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if issue.State == "Triage" {
				response = f.request(t, member, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "promote-confirmed"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
			}
			claimable, reason, err = nativeIssueContractClaimable(t.Context(), f.service.database.db, scope, id)
			if err != nil || !claimable || reason != "" {
				t.Fatalf("engineered contract=%t, %s, %v", claimable, reason, err)
			}
		})
	}
}
