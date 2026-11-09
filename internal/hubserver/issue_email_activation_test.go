package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEmailSenderStartupAndIssueDelivery(t *testing.T) {
	repository := projectRecordPostgres(t)
	foreignToken := ""
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f, owner := newChangePolicyPostgresTenantFixture(t, repository, org, &memoryRunDetails{})
		member := f.user(t, "commenter", "member", "commenter@example.test", "", "")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			m, err := w.AccessMember(t.Context(), member.identity.Subject)
			if err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: m.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: m.PrincipalID, ProjectID: f.project})
		}); err != nil {
			t.Fatal(err)
		}
		messages := []auth.EmailMessage{}
		attempts := 0
		var cancelDelivery context.CancelFunc
		rejection := &auth.EmailDeliveryError{Outcome: "rejected", Err: errors.New("example send rejection")}
		var sendFailure error = rejection
		cfg := f.service.config
		cfg.newOutboxTimer = func(time.Duration) outboxTimer {
			return outboxTimer{ticks: make(chan time.Time), reset: func(time.Duration) {}, stop: func() {}}
		}
		at := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
		cfg.now = func() time.Time { return at }
		hosted := *cfg.Hosted
		cfg.Hosted = &hosted
		cfg.Hosted.EmailSender = healthEmailSenderFunc(func(ctx context.Context, message auth.EmailMessage) error {
			attempts++
			if attempts == 2 && cancelDelivery != nil {
				cancelDelivery()
			}
			if sendFailure != nil {
				return sendFailure
			}
			messages = append(messages, message)
			return nil
		})
		if err := f.service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatalf("open configured PostgreSQL email service: %v", err)
		}
		defer service.Close()
		service.stopIssueMailer()
		f.service = service
		if foreignToken != "" {
			requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, "/issue-notifications/unsubscribe?token="+url.QueryEscape(foreignToken), nil), http.StatusNotFound)
		}
		requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, "/issue-notifications/unsubscribe?token=invalid", nil), http.StatusNotFound)
		for _, key := range []string{"first", "second"} {
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example " + key, Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var item tracker.NativeIssue
			decodeHubResponse(t, response, &item)
			response = f.request(t, member, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment_" + key}, Body: "Example discussion"})
			requireNativeStatus(t, response, http.StatusOK)
		}
		now := service.config.now().Add(2 * time.Minute)
		deliveryCtx, cancel := context.WithCancel(t.Context())
		cancelDelivery = cancel
		defer cancel()
		if err := service.deliverIssueEmails(deliveryCtx, now); !errors.Is(err, rejection) {
			t.Fatalf("rejected send error=%v", err)
		}
		if attempts != 2 || len(messages) != 0 {
			t.Fatalf("rejected attempts=%d sent=%d", attempts, len(messages))
		}
		stored, ok := projectRecordDatabases.Load(repository)
		if !ok {
			t.Fatal("PostgreSQL fixture owner missing")
		}
		rows, err := stored.(*sql.DB).QueryContext(t.Context(), `SELECT id FROM issue_notification_receipts WHERE organization_id=$1 ORDER BY id`, org)
		if err != nil {
			t.Fatal(err)
		}
		receipts := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			receipts = append(receipts, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(receipts) != 2 {
			t.Fatalf("receipts=%d", len(receipts))
		}
		cfg = service.config
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err = Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer service.Close()
		service.stopIssueMailer()
		f.service = service
		for _, id := range receipts {
			receipt, err := repository.NotificationReceipt(t.Context(), org, id)
			if err != nil || receipt.Status != "rejected" || receipt.Attempts != 1 {
				t.Fatalf("reopened receipt=%+v err=%v", receipt, err)
			}
		}
		if err := service.deliverIssueEmails(t.Context(), now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 {
			t.Fatal("rejected send retried before its backoff")
		}
		sendFailure = nil
		if err := service.deliverIssueEmails(t.Context(), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		for _, id := range receipts {
			receipt, err := repository.NotificationReceipt(t.Context(), org, id)
			if err != nil || receipt.Status != "sent" || receipt.Attempts != 2 {
				t.Fatalf("sent receipt=%+v err=%v", receipt, err)
			}
		}
		if len(messages) != 2 {
			t.Fatalf("messages=%d want 2", len(messages))
		}
		for _, message := range messages {
			if !strings.Contains(message.Body, "commented") || message.To != "review-admin@example.test" {
				t.Fatalf("message=%+v", message)
			}
			marker := "Unsubscribe from this issue: "
			at := strings.Index(message.Body, marker)
			if at < 0 {
				t.Fatal("missing unsubscribe link")
			}
			link, err := url.Parse(strings.TrimSpace(strings.Split(message.Body[at+len(marker):], "\n")[0]))
			if err != nil {
				t.Fatal(err)
			}
			foreignToken = link.Query().Get("token")
			requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, "/issue-notifications/unsubscribe?token="+url.QueryEscape(foreignToken), nil), http.StatusOK)
		}
		if err := service.deliverIssueEmails(t.Context(), now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 {
			t.Fatal("notification replay sent duplicate messages")
		}

		preferencePath := "/api/v2/organizations/" + string(org) + "/daily-summary-preference"
		var end time.Time
		for _, user := range []hostedSecurityUser{owner, member} {
			response := f.request(t, user, http.MethodPut, preferencePath, map[string]any{"enabled": true, "send_time": "06:00", "time_zone": "UTC", "days": "every_day"})
			requireNativeStatus(t, response, http.StatusOK)
			var preference dailySummaryPreference
			decodeHubResponse(t, response, &preference)
			if preference.NextSend == nil {
				t.Fatal("summary has no due time")
			}
			if !end.IsZero() && !end.Equal(*preference.NextSend) {
				t.Fatal("same schedule produced different windows")
			}
			end = *preference.NextSend
		}
		health := evaluateHealth(at, healthSnapshot{Runners: []healthRunner{{ID: "runner_first", Projects: []string{string(f.project)}, CreatedAt: at.Add(-time.Hour)}, {ID: "runner_second", Projects: []string{string(f.project)}, CreatedAt: at.Add(-time.Hour)}}})
		if err := service.publishHealthEvaluation(t.Context(), org, at, health); err != nil {
			t.Fatal(err)
		}
		due := end.Add(time.Hour)
		sendFailure = rejection
		if err := service.deliverDailySummaries(t.Context(), due); !errors.Is(err, rejection) {
			t.Fatalf("rejected summary error=%v", err)
		}
		if len(messages) != 2 {
			t.Fatal("rejected summary was recorded as sent")
		}
		cfg = service.config
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err = Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer service.Close()
		service.stopIssueMailer()
		f.service = service
		summaryOwner := service.config.AccessRepository.(accountstore.SummaryDeliveryRepository)
		for _, test := range []struct{ user, status string }{{owner.identity.Subject, "rejected"}, {member.identity.Subject, "quiet"}} {
			status, err := summaryOwner.SummaryStatus(t.Context(), org, test.user, end)
			if err != nil || status != test.status {
				t.Fatalf("reopened summary status=%s want=%s err=%v", status, test.status, err)
			}
		}
		sendFailure = nil
		if err := service.deliverDailySummaries(t.Context(), due.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 3 || !strings.Contains(messages[2].Subject, "2 need you") {
			t.Fatalf("daily summary messages=%+v", messages)
		}
		for _, test := range []struct{ user, status string }{{owner.identity.Subject, "sent"}, {member.identity.Subject, "quiet"}} {
			status, err := summaryOwner.SummaryStatus(t.Context(), org, test.user, end)
			if err != nil || status != test.status {
				t.Fatalf("summary receipt user=%s status=%s err=%v", test.user, status, err)
			}
		}
		if err := service.deliverDailySummaries(t.Context(), due); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 3 {
			t.Fatal("daily summary replay sent a duplicate")
		}

	}
}
