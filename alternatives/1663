package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssueParticipantEmails(t *testing.T) {
	for _, test := range []struct {
		name               string
		author, eventActor string
		kind               string
		data               tracker.CollaborationData
		commenter          bool
		commenterKind      string
		want               int
		verify             bool
	}{
		{name: "Reporter filer promoted", author: "reporter", eventActor: "member", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Triage", ToState: "Todo"}, want: 1},
		{name: "member comment", author: "reporter", eventActor: "member", kind: "comment.created", data: tracker.CollaborationData{CommentID: "cmt_notice"}, want: 1},
		{name: "Reporter commenter and member filer notified by worker", author: "member", eventActor: "runner", commenter: true, kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, want: 2},
		{name: "Reporter filer asked to verify", author: "reporter", eventActor: "runner", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done", Verification: tracker.VerificationPending}, want: 1, verify: true},
		{name: "only the filer is asked to verify", author: "member", eventActor: "runner", commenter: true, kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done", Verification: tracker.VerificationPending}, want: 2, verify: true},
		{name: "runner commenter excluded", author: "reporter", eventActor: "member", commenter: true, commenterKind: "runner", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, want: 1},
		{name: "service commenter excluded", author: "reporter", eventActor: "member", commenter: true, commenterKind: "integration", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, want: 1},
		{name: "own action", author: "reporter", eventActor: "reporter", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Triage", ToState: "Cancelled"}},
		{name: "related addition", author: "reporter", eventActor: "member", kind: "issue.edited", data: tracker.CollaborationData{Operation: "add", Fields: []string{"related"}}, want: 2},
		{name: "related removal", author: "reporter", eventActor: "member", kind: "issue.edited", data: tracker.CollaborationData{Operation: "remove", Fields: []string{"related"}}},
		{name: "body edit", author: "reporter", eventActor: "member", kind: "issue.edited", data: tracker.CollaborationData{Fields: []string{"body"}}},
		{name: "comment edit", author: "reporter", eventActor: "member", kind: "comment.edited", data: tracker.CollaborationData{CommentID: "cmt_notice"}},
		{name: "same lane", author: "reporter", eventActor: "member", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Todo", ToState: "Todo"}},
		{name: "runner filer excluded", author: "runner", eventActor: "member", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}},
		{name: "service filer excluded", author: "integration", eventActor: "member", kind: "workflow.transitioned", data: tracker.CollaborationData{FromState: "Cancelled", ToState: "Todo"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			reporter := f.user(t, "reporter", "reporter", "reporter@example.test", "write", "")
			member := f.user(t, "member", "member", "member@example.test", "write", "")
			f.grant(t, reporter, true, false)
			f.grant(t, member, true, false)
			users := map[string]hostedSecurityUser{"reporter": reporter, "member": member}
			request := func(user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return f.serve(t, hostedSharedRequest{user: &user, method: method, target: path, body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
			}
			create := func(key string) tracker.NativeIssue {
				var issue tracker.NativeIssue
				r := request(member, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Notification example " + key, Body: "private-issue-body-sentinel" + issueContractTestSections})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &issue)
				return issue
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET duplicate_challenge=0 WHERE id=?", f.project)
			issue, other := create("first"), create("second")
			principal := func(user hostedSecurityUser) string {
				var id string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", user.identity.Subject).Scan(&id); err != nil {
					t.Fatal(err)
				}
				return id
			}
			actor := tracker.Actor{Kind: "human", PrincipalID: principal(member)}
			if user, found := users[test.author]; found {
				actor.PrincipalID = principal(user)
			} else {
				actor.Kind, actor.PrincipalID = test.author, test.author
			}
			raw, _ := json.Marshal(actor)
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE issues SET actor_json=? WHERE native_id IN (?,?)", string(raw), issue.WorkItemID, other.WorkItemID)
			if test.verify {
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE issues SET verification_state='pending' WHERE native_id=?", issue.WorkItemID)
			}
			now := time.Now()
			appendEvent := func(item tracker.NativeIssue, eventActor tracker.Actor, kind string, data tracker.CollaborationData, at time.Time) {
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				scope := nativeScope{organization: "org_security", project: f.project, sourceActor: &eventActor}
				if err := appendNativeHistory(t.Context(), tx, scope, string(item.WorkItemID), kind, data, at); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if test.commenter {
				commentActor := tracker.Actor{Kind: "human", PrincipalID: principal(reporter)}
				if test.commenterKind != "" {
					commentActor.Kind, commentActor.PrincipalID = test.commenterKind, principal(member)
				}
				appendEvent(issue, commentActor, "comment.created", tracker.CollaborationData{CommentID: "cmt_reporter"}, now)
			}
			if test.commenterKind != "" {
				if err := f.service.collectIssueEmails(t.Context()); err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM issue_notification_batches")
			}
			eventActor := tracker.Actor{Kind: "human"}
			if user, found := users[test.eventActor]; found {
				eventActor.PrincipalID = principal(user)
			} else {
				eventActor.Kind, eventActor.PrincipalID = "runner", "runner_fixture"
			}
			data := test.data
			if len(data.Fields) > 0 && data.Fields[0] == "related" {
				data.RelatedWorkItemID = other.WorkItemID
			}
			data.ReasonDetail = "private-comment-text-sentinel"
			appendEvent(issue, eventActor, test.kind, data, now.Add(time.Second))
			messages := []auth.EmailMessage{}
			f.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
				messages = append(messages, message)
				return nil
			})
			if err := f.service.deliverIssueEmails(t.Context(), now.Add(30*time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 0 {
				t.Fatal("email sent before debounce")
			}
			if err := f.service.deliverIssueEmails(t.Context(), now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(messages) != test.want {
				t.Fatalf("got %d emails, want %d: %+v", len(messages), test.want, messages)
			}
			for _, message := range messages {
				if !strings.Contains(message.Body, "https://hub.example.test/local-organization/private-project-sentinel/issues/") || !strings.HasPrefix(message.Subject, "[Detent] ") || !strings.Contains(message.Subject, " #") || !strings.Contains(message.Body, "Unsubscribe from this issue:") {
					t.Fatalf("incomplete message: %+v", message)
				}
				if strings.Contains(message.Body, "private-issue-body-sentinel") || strings.Contains(message.Body, "private-comment-text-sentinel") {
					t.Fatal("email contains issue or comment text")
				}
				if test.kind == "comment.created" && (!strings.Contains(message.Body, member.identity.Email) || !strings.Contains(message.Body, "#issue-comment-cmt_notice")) {
					t.Fatalf("missing commenter/link: %s", message.Body)
				}
				if test.kind == "workflow.transitioned" && !strings.Contains(message.Body, test.data.FromState+" to "+test.data.ToState) {
					t.Fatalf("missing states: %s", message.Body)
				}
				asked := strings.HasPrefix(message.Subject, "[Detent] Please verify #") && strings.Contains(message.Body, "Please verify #")
				filer := users[test.author].identity.Email == message.To
				if asked != (test.verify && filer) {
					t.Fatalf("verification prompt=%t for filer=%t: %+v", asked, filer, message)
				}
			}
			if err := f.service.deliverIssueEmails(t.Context(), now.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if len(messages) != test.want {
				t.Fatal("history replay sent more emails")
			}
		})
	}
}

func TestIssueEmailBatchingAndOptOut(t *testing.T) {
	for _, test := range []string{"batch", "restart batch", "unsubscribe", "preference", "revoked access", "delivery failure"} {
		t.Run(test, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			reporter := f.user(t, "reporter", "reporter", "reporter@example.test", "write", "")
			member := f.user(t, "member", "member", "member@example.test", "write", "")
			f.grant(t, reporter, true, false)
			f.grant(t, member, true, false)
			request := func(user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return f.serve(t, hostedSharedRequest{user: &user, method: method, target: path, body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
			}
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			r := request(owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"name": "Notification project", "grant_access": true, "idempotency_key": "notification-project"})
			requireNativeStatus(t, r, http.StatusCreated)
			var project tracker.NativeProject
			decodeHubResponse(t, r, &project)
			f.project = project.ID
			f.base = "/api/v2/organizations/org_security/projects/" + string(project.ID)
			seedNativeFixturePolicy(t, f.service, "org_security/"+string(project.ID))
			f.grant(t, reporter, true, false)
			f.grant(t, member, true, false)
			var issue tracker.NativeIssue
			r = request(reporter, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "file"}, Type: "bug", Title: "Reporter notification example", Body: reporterBugSections + "\nprivate-issue-body-sentinel" + issueContractTestSections})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &issue)
			if issue.State != "Triage" {
				t.Fatal("Reporter issue not in Triage")
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			now := time.Now()
			r = request(member, http.MethodPost, path+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "promote"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &issue)
			for index := range 9 {
				r = request(member, http.MethodPost, path+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", index)}, Body: "private-comment-text-sentinel"})
				requireNativeStatus(t, r, http.StatusOK)
			}
			messages := []auth.EmailMessage{}
			sender := healthEmailSenderFunc(func(ctx context.Context, message auth.EmailMessage) error {
				var state string
				if err := f.service.database.db.QueryRowContext(ctx, "SELECT w.detent_state FROM issues i JOIN workflow_states w ON w.id=i.workflow_state_id WHERE i.native_id=?", issue.WorkItemID).Scan(&state); err != nil {
					return err
				}
				if state != "Todo" {
					t.Errorf("mail observed uncommitted lane: %s", state)
				}
				messages = append(messages, message)
				if test == "delivery failure" {
					return errors.New("fixture mail failure")
				}
				return nil
			})
			f.service.config.Hosted.EmailSender = sender
			if err := f.service.deliverIssueEmails(t.Context(), now.Add(30*time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(messages) != 0 {
				t.Fatal("debounce sent early")
			}
			prefPath := "/api/v2/organizations/org_security/issue-notification-preference"
			r = request(reporter, http.MethodGet, prefPath, nil)
			requireNativeStatus(t, r, http.StatusOK)
			var preference issueNotificationPreference
			decodeHubResponse(t, r, &preference)
			if !preference.Enabled {
				t.Fatal("preference not on by default")
			}
			switch test {
			case "restart batch":
				f.service.config.Hosted.EmailSender = nil
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(t, cfg)
				f.service.config.Hosted.EmailSender = sender
			case "unsubscribe":
				var token string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT token FROM issue_subscriptions WHERE work_item_id=? AND user_id=?", issue.WorkItemID, reporter.identity.Subject).Scan(&token); err != nil {
					t.Fatal(err)
				}
				r = f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/issue-notifications/unsubscribe", body: `{"token":"` + token + `"}`})
				requireNativeStatus(t, r, http.StatusNoContent)
			case "preference":
				r = request(reporter, http.MethodPut, prefPath, map[string]bool{"enabled": false})
				requireNativeStatus(t, r, http.StatusOK)
			case "revoked access":
				operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", reporter.identity.Subject, f.project)
			}
			err := f.service.deliverIssueEmails(t.Context(), now.Add(2*time.Minute))
			if (err != nil) != (test == "delivery failure") {
				t.Fatalf("delivery result %v", err)
			}
			want := 0
			if test == "batch" || test == "restart batch" || test == "delivery failure" {
				want = 1
			}
			if len(messages) != want {
				t.Fatalf("got %d messages, want %d", len(messages), want)
			}
			if want == 1 {
				message := messages[0]
				if strings.Count(message.Body, " commented: ") != 9 || !strings.Contains(message.Body, "Triage to Todo") || strings.Contains(message.Body, "private-issue-body-sentinel") || strings.Contains(message.Body, "private-comment-text-sentinel") {
					t.Fatalf("incorrect batch: %s", message.Body)
				}
				if message.To != reporter.identity.Email {
					t.Fatalf("recipient %s", message.To)
				}
				unsubscribe := strings.TrimSpace(strings.Split(message.Body, "Unsubscribe from this issue: ")[1])
				if _, err := url.ParseRequestURI(unsubscribe); err != nil {
					t.Fatal(err)
				}
			}
			r = request(member, http.MethodGet, path, nil)
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &issue)
			if issue.State != "Todo" {
				t.Fatal("mail failure rolled back lane")
			}
			r = request(member, http.MethodPost, path+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "later-comment"}, Body: "Later private observation"})
			requireNativeStatus(t, r, http.StatusOK)
			err = f.service.deliverIssueEmails(t.Context(), now.Add(3*time.Minute))
			if (err != nil) != (test == "delivery failure") {
				t.Fatalf("later delivery %v", err)
			}
			if test == "unsubscribe" || test == "preference" || test == "revoked access" {
				if len(messages) != 0 {
					t.Fatal("opted-out participant received later email")
				}
			}
		})
	}
}
