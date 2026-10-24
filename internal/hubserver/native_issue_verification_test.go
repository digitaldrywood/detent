package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type verificationFixture struct {
	hostedSharedFixture
	users map[string]hostedSecurityUser
}

func newVerificationFixture(t *testing.T) verificationFixture {
	t.Helper()
	f := newHostedSharedFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	users := map[string]hostedSecurityUser{
		"reporter": f.user(t, "reporter", "reporter", "reporter@example.test", "write", ""),
		"tester":   f.user(t, "tester", "reporter", "tester@example.test", "write", ""),
		"member":   f.user(t, "member", "member", "member@example.test", "write", ""),
	}
	v := verificationFixture{hostedSharedFixture: f, users: users}
	r := v.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"name": "Verification project", "grant_access": true, "idempotency_key": "verification-project"})
	requireNativeStatus(t, r, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, r, &project)
	v.project = project.ID
	v.base = "/api/v2/organizations/org_security/projects/" + string(project.ID)
	seedNativeFixturePolicy(t, v.service, "org_security/"+string(project.ID))
	for _, user := range users {
		v.grant(t, user, true, false)
	}
	operatorSQL(t, v.hostedSecurityFixture, "UPDATE projects SET duplicate_challenge=0 WHERE id=?", v.project)
	return v
}

func (v verificationFixture) request(t *testing.T, user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := hostedSharedRequest{user: &user, method: method, target: path, csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r.body = string(raw)
	}
	return v.serve(t, r)
}

func (v verificationFixture) issue(t *testing.T, item tracker.NativeWorkItemID) tracker.NativeIssue {
	t.Helper()
	var issue tracker.NativeIssue
	r := v.request(t, v.users["member"], http.MethodGet, v.base+"/work-items/"+string(item), nil)
	requireNativeStatus(t, r, http.StatusOK)
	decodeHubResponse(t, r, &issue)
	return issue
}

func (v verificationFixture) move(t *testing.T, issue tracker.NativeIssue, state string) tracker.NativeIssue {
	t.Helper()
	r := v.request(t, v.users["member"], http.MethodPost, v.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + string(issue.WorkItemID) + "-" + state + "-" + strconv.FormatInt(int64(issue.Revision), 10)}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"})
	requireNativeStatus(t, r, http.StatusOK)
	decodeHubResponse(t, r, &issue)
	return issue
}

func (v verificationFixture) file(t *testing.T, filer, key string, actor *tracker.Actor, body string, prepare ...func(tracker.NativeIssue)) tracker.NativeIssue {
	t.Helper()
	var issue tracker.NativeIssue
	r := v.request(t, v.users[filer], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "file-" + key}, Type: "bug", Title: "Verification example " + key, Body: reporterBugSections + issueContractTestSections})
	requireNativeStatus(t, r, http.StatusOK)
	decodeHubResponse(t, r, &issue)
	if actor != nil {
		raw, err := json.Marshal(actor)
		if err != nil {
			t.Fatal(err)
		}
		operatorSQL(t, v.hostedSecurityFixture, "UPDATE issues SET actor_json=? WHERE native_id=?", string(raw), issue.WorkItemID)
	}
	if body != "" {
		operatorSQL(t, v.hostedSecurityFixture, "UPDATE issues SET body=? WHERE native_id=?", body, issue.WorkItemID)
	}
	for _, apply := range prepare {
		apply(issue)
	}
	issue = v.issue(t, issue.WorkItemID)
	if issue.State == "Triage" {
		issue = v.move(t, issue, "Todo")
	}
	return v.move(t, issue, "Done")
}

func (v verificationFixture) verify(t *testing.T, answerer string, issue tracker.NativeIssue, key, result, comment string) *httptest.ResponseRecorder {
	t.Helper()
	return v.request(t, v.users[answerer], http.MethodPost, v.base+"/work-items/"+string(issue.WorkItemID)+"/verification", tracker.VerifyFix{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, Result: result, Comment: comment})
}

func TestNativeIssueVerificationEntersPendingByFiler(t *testing.T) {
	v := newVerificationFixture(t)
	stamped := issueorigin.Stamp(reporterBugSections, issueorigin.Origin{Kind: "worker", Fingerprint: "verification-example"})
	for _, test := range []struct {
		name, filer string
		actor       *tracker.Actor
		body        string
		workspace   bool
		pending     bool
	}{
		{name: "Reporter", filer: "reporter", pending: true},
		{name: "member", filer: "member", pending: true},
		{name: "worker", filer: "member", actor: &tracker.Actor{Kind: "runner", PrincipalID: "runner_example"}},
		{name: "service key", filer: "member", actor: &tracker.Actor{Kind: "integration", PrincipalID: "service_example"}},
		{name: "machine origin", filer: "member", body: stamped},
		{name: "workspace item", filer: "member", workspace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var prepare []func(tracker.NativeIssue)
			if test.workspace {
				prepare = append(prepare, func(issue tracker.NativeIssue) {
					stamp := formatHubTime(time.Now())
					workspace := "workspace_verification_example"
					operatorSQL(t, v.hostedSecurityFixture, `INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,ref,state,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) VALUES(?,'org_security',?,?,'develop','ready',300,?,?,?,?,?)`, workspace, v.project, issue.WorkItemID, stamp, stamp, v.users["member"].identity.Subject, stamp, stamp)
					operatorSQL(t, v.hostedSecurityFixture, `INSERT INTO workspace_items(work_item_id,workspace_id,organization_id,project_id,created_at) VALUES(?,?,'org_security',?,?)`, issue.WorkItemID, workspace, v.project, stamp)
				})
			}
			issue := v.file(t, test.filer, "pending-"+test.name, test.actor, test.body, prepare...)
			if issue.State != "Done" || !issue.Terminal {
				t.Fatalf("state=%s terminal=%t", issue.State, issue.Terminal)
			}
			if got := issue.Verification != nil && issue.Verification.State == tracker.VerificationPending; got != test.pending {
				t.Fatalf("verification=%+v, want pending=%t", issue.Verification, test.pending)
			}
			if !test.pending {
				requireNativeStatus(t, v.verify(t, "member", issue, "machine-"+test.name, tracker.VerifyResultVerified, ""), http.StatusUnprocessableEntity)
			}
		})
	}
}

func TestNativeIssueVerificationAnswers(t *testing.T) {
	v := newVerificationFixture(t)
	for _, test := range []struct {
		name, filer, answerer string
		result, comment       string
		status                int
		state                 string
		verified              bool
		archived              bool
	}{
		{name: "filer verifies", filer: "reporter", answerer: "reporter", result: "verified", status: http.StatusOK, state: "Done", verified: true},
		{name: "member verifies", filer: "reporter", answerer: "member", result: "verified", status: http.StatusOK, state: "Done", verified: true},
		{name: "other Reporter verifies", filer: "reporter", answerer: "tester", result: "verified", status: http.StatusNotFound, state: "Done"},
		{name: "Reporter on member issue", filer: "member", answerer: "reporter", result: "verified", status: http.StatusNotFound, state: "Done"},
		{name: "not fixed without comment", filer: "reporter", answerer: "reporter", result: "not_fixed", status: http.StatusUnprocessableEntity, state: "Done"},
		{name: "not fixed blank comment", filer: "reporter", answerer: "reporter", result: "not_fixed", comment: "   ", status: http.StatusUnprocessableEntity, state: "Done"},
		{name: "unknown result", filer: "reporter", answerer: "reporter", result: "maybe", status: http.StatusUnprocessableEntity, state: "Done"},
		{name: "filer not fixed", filer: "reporter", answerer: "reporter", result: "not_fixed", comment: "Saving still loses the change.", status: http.StatusOK, state: "Todo"},
		{name: "member not fixed", filer: "reporter", answerer: "member", result: "not_fixed", comment: "Still reproduces on staging.", status: http.StatusOK, state: "Todo"},
		{name: "archived not fixed", filer: "reporter", answerer: "reporter", result: "not_fixed", comment: "Still broken after auto-archive.", status: http.StatusOK, state: "Todo", archived: true},
		{name: "other Reporter not fixed", filer: "reporter", answerer: "tester", result: "not_fixed", comment: "Not mine to answer.", status: http.StatusNotFound, state: "Done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := v.file(t, test.filer, "answer-"+test.name, nil, "")
			if test.archived {
				r := v.request(t, v.users["member"], http.MethodPost, v.base+"/work-items/"+string(issue.WorkItemID)+"/archive", map[string]any{"idempotency_key": "archive-" + test.name, "expected_revision": strconv.FormatInt(int64(issue.Revision), 10)})
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &issue)
				if !issue.Archived || issue.Verification == nil || issue.Verification.State != tracker.VerificationPending {
					t.Fatalf("archived=%t verification=%+v", issue.Archived, issue.Verification)
				}
			}
			requireNativeStatus(t, v.verify(t, test.answerer, issue, "verify-"+test.name, test.result, test.comment), test.status)
			after := v.issue(t, issue.WorkItemID)
			if after.State != test.state {
				t.Fatalf("state=%s, want %s", after.State, test.state)
			}
			switch {
			case test.verified:
				answerer := v.issue(t, issue.WorkItemID)
				if answerer.Verification == nil || answerer.Verification.State != tracker.VerificationVerified || answerer.Verification.VerifiedAt == nil || answerer.Verification.VerifiedBy == nil || answerer.Verification.VerifiedBy.Kind != "human" {
					t.Fatalf("verification=%+v", answerer.Verification)
				}
				var principal string
				if err := v.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", v.users[test.answerer].identity.Subject).Scan(&principal); err != nil {
					t.Fatal(err)
				}
				if answerer.Verification.VerifiedBy.PrincipalID != principal {
					t.Fatalf("verified by %s, want %s", answerer.Verification.VerifiedBy.PrincipalID, principal)
				}
			case test.state == "Todo":
				if after.Verification != nil || after.Terminal || after.Archived {
					t.Fatalf("reopened verification=%+v terminal=%t archived=%t", after.Verification, after.Terminal, after.Archived)
				}
				var comments tracker.Page[tracker.NativeComment]
				decodeHubResponse(t, v.request(t, v.users["member"], http.MethodGet, v.base+"/work-items/"+string(issue.WorkItemID)+"/comments", nil), &comments)
				if len(comments.Items) != 1 || comments.Items[0].Body != test.comment {
					t.Fatalf("comments=%+v", comments.Items)
				}
				requireNativeStatus(t, v.verify(t, test.answerer, issue, "verify-"+test.name, test.result, test.comment), http.StatusOK)
				requireNativeStatus(t, v.verify(t, test.answerer, after, "again-"+test.name, tracker.VerifyResultVerified, ""), http.StatusUnprocessableEntity)
				var reopens int
				if err := v.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='workflow.transitioned' AND json_extract(data_json,'$.from_state')='Done'", issue.WorkItemID).Scan(&reopens); err != nil {
					t.Fatal(err)
				}
				if reopens != 1 {
					t.Fatalf("reopen events=%d, want 1", reopens)
				}
				w := operatortool.AnalyticsWindow{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Bucket: 2 * time.Hour}
				landing := nativeAnalyticsLanding{ChangeID: "change_example", WorkItemID: issue.WorkItemID, Landing: tracker.ChangeLanding{VersionID: "version_example", LandedAt: time.Now().Add(-30 * time.Minute)}}
				quality, err := readAnalyticsQualityEvents(t, v, issue.WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				summary := summarizeAnalyticsQuality([]nativeAnalyticsLanding{landing}, quality, nil, w)
				if len(summary.Occurrences.Items) != 1 || summary.Occurrences.Items[0].Kind != "reopened" {
					t.Fatalf("escapes=%+v", summary.Occurrences.Items)
				}
			default:
				if after.Verification == nil || after.Verification.State != tracker.VerificationPending {
					t.Fatalf("refused answer changed verification: %+v", after.Verification)
				}
			}
		})
	}
}

func readAnalyticsQualityEvents(t *testing.T, v verificationFixture, item tracker.NativeWorkItemID) ([]qualityHistoryEvent, error) {
	t.Helper()
	rows, err := v.service.database.db.QueryContext(t.Context(), "SELECT id,type,recorded_at,data_json FROM collaboration_events WHERE work_item_id=? AND type='workflow.transitioned' ORDER BY sequence", item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []qualityHistoryEvent{}
	for rows.Next() {
		event := qualityHistoryEvent{Item: string(item)}
		var at, raw string
		if err := rows.Scan(&event.ID, &event.Kind, &at, &raw); err != nil {
			return nil, err
		}
		if event.At, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &event.Data); err != nil {
			return nil, err
		}
		event.ToTerminal = event.Data.ToState == "Done"
		events = append(events, event)
	}
	return events, rows.Err()
}

func TestNativeIssueVerificationListAndEmail(t *testing.T) {
	v := newVerificationFixture(t)
	messages := []auth.EmailMessage{}
	v.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(_ context.Context, message auth.EmailMessage) error {
		messages = append(messages, message)
		return nil
	})
	started := time.Now()
	pending := v.file(t, "reporter", "list-pending", nil, "")
	verified := v.file(t, "reporter", "list-verified", nil, "")
	requireNativeStatus(t, v.verify(t, "reporter", verified, "list-verify", tracker.VerifyResultVerified, ""), http.StatusOK)
	other := v.file(t, "tester", "list-other", nil, "")
	open := v.file(t, "reporter", "list-open", nil, "")
	open = v.move(t, open, "Todo")
	if err := v.service.deliverIssueEmails(t.Context(), started.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		user  string
		issue tracker.NativeIssue
		want  int
	}{
		{"reporter", pending, 1},
		{"reporter", verified, 0},
		{"tester", other, 1},
		{"reporter", open, 0},
		{"member", pending, 0},
	} {
		got := 0
		for _, message := range messages {
			if strings.Contains(message.HTMLBody, "Your issue is Done. Please verify it.") != strings.Contains(message.Subject, "Please verify #") {
				t.Fatal("HTML verification request does not match current verification state")
			}
			if message.To == v.users[test.user].identity.Email && strings.Contains(message.Subject, "Please verify #"+strconv.Itoa(test.issue.Number)+":") {
				got++
				if !strings.Contains(message.Body, "Please verify #"+strconv.Itoa(test.issue.Number)) || !strings.Contains(message.Body, "Todo to Done") {
					t.Fatalf("verification email body=%s", message.Body)
				}
			}
		}
		if got != test.want {
			t.Fatalf("%s verification emails for #%d=%d, want %d: %+v", test.user, test.issue.Number, got, test.want, messages)
		}
	}
	drainTestNativeBoard(t, v.service)
	for _, path := range []string{"/work-items?created_by=me&verification=pending", "/work-items?include=board&created_by=me&verification=pending"} {
		t.Run(path, func(t *testing.T) {
			var page tracker.NativeIssuePage
			r := v.request(t, v.users["reporter"], http.MethodGet, v.base+path, nil)
			requireNativeStatus(t, r, http.StatusOK)
			decodeHubResponse(t, r, &page)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != pending.WorkItemID {
				t.Fatalf("to-verify list=%+v", page.Items)
			}
		})
	}
	requireNativeStatus(t, v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items?verification=unknown", nil), http.StatusUnprocessableEntity)
}

func TestVerifyFixMCP(t *testing.T) {
	f := newNativeFixture(t, nil, "", "verify-fix-mcp")
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/verify-fix-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "verify-fix", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/verify-fix-test", f.token, map[string]any{}), http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{f.service}
	call := func(name string, arguments map[string]any) (operatortool.Result, error) {
		t.Helper()
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		return executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
	}
	done := func(name string) tracker.NativeIssue {
		t.Helper()
		issue := f.create(t, name)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done-" + name}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		return issue
	}
	first, second := done("first"), done("second")
	list := func() []operatortool.NativeItem {
		t.Helper()
		result, err := call(operatortool.WorkList, map[string]any{"project_id": f.project.ID, "created_by": "me", "verification": "pending"})
		if err != nil {
			t.Fatal(err)
		}
		var page operatortool.WorkReadResult[operatortool.NativeWorkPage]
		if err := json.Unmarshal(result.Content, &page); err != nil {
			t.Fatal(err)
		}
		return page.Data.Items
	}
	if items := list(); len(items) != 2 || items[0].Verification == nil || items[0].Verification.State != tracker.VerificationPending {
		t.Fatalf("pending list=%+v", items)
	}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		state     string
		invalid   bool
	}{
		{name: "not fixed needs a comment", arguments: map[string]any{"identifier": string(second.WorkItemID), "result": "not_fixed"}, invalid: true},
		{name: "verified", arguments: map[string]any{"identifier": string(first.WorkItemID), "result": "verified", "expected_revision": strconv.FormatInt(int64(first.Revision), 10)}, state: "Done"},
		{name: "not fixed", arguments: map[string]any{"identifier": string(second.WorkItemID), "result": "not_fixed", "comment": "Still reproduces."}, state: "Todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.arguments["project_id"], test.arguments["request_id"] = f.project.ID, test.name
			result, err := call(operatortool.VerifyFix, test.arguments)
			if test.invalid {
				var refusal *operatortool.RequestError
				if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.Message, "comment:") {
					t.Fatalf("missing comment error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var answered operatortool.WorkReadResult[tracker.NativeIssue]
			if err := json.Unmarshal(result.Content, &answered); err != nil {
				t.Fatal(err)
			}
			if answered.Data.State != test.state {
				t.Fatalf("answer=%s", result.Content)
			}
		})
	}
	if items := list(); len(items) != 0 {
		t.Fatalf("answered issues still pending: %+v", items)
	}
}
