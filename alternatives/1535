package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeVisualReferenceWrites(t *testing.T) {
	t.Parallel()
	for _, writer := range []string{"title", "body", "discussion", "review", "artifacts", "evidence"} {
		t.Run(writer, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
			otherIssue := other.create(t, "other-work")
			var uploader string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name='operator-changes'").Scan(&uploader); err != nil {
				t.Fatal(err)
			}
			upload := func(project tracker.NativeProject, mime string, deleted bool) attachment.Metadata {
				t.Helper()
				m := attachment.Metadata{ID: conversation.NewAttachmentID(), ProjectID: string(project.ID), Uploader: uploader, Name: "upload", ContentType: mime, Size: 100, SHA256: policy.Digest([]byte(mime))}
				if strings.HasPrefix(mime, "image/") {
					m.Width, m.Height = 1, 1
				}
				var tombstone any
				if deleted {
					tombstone = formatHubTime(time.Now())
				}
				_, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at,deleted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, project.OrganizationID, project.ID, m.Uploader, m.Name, m.ContentType, m.Size, m.SHA256, m.Width, m.Height, formatHubTime(time.Now()), tombstone)
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			image := upload(f.project, "image/png", false)
			pdf := upload(f.project, "application/pdf", false)
			text := upload(f.project, "text/plain", false)
			deleted := upload(f.project, "image/png", true)
			foreign := upload(other.project, "image/png", false)
			foreignBody := "https://figma.com/design/example " + foreign.Markdown(string(other.project.OrganizationID))
			response := performHubAPIRequest(t, f.service, http.MethodPost, other.base+"/work-items/"+string(otherIssue.WorkItemID)+"/changes", other.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "other-change"}, Title: "Other visual", Body: foreignBody})
			requireNativeStatus(t, response, http.StatusOK)
			var otherChange tracker.ChangeRequest
			decodeHubResponse(t, response, &otherChange)
			if otherChange.Body != foreignBody {
				t.Fatal("other project is not usable")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO attachment_references(attachment_id,work_item_id) VALUES(?,?)", image.ID, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}

			discussionPath := f.path + "/discussion"
			if writer == "discussion" {
				linked := f.create(t, "linked-work")
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.path[:strings.LastIndex(f.path, "/")], f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &f.change)
				f.path = f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes/" + f.change.ID
				discussionPath = f.base + "/work-items/" + string(linked.WorkItemID) + "/changes/" + f.change.ID + "/discussion"
			}

			current := f.publish(t, "initial", "")
			const visual = "https://figma.com/design/example"
			artifact := func(uri string) tracker.ChangeArtifact {
				return tracker.ChangeArtifact{Kind: "artifact", URI: uri, SHA256: image.SHA256, Availability: "available"}
			}
			canonical := func(m attachment.Metadata) string {
				return "/organizations/" + string(f.project.OrganizationID) + "/api/v2/projects/" + m.ProjectID + "/attachments/" + m.ID
			}
			build := func(key, body string, artifacts []tracker.ChangeArtifact) (string, any) {
				mutation := tracker.Mutation{IdempotencyKey: key}
				switch writer {
				case "title", "body":
					request := tracker.CreateChange{Mutation: mutation, Title: "Visual change", Body: body}
					if writer == "title" {
						request.Title, request.Body = body, "Change body"
					}
					return f.path[:strings.LastIndex(f.path, "/")], request
				case "discussion":
					return discussionPath, tracker.DiscussChange{Mutation: mutation, Body: body}
				case "review":
					return f.path + "/versions/" + current.ID + "/reviews", tracker.ReviewChange{Mutation: mutation, Decision: "commented", Body: body}
				case "artifacts":
					input := changeTestInput()
					input.Artifacts = artifacts
					return f.path + "/versions", tracker.PublishChangeVersion{Mutation: mutation, ExpectedVersionID: current.ID, ChangeVersionInput: input}
				default:
					request := changeTestResult(current)
					request.Mutation, request.Evidence = mutation, artifacts
					return f.path + "/versions/" + current.ID + "/checks", request
				}
			}
			send := func(path string, request any, want int) *httptest.ResponseRecorder {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request)
				requireNativeStatus(t, response, want)
				if writer == "artifacts" && want == http.StatusOK {
					decodeHubResponse(t, response, &current)
				}
				return response
			}
			legacyPath, legacy := build("legacy-visual", visual, []tracker.ChangeArtifact{artifact("https://example.test/screenshot.png")})
			send(legacyPath, legacy, http.StatusOK)
			provenance := &tracker.Provenance{Provider: "github", ExternalID: "example-comment", AuthorID: "example-author", CreatedAt: time.Now(), UpdatedAt: time.Now(), ObservedAt: time.Now()}
			if writer == "discussion" {
				request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "legacy-provenance"}, Body: visual, Provenance: provenance}
				send(f.path+"/discussion", request, http.StatusOK)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=1 WHERE id=?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			send(legacyPath, legacy, http.StatusOK)
			if writer == "discussion" {
				send(f.path+"/discussion", tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "provenance-replay"}, Body: visual, Provenance: provenance}, http.StatusOK)
				provenance.ExternalID = "new-example-comment"
			}
			if writer == "evidence" {
				current = f.publish(t, "new-check", current.ID)
			}
			snapshot := func() string {
				t.Helper()
				var change string
				var versions, evidence, refs, history, revisions, receipts, changes, retained int
				err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT record_json,
     (SELECT count(*) FROM change_versions), (SELECT count(*) FROM change_evidence),
     (SELECT count(*) FROM attachment_references), (SELECT count(*) FROM collaboration_events),
     (SELECT count(*) FROM collaboration_versions), (SELECT count(*) FROM native_commands),
     (SELECT count(*) FROM change_requests), (SELECT coalesce(sum(retain_with_work_item),0) FROM attachment_references) FROM change_requests WHERE id=?`, f.change.ID).Scan(&change, &versions, &evidence, &refs, &history, &revisions, &receipts, &changes, &retained)
				if err != nil {
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
				{name: "unrelated upload", body: visual + "\n\n" + image.Markdown(string(f.project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(image)), artifact("https://example.test/screen.png")}},
				{name: "wrong MIME", body: visual + " " + text.Markdown(string(f.project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(text))}},
				{name: "missing", body: visual + " ![upload](attachment:att_0123456789abcdef0123456789abcdef)", artifacts: []tracker.ChangeArtifact{artifact("attachment:att_0123456789abcdef0123456789abcdef")}},
				{name: "deleted", body: visual + " " + deleted.Markdown(string(f.project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(deleted))}},
				{name: "cross project", body: visual + " ![upload](attachment:" + foreign.ID + ")", artifacts: []tracker.ChangeArtifact{artifact("attachment:" + foreign.ID)}},
				{name: "cross project canonical", body: visual + " " + foreign.Markdown(string(f.project.OrganizationID)), artifacts: []tracker.ChangeArtifact{artifact(canonical(foreign))}},
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
				fresh := upload(f.project, "image/png", false)
				table := "change_evidence"
				if writer == "title" || writer == "body" {
					table = "change_requests"
				}
				if writer == "artifacts" {
					table = "change_versions"
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "CREATE TRIGGER refuse_visual_publication BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT,'fixture refusal'); END"); err != nil {
					t.Fatal(err)
				}
				before := snapshot()
				a := artifact("attachment:" + fresh.ID)
				path, request := build("rollback", visual+" "+fresh.Markdown(string(f.project.OrganizationID)), []tracker.ChangeArtifact{a})
				send(path, request, http.StatusInternalServerError)
				if after := snapshot(); before != after {
					t.Fatalf("failed publication changed records: %s -> %s", before, after)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER refuse_visual_publication"); err != nil {
					t.Fatal(err)
				}
			})

			for i, m := range []attachment.Metadata{image, pdf} {
				if writer == "evidence" && i > 0 {
					current = f.publish(t, fmt.Sprintf("check-version-%d", i), current.ID)
				}
				uri := "attachment:" + m.ID
				if i > 0 {
					uri = canonical(m)
				}
				body := visual + " " + m.Markdown(string(f.project.OrganizationID)) + ". [Source](https://github.com/example-org/example-project/pull/1)"
				accepted := artifact(uri)
				accepted.SHA256 = m.SHA256
				path, request := build(fmt.Sprintf("paired-%d", i), body, []tracker.ChangeArtifact{accepted})
				send(path, request, http.StatusOK)
				var bound bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM attachment_references WHERE attachment_id=? AND work_item_id=? AND comment_id='' AND retain_with_work_item=1)", m.ID, f.issue.WorkItemID).Scan(&bound); err != nil || !bound {
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
					current = f.publish(t, "nonvisual-check", current.ID)
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
			issuePath := f.base + "/work-items/" + string(f.issue.WorkItemID)
			response = performHubAPIRequest(t, f.service, http.MethodPatch, issuePath, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "replace-issue-body"}, ExpectedRevision: f.issue.Revision, Body: new("Replacement" + issueContractTestSections)})
			requireNativeStatus(t, response, http.StatusOK)
			var retained int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachment_references WHERE work_item_id=? AND retain_with_work_item=1 AND attachment_id IN (?,?)", f.issue.WorkItemID, image.ID, pdf.ID).Scan(&retained); err != nil || retained != 2 {
				t.Fatalf("issue edit removed immutable references: count=%d error=%v", retained, err)
			}
		})
	}
}
