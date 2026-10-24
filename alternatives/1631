package hubserver

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthEmailSenderFunc func(context.Context, auth.EmailMessage) error

func (f healthEmailSenderFunc) SendEmail(ctx context.Context, message auth.EmailMessage) error {
	return f(ctx, message)
}

func TestHealthFindingEmails(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, severity                         string
		unavailable, failure, legacy, rollback bool
	}{
		{name: "attention", severity: "attention"},
		{name: "watch", severity: "watch"},
		{name: "SMTP unavailable", severity: "attention", unavailable: true},
		{name: "watch without SMTP", severity: "watch", unavailable: true},
		{name: "SMTP failure does not repeat or skip other owners", severity: "attention", failure: true},
		{name: "existing finding does not send a historical open", severity: "attention", legacy: true},
		{name: "failed finding transaction sends nothing", severity: "attention", rollback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newBrowserHostedFixtureServing(t, true, "org_health_email", false)
			f := nativeFixture{service: h.service, project: tracker.NativeProject{OrganizationID: "org_health_email", ID: tracker.ProjectID(h.project)}}
			for _, member := range []struct{ user, role, status, organization string }{
				{"user_second_owner", "owner", "active", "org_browser_provider"},
				{"user_inactive_owner", "owner", "inactive", "org_browser_provider"},
				{"user_other_owner", "owner", "active", "org_other"},
				{"user_admin", "admin", "active", "org_browser_provider"},
			} {
				membership, err := h.provider.CreateMembership(t.Context(), member.user, member.organization, member.role)
				if err != nil {
					t.Fatal(err)
				}
				membership.Status = member.status
				h.provider.members[membership.ID] = membership
			}
			h.provider.users = map[string]auth.HostedUser{
				"user_browser_owner": {ID: "user_browser_owner", Email: "owner@example.test"},
				"user_second_owner":  {ID: "user_second_owner", Email: "second@example.test"},
			}
			messages := []auth.EmailMessage{}
			if !test.unavailable {
				h.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(ctx context.Context, message auth.EmailMessage) error {
					var count int
					if err := h.service.database.db.QueryRowContext(ctx, "SELECT count(*) FROM health_findings WHERE email_open_attempted=1 OR email_resolve_attempted=1").Scan(&count); err != nil {
						return err
					}
					if count == 0 {
						t.Error("email preceded committed finding")
					}
					messages = append(messages, message)
					if test.failure && message.To == "owner@example.test" {
						return errors.New("fixture SMTP failure")
					}
					return nil
				})
			}
			now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
			finding := newHealthFinding("test_signal", "instance", "project", h.project, "Finding summary.", "The owner checks the runner.", []string{h.project}, healthEvidence{Counts: map[string]int{"leases": 2}})
			finding.ConditionSince = now.Add(-48 * time.Hour)
			finding.Severity = test.severity
			if test.legacy || test.rollback {
				if test.rollback {
					if _, err := h.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_health_tick BEFORE INSERT ON health_detector_ticks BEGIN SELECT RAISE(ABORT, 'fixture health tick failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				tx, err := h.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if test.legacy {
					_, err = writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now.Add(-time.Minute), []healthFinding{finding})
					if err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				} else {
					err = h.service.commitHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, []healthFinding{finding})
					if err == nil {
						t.Error("finding transaction succeeded")
					}
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
					var count int
					if err := h.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings").Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 || len(messages) != 0 {
						t.Fatalf("rollback findings=%d emails=%d", count, len(messages))
					}
					if _, err := h.service.database.db.ExecContext(t.Context(), "DROP TRIGGER fail_health_tick"); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, step := range []struct {
				name   string
				offset time.Duration
				active bool
				emails int
			}{
				{"open", 0, true, 2},
				{"repeated evaluation", time.Minute, true, 2},
				{"resolve", 2 * time.Minute, false, 4},
				{"stays resolved", 3 * time.Minute, false, 4},
				{"reopen at window boundary", 62 * time.Minute, true, 4},
				{"resolve reused finding", 63 * time.Minute, false, 4},
				{"new occurrence outside window", 124 * time.Minute, true, 6},
			} {
				t.Run(step.name, func(t *testing.T) {
					active := []healthFinding{}
					if step.active {
						active = append(active, finding)
					}
					before := len(messages)
					tx, err := h.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					err = h.service.commitHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now.Add(step.offset), active)
					want := step.emails
					if test.legacy {
						want -= 2
					}
					if test.unavailable || test.severity == "watch" {
						want = 0
					}
					if (err != nil) != (test.failure && want > before) {
						t.Fatalf("evaluation error=%v", err)
					}
					if len(messages) != want {
						t.Fatalf("emails=%d want %d", len(messages), want)
					}
					for i, message := range messages[before:] {
						recipient := []string{"owner@example.test", "second@example.test"}[i%2]
						if message.To != recipient || message.Subject != "[Detent] test_signal: project "+h.project {
							t.Fatalf("message=%+v", message)
						}
						state := "resolved"
						if step.active {
							state = "opened"
						}
						if !strings.Contains(message.Body, "Finding "+state) {
							t.Fatalf("wrong state: %s", message.Body)
						}
					}
					state := "resolved"
					if step.active {
						state = "open"
					}
					page, err := h.service.readHealthFindings(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, state, "", "", 100)
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Items) == 0 {
						t.Fatal("finding unavailable through health read")
					}
					for _, item := range page.Items {
						if item.EmailUnavailable != (test.unavailable && test.severity == "attention") {
							t.Fatalf("unavailable=%v", item.EmailUnavailable)
						}
					}
				})
			}
		})
	}
}

func TestHealthEmailTemplate(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "email subject")
	s := f.service
	s.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID), PublicURL: "https://cloud.example.test", SharedEntry: &HostedSharedEntry{}}
	for _, test := range []struct{ kind, path, state string }{
		{"work_item", "/local-organization/native/issues/1", "opened"},
		{"runner", "/fleet", "opened"},
		{"project", "/fleet", "opened"},
		{"org", "/fleet", "opened"},
		{"runner", "/fleet", "resolved"},
	} {
		t.Run(test.kind+" "+test.state, func(t *testing.T) {
			opened := time.Date(2026, 10, 6, 20, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
			f := newHealthFinding("signal <script> & health", "instance", test.kind, string(issue.WorkItemID), "Summary <script> & details.\nprivate raw log sentinel", "The owner checks <runner> & acts.", nil, healthEvidence{EventIDs: []string{"private-event", "private-event-two"}, AttemptIDs: []string{"private-attempt"}, Counts: map[string]int{"leases <active> & ready": 3}})
			f.OpenedAt = opened
			subject := test.kind + " " + string(issue.WorkItemID)
			if test.kind == "runner" {
				f.Subject.ID += " <script> & subject"
				subject += " <script> & subject"
			}
			if test.state == "resolved" {
				resolved := opened.Add(time.Hour)
				f.ResolvedAt = &resolved
			}
			message, err := s.renderHealthEmail(t.Context(), f)
			if err != nil {
				t.Fatal(err)
			}
			link := "https://cloud.example.test" + func() string {
				if test.kind == "work_item" {
					return test.path
				}
				return "/local-organization" + test.path
			}()
			if message.Subject != "[Detent] signal <script> & health: "+subject {
				t.Fatalf("subject changed: %q", message.Subject)
			}
			for _, part := range []struct{ name, body string }{{"text", message.Body}, {"HTML", html.UnescapeString(message.HTMLBody)}} {
				for _, want := range []string{"Finding " + test.state, "signal <script> & health", subject, "Summary <script> & details.", "The owner checks <runner> & acts.", "Oct 6, 2026 18:00 UTC", "Events", "Attempts", "leases <active> & ready", link} {
					if !strings.Contains(part.body, want) {
						t.Errorf("%s missing %q: %s", part.name, want, part.body)
					}
				}
				if strings.Contains(part.body, "Oct 6, 2026 19:00 UTC") != (test.state == "resolved") {
					t.Errorf("%s has wrong resolved time: %s", part.name, part.body)
				}
			}
			for _, want := range []string{"Events: 2", "Attempts: 1", "leases <active> & ready: 3"} {
				if !strings.Contains(message.Body, want) {
					t.Errorf("text missing %q", want)
				}
			}
			for _, want := range []string{"&lt;script&gt; &amp; health", "Summary &lt;script&gt; &amp; details.", "&lt;runner&gt; &amp; acts.", "leases &lt;active&gt; &amp; ready", `href="` + link + `"`, ">1</td>", ">2</td>", ">3</td>", "Open " + strings.ReplaceAll(test.kind, "_", " ")} {
				if !strings.Contains(message.HTMLBody, want) {
					t.Errorf("HTML missing %q: %s", want, message.HTMLBody)
				}
			}
			if strings.Contains(message.HTMLBody, "<script>") || strings.Contains(message.HTMLBody, "<runner>") || len(message.InlineImages) != 1 {
				t.Fatal("HTML has unescaped content or lacks the shared mark")
			}
			color := "#fe9a00"
			if test.state == "resolved" {
				color = "#00bc7d"
			}
			if !strings.Contains(message.HTMLBody, "background-color:"+color) {
				t.Error("missing state indicator")
			}
			for _, private := range []string{"private raw log sentinel", "private-event", "private-attempt"} {
				if strings.Contains(message.Body+message.HTMLBody, private) {
					t.Fatalf("private evidence leaked: %s", private)
				}
			}
			for _, credential := range []string{
				"password=hunter2", "api_key=private-key", "token=private-token", "secret='private value'", "Authorization: Bearer abc.def", "ghp_abcdefghijklmnopqrstuvwxyz123456789", "github_pat_private", "sk-proj-private", "AKIA1234567890123456", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.private", "https://user:password@example.test/path?token=private", "0123456789abcdefghijklmnopqrstuvwxyz",
			} {
				f.Summary, f.NextAction, f.Signal = credential, credential, credential
				f.Subject.ID = credential
				f.Subject.Kind = credential
				f.Evidence.Counts = map[string]int{credential: 1}
				message, err := s.renderHealthEmail(t.Context(), f)
				if err != nil {
					t.Fatal(err)
				}
				for _, part := range []string{message.Body, html.UnescapeString(message.HTMLBody), message.Subject} {
					if strings.Contains(part, credential) || !strings.Contains(part, "[redacted]") {
						t.Fatalf("credential not redacted: %q", credential)
					}
				}
			}
		})
	}
}
