package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type issueMailerRepository struct {
	tenantstore.Repository
	accountstore.SummaryDeliveryRepository
	accountstore.NotificationRepository
	reads         chan context.Context
	notifications chan context.Context
	summaryDue    bool
}

func (r *issueMailerRepository) DueSummaries(_ context.Context, org tracker.OrganizationID, now time.Time) ([]accountstore.SummaryRecipient, error) {
	if !r.summaryDue {
		return nil, nil
	}
	r.summaryDue = false
	return []accountstore.SummaryRecipient{{Organization: string(org), TimeZone: "UTC", Eligible: true, End: now}}, nil
}

func (*issueMailerRepository) SummaryStatus(context.Context, tracker.OrganizationID, string, time.Time) (string, error) {
	return "", nil
}

func (r *issueMailerRepository) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	r.reads <- ctx
	<-ctx.Done()
	return ctx.Err()
}

func (r *issueMailerRepository) NotificationEvents(ctx context.Context, _ tracker.OrganizationID) ([]eventstore.Event, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r.notifications <- ctx:
		return nil, nil
	}
}

func (*issueMailerRepository) NotificationSeek(context.Context, tracker.OrganizationID) (accountstore.NotificationCursor, error) {
	return accountstore.NotificationCursor{}, nil
}

func (*issueMailerRepository) DueNotification(context.Context, tracker.OrganizationID, time.Time, accountstore.NotificationCursor, []accountstore.NotificationCandidate) (accountstore.NotificationPage, error) {
	return accountstore.NotificationPage{}, nil
}

func TestIssueMailerTickDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stop    bool
		wantErr error
		elapsed time.Duration
	}{
		{name: "tick expires and notifications resume", wantErr: context.DeadlineExceeded, elapsed: 10 * time.Second},
		{name: "shutdown cancels the pending read", stop: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo := &issueMailerRepository{reads: make(chan context.Context, 1), notifications: make(chan context.Context, 1), summaryDue: true}
				s := &Service{config: Config{
					Hosted: &hostedplans.HostedConfig{OrganizationID: "org_example", EmailSender: healthEmailSenderFunc(func(context.Context, auth.EmailMessage) error {
						t.Error("mailer sent an email after a failed summary read")
						return nil
					})},
					AccessRepository: repo, TenantRepository: repo, Clock: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				}}
				s.startIssueMailer()
				defer s.stopIssueMailer()

				ctx := <-repo.reads
				started := time.Now()
				deadline, ok := ctx.Deadline()
				if !ok || deadline.Sub(started) != 10*time.Second {
					t.Fatalf("summary read deadline = %v, present = %t, want 10s from read start", deadline, ok)
				}
				if tc.stop {
					s.stopIssueMailer()
				}
				<-ctx.Done()
				if !errors.Is(ctx.Err(), tc.wantErr) || time.Since(started) != tc.elapsed {
					t.Fatalf("summary read ended after %s with %v, want %s and %v", time.Since(started), ctx.Err(), tc.elapsed, tc.wantErr)
				}
				if !tc.stop {
					select {
					case <-s.issueMailer.done:
						t.Fatal("tick expiry stopped the mail worker")
					case next := <-repo.notifications:
						if nextDeadline, ok := next.Deadline(); !ok || nextDeadline.Sub(deadline) != 10*time.Second {
							t.Fatalf("notification deadline = %v, present = %t, want 10s after summary tick expired", nextDeadline, ok)
						}
					}
				}
			})
		})
	}
}

type unreadableDetails struct {
	attachment.DetailStore
	unreadable bool
}

func (d *unreadableDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if d.unreadable {
		return nil, errors.New("detail read unavailable")
	}
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestIssueEmailKeepsNoticeWhenVerificationIsUnreadable(t *testing.T) {
	t.Parallel()
	v := newVerificationFixture(t)
	requests := 0
	issue := v.file(t, "reporter", "unreadable-verification", nil, "")
	v.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
		if message.To == v.users["reporter"].identity.Email && strings.Contains(message.Subject, "Please verify #"+strconv.Itoa(issue.Number)+":") {
			requests++
		}
		return nil
	})
	if err := v.service.collectIssueEmails(t.Context()); err != nil {
		t.Fatal(err)
	}
	details := &unreadableDetails{DetailStore: v.service.runDetailStore(), unreadable: true}
	v.service.config.RunDetails = details
	due := time.Now().Add(2 * time.Minute)
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err == nil || requests != 0 {
		t.Fatalf("unreadable verification error=%v requests=%d", err, requests)
	}
	details.unreadable = false
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err != nil || requests != 1 {
		t.Fatalf("readable verification error=%v requests=%d, want 1", err, requests)
	}
}

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
			s := &Service{config: Config{Hosted: &hostedplans.HostedConfig{PublicURL: "https://hub.example.test", OrganizationID: "org_example", SharedEntry: &hostedplans.HostedSharedEntry{}}}}
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
	t.Parallel()
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

func TestIssueEmailPassesSubscriptionHeldByAnotherWorker(t *testing.T) {
	t.Parallel()
	v := newVerificationFixture(t)
	requests := map[string]int{}
	v.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
		if strings.Contains(message.Subject, "Please verify #") {
			requests[message.To]++
		}
		return nil
	})
	v.file(t, "reporter", "held-first", nil, "")
	v.file(t, "tester", "held-second", nil, "")
	if err := v.service.collectIssueEmails(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, ok := projectRecordDatabases.Load(v.service.config.TenantRepository)
	if !ok {
		t.Fatal("verification fixture has no isolated PostgreSQL database")
	}
	held, err := stored.(*sql.DB).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	var first string
	if err := held.QueryRowContext(t.Context(), `SELECT m.email FROM issue_subscriptions s JOIN hosted_members m ON m.organization_id=s.organization_id AND m.user_id=s.user_id WHERE s.organization_id=$1 ORDER BY s.work_item_id,s.user_id LIMIT 1 FOR UPDATE OF s`, v.service.accessOrganization()).Scan(&first); err != nil {
		t.Fatal(err)
	}
	second := v.users["reporter"].identity.Email
	if second == first {
		second = v.users["tester"].identity.Email
	}
	due := time.Now().Add(2 * time.Minute)
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err != nil || requests[first] != 0 || requests[second] != 1 {
		t.Fatalf("with %s held: error=%v requests=%v, want only %s", first, err, requests, second)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err != nil || requests[first] != 1 || requests[second] != 1 {
		t.Fatalf("after release: error=%v requests=%v", err, requests)
	}
}

func TestIssueEmailReachesDueNoticeBehindFutureNotices(t *testing.T) {
	t.Parallel()
	v := newVerificationFixture(t)
	requests := map[string]int{}
	v.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
		if strings.Contains(message.Subject, "Please verify #") {
			requests[message.To]++
		}
		return nil
	})
	v.file(t, "reporter", "future-prefix", nil, "")
	v.file(t, "tester", "due-tail", nil, "")
	if err := v.service.collectIssueEmails(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, ok := projectRecordDatabases.Load(v.service.config.TenantRepository)
	if !ok {
		t.Fatal("verification fixture has no isolated PostgreSQL database")
	}
	admin, organization := stored.(*sql.DB), v.service.accessOrganization()
	var item, user, waiting string
	if err := admin.QueryRowContext(t.Context(), `SELECT s.work_item_id,s.user_id,m.email FROM issue_subscriptions s JOIN hosted_members m ON m.organization_id=s.organization_id AND m.user_id=s.user_id WHERE s.organization_id=$1 ORDER BY s.work_item_id,s.user_id LIMIT 1`, organization).Scan(&item, &user, &waiting); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(2 * time.Minute)
	if _, err := admin.ExecContext(t.Context(), `UPDATE issue_notification_notices SET due_at=$4 WHERE organization_id=$1 AND work_item_id=$2 AND user_id=$3`, organization, item, user, due.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO issue_notification_notices(organization_id,work_item_id,user_id,event_id,source_item,due_at,state_from,state_to,author,comment_id,related,verify) SELECT $1,$2,$3,'a_'||n,$2,$4,'','','','',false,false FROM generate_series(1,$5::integer) n`, organization, item, user, due.Add(time.Hour), 100*128+200); err != nil {
		t.Fatal(err)
	}
	reached := v.users["reporter"].identity.Email
	if reached == waiting {
		reached = v.users["tester"].identity.Email
	}
	for tick := range 3 {
		if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err != nil || requests[reached] != 1 || requests[waiting] != 0 {
			t.Fatalf("tick=%d error=%v requests=%v, want one for %s and none before its time for %s", tick, err, requests, reached, waiting)
		}
	}
}

func TestIssueEmailReachesRecipientBehindFailingSubscriptions(t *testing.T) {
	t.Parallel()
	v := newVerificationFixture(t)
	later := v.users["tester"].identity.Email
	delivered := map[string]int{}
	v.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
		delivered[message.To]++
		return nil
	})
	for index := range 100 {
		v.file(t, "reporter", "failing-"+strconv.Itoa(index), nil, "")
	}
	last := v.file(t, "tester", "later-recipient", nil, "")
	stored, ok := projectRecordDatabases.Load(v.service.config.TenantRepository)
	if !ok {
		t.Fatal("verification fixture has no isolated PostgreSQL database")
	}
	admin, organization := stored.(*sql.DB), v.service.accessOrganization()
	for subscriptions := 0; subscriptions != 101; {
		if err := v.service.collectIssueEmails(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRowContext(t.Context(), `SELECT count(DISTINCT (work_item_id,user_id)) FROM issue_notification_notices WHERE organization_id=$1`, organization).Scan(&subscriptions); err != nil {
			t.Fatal(err)
		}
	}
	due := time.Now().Add(2 * time.Minute)
	if _, err := admin.ExecContext(t.Context(), `UPDATE issue_notification_notices SET verify=false,due_at=$3 WHERE organization_id=$1 AND work_item_id=$2`, organization, last.WorkItemID, due); err != nil {
		t.Fatal(err)
	}
	v.service.config.RunDetails = &unreadableDetails{DetailStore: v.service.runDetailStore(), unreadable: true}
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err == nil || len(delivered) != 0 {
		t.Fatalf("first run error=%v delivered=%v, want 100 unreadable subscriptions and nothing sent", err, delivered)
	}
	if err := v.service.deliverIssueEmails(t.Context(), t.Context(), due); err == nil || delivered[later] != 1 || len(delivered) != 1 {
		t.Fatalf("second run error=%v delivered=%v, want the later recipient %s once", err, delivered, later)
	}
}
