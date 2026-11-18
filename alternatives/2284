package nativework_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeVisualReferenceWrites(t *testing.T) {
	t.Parallel()
	for _, writer := range []string{"title", "body", "discussion", "review", "artifacts", "evidence"} {
		t.Run(writer, func(t *testing.T) {
			f := hubfixture.NewChange(t, nil)
			other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "other")
			otherIssue := other.Create(t, "other-work")
			credential, err := f.Credential(t, f.Token)
			if err != nil {
				t.Fatal(err)
			}
			uploader := credential.ID
			upload := func(project tracker.NativeProject, mime string, deleted bool) attachment.Metadata {
				t.Helper()
				m := attachment.Metadata{ID: conversation.NewAttachmentID(), ProjectID: string(project.ID), Uploader: uploader, Name: "upload", ContentType: mime, Size: 100, SHA256: policy.Digest([]byte(mime))}
				if strings.HasPrefix(mime, "image/") {
					m.Width, m.Height = 1, 1
				}
				m.CreatedAt = time.Now().UTC()
				if deleted {
					m.DeletedAt = &m.CreatedAt
				}
				if err := f.Hub.Config.TenantRepository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: project.OrganizationID, Metadata: m, Kind: "attachment"})
				}); err != nil {
					t.Fatal(err)
				}
				return m
			}
			image := upload(f.Project, "image/png", false)
			pdf := upload(f.Project, "application/pdf", false)
			text := upload(f.Project, "text/plain", false)
			deleted := upload(f.Project, "image/png", true)
			foreign := upload(other.Project, "image/png", false)
			foreignBody := "https://figma.com/design/example " + foreign.Markdown(string(other.Project.OrganizationID))
			response := f.Hub.Request(t, http.MethodPost, other.Base+"/work-items/"+string(otherIssue.WorkItemID)+"/changes", other.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "other-change"}, Title: "Other visual", Body: foreignBody})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var otherChange tracker.ChangeRequest
			hubfixture.Decode(t, response, &otherChange)
			if otherChange.Body != foreignBody {
				t.Fatal("other project is not usable")
			}
			if err := f.Hub.Config.TenantRepository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.BindAttachment(t.Context(), attachmentstore.Reference{ProjectID: f.Project.ID, AttachmentID: image.ID, WorkItemID: f.Issue.WorkItemID})
			}); err != nil {
				t.Fatal(err)
			}

			discussionPath := f.Path + "/discussion"
			if writer == "discussion" {
				linked := f.Create(t, "linked-work")
				response := f.Hub.Request(t, http.MethodPost, f.Path[:strings.LastIndex(f.Path, "/")], f.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				hubfixture.Decode(t, response, &f.Change)
				f.Path = f.Base + "/work-items/" + string(f.Issue.WorkItemID) + "/changes/" + f.Change.ID
				discussionPath = f.Base + "/work-items/" + string(linked.WorkItemID) + "/changes/" + f.Change.ID + "/discussion"
			}

			current := f.Publish(t, "initial", "")
			const visual = "https://figma.com/design/example"
			artifact := func(uri string) tracker.ChangeArtifact {
				return tracker.ChangeArtifact{Kind: "artifact", URI: uri, SHA256: image.SHA256, Availability: "available"}
			}
			canonical := func(m attachment.Metadata) string {
				return "/organizations/" + string(f.Project.OrganizationID) + "/api/v2/projects/" + m.ProjectID + "/attachments/" + m.ID
			}
			build := func(key, body string, artifacts []tracker.ChangeArtifact) (string, any) {
				mutation := tracker.Mutation{IdempotencyKey: key}
				switch writer {
				case "title", "body":
					request := tracker.CreateChange{Mutation: mutation, Title: "Visual change", Body: body}
					if writer == "title" {
						request.Title, request.Body = body, "Change body"
					}
					return f.Path[:strings.LastIndex(f.Path, "/")], request
				case "discussion":
					return discussionPath, tracker.DiscussChange{Mutation: mutation, Body: body}
				case "review":
					return f.Path + "/versions/" + current.ID + "/reviews", tracker.ReviewChange{Mutation: mutation, Decision: "commented", Body: body}
				case "artifacts":
					input := hubfixture.ChangeInput()
					input.Artifacts = artifacts
					return f.Path + "/versions", tracker.PublishChangeVersion{Mutation: mutation, ExpectedVersionID: current.ID, ChangeVersionInput: input}
				default:
					request := hubfixture.CheckResult(current)
					request.Mutation, request.Evidence = mutation, artifacts
					return f.Path + "/versions/" + current.ID + "/checks", request
				}
			}
			send := func(path string, request any, want int) *httptest.ResponseRecorder {
				t.Helper()
				response := f.Hub.Request(t, http.MethodPost, path, f.Token, request)
				hubfixture.RequireStatus(t, response, want)
				if writer == "artifacts" && want == http.StatusOK {
					hubfixture.Decode(t, response, &current)
				}
				return response
			}
			legacyPath, legacy := build("legacy-visual", visual, []tracker.ChangeArtifact{artifact("https://example.test/screenshot.png")})
			send(legacyPath, legacy, http.StatusOK)
			provenance := &tracker.Provenance{Provider: "github", ExternalID: "example-comment", AuthorID: "example-author", CreatedAt: time.Now(), UpdatedAt: time.Now(), ObservedAt: time.Now()}
			if writer == "discussion" {
				request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "legacy-provenance"}, Body: visual, Provenance: provenance}
				send(f.Path+"/discussion", request, http.StatusOK)
			}
			if err := f.Hub.Config.TenantRepository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.Project.ID)
				if err != nil {
					return err
				}
				return w.UpdateProjectContent(t.Context(), f.Project.ID, project.Core.ContentSettingsRevision, true)
			}); err != nil {
				t.Fatal(err)
			}
			send(legacyPath, legacy, http.StatusOK)
			if writer == "discussion" {
				send(f.Path+"/discussion", tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "provenance-replay"}, Body: visual, Provenance: provenance}, http.StatusOK)
				provenance.ExternalID = "new-example-comment"
			}
			if writer == "evidence" {
				current = f.Publish(t, "new-check", current.ID)
			}
			snapshot := func() string {
				t.Helper()
				var change string
				var versions, evidence, refs, history, revisions, receipts, changes, retained int
				var row eventstore.Change
				if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.Change(t.Context(), f.Change.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				value, err := eventstore.NewContent(f.Hub.Config.RunDetails).Change(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				change = string(encoded)
				if err := f.Hub.Admin.QueryRowContext(t.Context(), `SELECT
     (SELECT count(*) FROM change_versions), (SELECT count(*) FROM change_evidence),
     (SELECT count(*) FROM attachment_item_references), (SELECT count(*) FROM collaboration_events),
     (SELECT count(*) FROM collaboration_versions), (SELECT count(*) FROM native_commands),
     (SELECT count(*) FROM change_requests), (SELECT count(*) FROM attachment_item_references WHERE retain_with_work_item)`).Scan(&versions, &evidence, &refs, &history, &revisions, &receipts, &changes, &retained); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal([]any{change, versions, evidence, refs, history, revisions, receipts, changes, retained})
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			cases := []struct {
				name, body string
				artifacts  []tracker.ChangeArtifact
			}{
				{name: "URL only", body: visual + "?secret=hidden", artifacts: []tracker.ChangeArtifact{artifact(visual)}},
				{name: "unrelated upload", body: visual + "\n\n" + image.Markdown(string(f.Project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(image)), artifact("https://example.test/screen.png")}},
				{name: "wrong MIME", body: visual + " " + text.Markdown(string(f.Project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(text))}},
				{name: "missing", body: visual + " ![upload](attachment:att_0123456789abcdef0123456789abcdef)", artifacts: []tracker.ChangeArtifact{artifact("attachment:att_0123456789abcdef0123456789abcdef")}},
				{name: "deleted", body: visual + " " + deleted.Markdown(string(f.Project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(deleted))}},
				{name: "cross project", body: visual + " ![upload](attachment:" + foreign.ID + ")", artifacts: []tracker.ChangeArtifact{artifact("attachment:" + foreign.ID)}},
				{name: "cross project canonical", body: visual + " " + foreign.Markdown(string(f.Project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(foreign))}},
			}
			if writer == "artifacts" || writer == "evidence" {
				cases[2].artifacts[0].SHA256 = text.SHA256
				cases = append(cases, struct {
					name, body string
					artifacts  []tracker.ChangeArtifact
				}{name: "image with query", artifacts: []tracker.ChangeArtifact{artifact("https://example.test/screen.png?secret=hidden")}})
				mislabelled := artifact("https://example.test/screen.png")
				mislabelled.Kind = "log"
				cases = append(cases, struct {
					name, body string
					artifacts  []tracker.ChangeArtifact
				}{name: "visual URI with nonvisual kind", artifacts: []tracker.ChangeArtifact{mislabelled}})
				wrongHash, unavailable := artifact(canonical(image)), artifact(canonical(image))
				wrongHash.SHA256, unavailable.Availability = strings.Repeat("f", 64), "unverified"
				cases = append(cases, struct {
					name, body string
					artifacts  []tracker.ChangeArtifact
				}{name: "wrong digest", artifacts: []tracker.ChangeArtifact{wrongHash}}, struct {
					name, body string
					artifacts  []tracker.ChangeArtifact
				}{name: "unavailable", artifacts: []tracker.ChangeArtifact{unavailable}}, struct {
					name, body string
					artifacts  []tracker.ChangeArtifact
				}{name: "object storage image", artifacts: []tracker.ChangeArtifact{artifact("s3://example-bucket/screen.png")}})
			}
			for i, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					before := snapshot()
					path, request := build(fmt.Sprintf("refusal-%d", i), test.body, test.artifacts)
					if writer == "discussion" {
						request = tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("refusal-%d", i)}, Body: test.body, Provenance: provenance}
					}
					raw := send(path, request, http.StatusUnprocessableEntity).Body.String()
					if !strings.Contains(raw, attachment.VisualReferenceMessage) || !strings.Contains(raw, "invalid_request") || strings.Contains(raw, "hidden") || strings.Contains(raw, "figma.com") {
						t.Fatalf("unsafe refusal=%s", raw)
					}

					location := "body block"
					if writer == "title" {
						location = "title block"
					}
					if writer == "artifacts" || writer == "evidence" {
						location = writer + "["
					}
					if !strings.Contains(raw, location) {
						t.Fatalf("refusal has no field location: %s", raw)
					}
					if after := snapshot(); before != after {
						t.Fatalf("refusal changed records: %s -> %s", before, after)
					}
				})
			}
			t.Run("publication rollback", func(t *testing.T) {
				fresh := upload(f.Project, "image/png", false)
				table := "change_evidence"
				if writer == "title" || writer == "body" {
					table = "change_requests"
				}
				if writer == "artifacts" {
					table = "change_versions"
				}
				if _, err := f.Hub.Admin.ExecContext(t.Context(), "CREATE FUNCTION refuse_visual_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture refusal'; END $$; CREATE TRIGGER refuse_visual_publication BEFORE INSERT ON "+table+" FOR EACH ROW EXECUTE FUNCTION refuse_visual_publication()"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubapi.ReadTimeout)
					defer cancel()
					if _, err := f.Hub.Admin.ExecContext(cleanup, "DROP TRIGGER refuse_visual_publication ON "+table+"; DROP FUNCTION refuse_visual_publication()"); err != nil {
						t.Error(err)
					}
				})
				before := snapshot()
				a := artifact("attachment:" + fresh.ID)
				path, request := build("rollback", visual+" "+fresh.Markdown(string(f.Project.OrganizationID)), []tracker.ChangeArtifact{a})
				send(path, request, http.StatusInternalServerError)
				if after := snapshot(); before != after {
					t.Fatalf("failed publication changed records: %s -> %s", before, after)
				}
			})

			for i, m := range []attachment.Metadata{image, pdf} {
				if writer == "evidence" && i > 0 {
					current = f.Publish(t, fmt.Sprintf("check-version-%d", i), current.ID)
				}
				uri := "attachment:" + m.ID
				if i > 0 {
					uri = canonical(m)
				}
				body := visual + " " + m.Markdown(string(f.Project.OrganizationID)) + ". [Source](https://github.com/example-org/example-project/pull/1)"
				accepted := artifact(uri)
				accepted.SHA256 = m.SHA256
				path, request := build(fmt.Sprintf("paired-%d", i), body, []tracker.ChangeArtifact{accepted})
				send(path, request, http.StatusOK)
				var bound bool
				if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					references, err := r.AttachmentReferences(t.Context(), f.Project.ID, m.ID, corestore.MaxPageSize)
					for _, reference := range references {
						bound = bound || reference.WorkItemID == f.Issue.WorkItemID && reference.CommentID == "" && reference.RetainWithWorkItem
					}
					return err
				}); err != nil || !bound {
					t.Fatalf("accepted upload unbound: %v", err)
				}
			}
			if writer != "artifacts" && writer != "evidence" {
				for i, body := range []string{"![upload](attachment:" + image.ID + ")", visual + " ![upload][shot]\n\n[shot]: attachment:" + image.ID, "https://example.test/docs `https://example.test/screen.png`\n\n```log\nhttps://figma.com/design/example\n```"} {
					path, request := build(fmt.Sprintf("ordinary-%d", i), body, nil)
					send(path, request, http.StatusOK)
				}
			} else {
				if writer == "evidence" {
					current = f.Publish(t, "nonvisual-check", current.ID)
				}
				var nonvisual []tracker.ChangeArtifact
				for _, kind := range []string{"code", "diff", "manifest", "test", "log", "checkpoint"} {
					a := artifact("https://example.test/" + kind)
					a.Kind = kind
					nonvisual = append(nonvisual, a)
				}
				a := artifact("attachment:" + text.ID)
				a.Kind, a.SHA256 = "log", text.SHA256
				nonvisual = append(nonvisual, a)
				path, request := build("nonvisual", "", nonvisual)
				send(path, request, http.StatusOK)
			}
			issuePath := f.Base + "/work-items/" + string(f.Issue.WorkItemID)
			response = f.Hub.Request(t, http.MethodPatch, issuePath, f.Token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "replace-issue-body"}, ExpectedRevision: f.Issue.Revision, Body: new("Replacement" + hubfixture.IssueContractSections)})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var retained int
			if err := f.Hub.Config.TenantRepository.ReadTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				for _, id := range []string{image.ID, pdf.ID} {
					references, err := r.AttachmentReferences(t.Context(), f.Project.ID, id, corestore.MaxPageSize)
					if err != nil {
						return err
					}
					for _, reference := range references {
						if reference.WorkItemID == f.Issue.WorkItemID && reference.RetainWithWorkItem {
							retained++
						}
					}
				}
				return nil
			}); err != nil || retained != 2 {
				t.Fatalf("issue edit removed immutable references: count=%d error=%v", retained, err)
			}
		})
	}
}
