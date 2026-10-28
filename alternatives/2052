package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudVisualReferenceWrites(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	csrf := cloudassert.CSRFToken("shared-"+owner.identity.Subject, "org_security")
	send := func(t *testing.T, method, suffix, key string, input any, want int) []byte {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		response := f.serve(t, hostedSharedRequest{user: &owner, method: method, target: f.base + suffix, body: string(raw), csrf: csrf})
		if response.Code != want {
			t.Fatalf("%s %s (%s): %d %s", method, suffix, key, response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}
	const source = "https://figma.com/design/example?secret=hidden"
	now := time.Now().UTC()
	provenance := &tracker.Provenance{Provider: "github", ExternalID: "historical", AuthorID: "example-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
	legacy := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "legacy"}, Title: "Legacy", Body: source + issueContractTestSections, State: "Todo"}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(send(t, http.MethodPost, "/work-items", "legacy", legacy, 200), &issue); err != nil {
		t.Fatal(err)
	}
	item := "/work-items/" + string(issue.WorkItemID)
	var comment tracker.NativeComment
	createComment := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "legacy-comment"}, Body: source}
	if err := json.Unmarshal(send(t, http.MethodPost, item+"/comments", "legacy-comment", createComment, 200), &comment); err != nil {
		t.Fatal(err)
	}
	imported := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "imported"}, Title: "Historical", Body: source + issueContractTestSections, State: "Todo", Provenance: provenance}
	send(t, http.MethodPost, "/work-items", "provenance-before", imported, 200)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=1,duplicate_challenge=0 WHERE id=?", f.project); err != nil {
		t.Fatal(err)
	}
	snapshot := func(t *testing.T) string {
		t.Helper()
		var body, title string
		var revision, sequence, versions, history, refs, receipts, issues, comments int
		if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT title,body,revision,event_sequence,
   (SELECT count(*) FROM collaboration_versions), (SELECT count(*) FROM collaboration_events),
   (SELECT count(*) FROM attachment_references), (SELECT count(*) FROM native_commands),
   (SELECT count(*) FROM issues), (SELECT count(*) FROM native_comments)
   FROM issues WHERE native_id=?`, issue.WorkItemID).Scan(&title, &body, &revision, &sequence, &versions, &history, &refs, &receipts, &issues, &comments); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal([]any{title, body, revision, sequence, versions, history, refs, receipts, issues, comments})
		return string(raw)
	}
	for _, test := range []struct {
		name, method, path string
		input              any
	}{
		{"filing", http.MethodPost, "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "refuse-create"}, Title: "New visual", Body: source + issueContractTestSections, State: "Todo"}},
		{"new provenance is not an exemption", http.MethodPost, "/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "new-provenance"}, Title: "New provenance", Body: source + issueContractTestSections, State: "Todo", Provenance: &tracker.Provenance{Provider: "github", ExternalID: "new", AuthorID: "example-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}},
		{"title", http.MethodPatch, item, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "refuse-title"}, ExpectedRevision: issue.Revision, Title: new(source)}},
		{"replacement retains old source", http.MethodPatch, item, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "refuse-body"}, ExpectedRevision: issue.Revision, Body: new(source + " changed" + issueContractTestSections)}},
		{"insert comment", http.MethodPost, item + "/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "refuse-comment"}, Body: source}},
		{"edit comment", http.MethodPatch, item + "/comments/" + comment.ID, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "refuse-comment-edit"}, ExpectedRevision: comment.Revision, Body: source + " changed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := snapshot(t)
			raw := send(t, test.method, test.path, test.name, test.input, http.StatusUnprocessableEntity)
			if !strings.Contains(string(raw), attachment.VisualReferenceMessage) || strings.Contains(string(raw), "hidden") || strings.Contains(string(raw), "figma.com") {
				t.Fatalf("unsafe refusal=%s", raw)
			}
			if after := snapshot(t); after != before {
				t.Fatalf("refusal changed state: %s -> %s", before, after)
			}
		})
	}
	send(t, http.MethodPost, "/work-items", "accepted replay", legacy, 200)
	imported.IdempotencyKey = "provenance-replay"
	send(t, http.MethodPost, "/work-items", "provenance-after", imported, 200)

	send(t, http.MethodPost, item+"/comments", "accepted comment replay", createComment, 200)
	send(t, http.MethodPatch, item, "no-op", tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "noop"}, ExpectedRevision: issue.Revision, Body: &legacy.Body}, 200)
	send(t, http.MethodPatch, item+"/comments/"+comment.ID, "comment no-op", tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-noop"}, ExpectedRevision: comment.Revision, Body: comment.Body}, 200)
	send(t, http.MethodPatch, item, "metadata only", tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "metadata"}, ExpectedRevision: issue.Revision, Priority: tracker.SetPriority(new(2))}, 200)
	send(t, http.MethodPatch, item+"/comments/"+comment.ID, "delete", tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "delete"}, ExpectedRevision: comment.Revision, Deleted: true}, 200)
}
