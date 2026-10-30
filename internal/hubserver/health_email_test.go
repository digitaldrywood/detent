package hubserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubhealth"
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
			finding := hubhealth.NewFinding("test_signal", "instance", "project", h.project, "Finding summary.", "The owner checks the runner.", []string{h.project}, hubhealth.Evidence{Counts: map[string]int{"leases": 2}})
			finding.ConditionSince = now.Add(-48 * time.Hour)
			finding.Severity = test.severity
			if test.legacy || test.rollback {
				if test.rollback {
					if _, err := h.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_health_tick BEFORE INSERT ON health_detector_ticks BEGIN SELECT RAISE(ABORT, 'fixture health tick failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				if test.legacy {
					tx, err := h.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					_, err = writeHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now.Add(-time.Minute), []hubhealth.Finding{finding})
					if err != nil {
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				} else {
					err := h.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, now, []hubhealth.Finding{finding}, nil)
					if err == nil {
						t.Error("finding transaction succeeded")
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
					active := []hubhealth.Finding{}
					if step.active {
						active = append(active, finding)
					}
					before := len(messages)
					err := h.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, now.Add(step.offset), active, nil)
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

func TestHealthEmailLinks(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "email subject")
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID), PublicURL: "https://cloud.example.test", SharedEntry: &HostedSharedEntry{}}
	for _, test := range []struct{ kind, path string }{
		{"work_item", "/local-organization/native/issues/1"},
		{"runner", "/local-organization/fleet"},
		{"project", "/local-organization/fleet"},
		{"org", "/local-organization/fleet"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			finding := hubhealth.NewFinding("signal", "instance", test.kind, string(issue.WorkItemID), "Summary", "Check finding", nil, hubhealth.Evidence{})
			message, err := f.service.renderHealthEmail(t.Context(), finding)
			if err != nil {
				t.Fatal(err)
			}
			link := "https://cloud.example.test" + test.path
			if !strings.Contains(message.Body, link) || !strings.Contains(message.HTMLBody, `href="`+link+`"`) {
				t.Fatalf("missing canonical finding link %q", link)
			}
		})
	}
}
