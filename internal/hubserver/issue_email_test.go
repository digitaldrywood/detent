package hubserver

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRenderIssueEmail(t *testing.T) {
	t.Parallel()
	const link = "https://hub.example.test/example-org/example-project/issues/42"
	const unsubscribe = "https://hub.example.test/organizations/org_example/issue-notifications/unsubscribe?token=sub+%2B%26"
	const title = "Fix <script>alert(1)</script> & layout"
	const author = "Alex <editor> & Sam"
	for _, test := range []struct {
		name    string
		notices []issueEmailNotice
		text    string
		html    []string
		verify  bool
	}{
		{
			name: "state batch",
			notices: []issueEmailNotice{
				{StateFrom: "Todo", StateTo: "In Progress"},
				{StateFrom: "In Progress", StateTo: "Rework"},
			},
			text: "Todo to In Progress\nIn Progress to Rework\n",
			html: []string{"Todo</span>", "In Progress</span>", "Rework</span>", "&rarr;", "color:#71717b", "color:#2b7fff", "color:#fe9a00"},
		},
		{
			name: "comment batch",
			notices: []issueEmailNotice{
				{Author: author, CommentID: "cmt_first"},
				{Author: "Jordan", CommentID: "cmt second/#"},
			},
			text: author + " commented: " + link + "#issue-comment-cmt_first\nJordan commented: " + link + "#issue-comment-cmt%20second%2F%23\n",
			html: []string{"Alex &lt;editor&gt; &amp; Sam</strong> commented", "Jordan</strong> commented", `href="` + link + `#issue-comment-cmt_first"`, `href="` + link + `#issue-comment-cmt%20second%2F%23"`, "View comment"},
		},
		{
			name: "related batch", notices: []issueEmailNotice{{Related: true}},
			text: "A related issue link was added: " + link + "\n",
			html: []string{"A related issue link was added."},
		},
		{
			name: "verify batch",
			notices: []issueEmailNotice{
				{StateFrom: "Merging", StateTo: "Done", Verify: true},
				{Author: author, CommentID: "cmt_verify"},
			},
			text:   "Merging to Done\n" + author + " commented: " + link + "#issue-comment-cmt_verify\n",
			html:   []string{"Merging</span>", "Done</span>", "color:#346bf1", "color:#00bc7d", "Alex &lt;editor&gt; &amp; Sam</strong> commented", `href="` + link + `#issue-comment-cmt_verify"`, ">Verify</a>", "Still broken with a comment"},
			verify: true,
		},
		{
			name: "mixed batch",
			notices: []issueEmailNotice{
				{StateFrom: "Todo", StateTo: "In Progress"},
				{Author: author, CommentID: "cmt_mixed"},
				{Related: true},
			},
			text: "Todo to In Progress\n" + author + " commented: " + link + "#issue-comment-cmt_mixed\nA related issue link was added: " + link + "\n",
			html: []string{"Todo</span>", "In Progress</span>", "Alex &lt;editor&gt; &amp; Sam</strong> commented", `href="` + link + `#issue-comment-cmt_mixed"`, "A related issue link was added."},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &Service{config: Config{Hosted: &HostedConfig{PublicURL: "https://hub.example.test", OrganizationID: "org_example", SharedEntry: &HostedSharedEntry{}}}}
			message, err := s.renderIssueEmail("member@example.test", link, title, 42, "sub +&", test.notices)
			if err != nil {
				t.Fatal(err)
			}
			wantSubject := "[Detent] #42 " + title
			wantBody := "#42 " + title + "\n\n" + test.text + "\nOpen issue: " + link + "\nUnsubscribe from this issue: " + unsubscribe + "\n"
			if test.verify {
				wantSubject = "[Detent] Please verify #42: " + title
				wantBody = "Please verify #42. Your issue is Done. Open it and choose Verified, or Still broken with a comment: " + link + "\n\n" + wantBody
			}
			if message.To != "member@example.test" || message.Subject != wantSubject || message.Body != wantBody {
				t.Fatalf("text envelope changed: %+v", message)
			}
			for _, want := range append(test.html, `href="`+link+`"`, `href="https://hub.example.test/organizations/org_example/issue-notifications/unsubscribe?token=sub&#43;%2B%26"`, "Issue #42</a>", "Fix &lt;script&gt;alert(1)&lt;/script&gt; &amp; layout", ">Open issue</a>", "You get this because you filed or commented on this issue.", `src="cid:detent-mark"`) {
				if !strings.Contains(message.HTMLBody, want) {
					t.Errorf("HTML missing %q", want)
				}
			}
			if strings.Contains(message.HTMLBody, "<script>") || strings.Contains(message.HTMLBody, "<editor>") {
				t.Fatal("HTML contains unescaped title or author")
			}
			callout := strings.Index(message.HTMLBody, "Your issue is Done. Please verify it.")
			if test.verify {
				noticeList := strings.Index(message.HTMLBody, `aria-label="Issue updates"`)
				verifyButton := strings.Index(message.HTMLBody, ">Verify</a>")
				if callout < 0 || callout >= noticeList || verifyButton < 0 || verifyButton >= noticeList {
					t.Fatal("verify callout and button must precede the notices")
				}
			} else if callout >= 0 || strings.Contains(message.HTMLBody, ">Verify</a>") {
				t.Fatal("ordinary batch contains a verify request")
			}
			if len(message.InlineImages) != 1 || message.InlineImages[0].ContentID != "detent-mark" {
				t.Fatal("email is missing its inline mark")
			}
		})
	}
}

func TestIssueNoticeSelection(t *testing.T) {
	for _, test := range []struct {
		name, kind              string
		data                    tracker.CollaborationData
		notify, related, verify bool
	}{
		{name: "transition", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done", Verification: tracker.VerificationPending}, notify: true, verify: true},
		{name: "same lane", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Todo"}},
		{name: "comment", kind: "comment.created", data: tracker.CollaborationData{CommentID: "comment_example"}, notify: true},
		{name: "comment edit", kind: "comment.edited", data: tracker.CollaborationData{CommentID: "comment_example"}},
		{name: "related addition", kind: "issue.edited", data: tracker.CollaborationData{Operation: "add", Fields: []string{"related"}, RelatedWorkItemID: "item_related"}, notify: true, related: true},
		{name: "related removal", kind: "issue.edited", data: tracker.CollaborationData{Operation: "remove", Fields: []string{"related"}, RelatedWorkItemID: "item_related"}},
		{name: "body edit", kind: "issue.edited", data: tracker.CollaborationData{Fields: []string{"body"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			notice, notify := issueNotice(issueEmailEvent{kind: test.kind, data: test.data})
			if notify != test.notify || notice.Related != test.related || notice.Verify != test.verify {
				t.Fatalf("notice=%+v notify=%v", notice, notify)
			}
		})
	}
}
