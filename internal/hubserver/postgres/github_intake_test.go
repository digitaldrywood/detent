package postgres_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubWebhookReplayRetryAndTenantIsolation(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	expectLatest := func(org tracker.OrganizationID, want *time.Time) {
		t.Helper()
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			received, err := r.(projectstore.GitHubIntakeReader).LatestGitHubWebhookReceipt(t.Context())
			if want == nil {
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("org=%s empty latest=%v err=%v", org, received, err)
				}
				return nil
			}
			if err != nil {
				return err
			}
			if !received.Equal(*want) {
				t.Fatalf("org=%s latest=%v want=%v", org, received, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	latestByOrganization := make(map[tracker.OrganizationID]time.Time)
	for index, org := range []tracker.OrganizationID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		expectLatest(org, nil)
		for _, id := range []string{"first", "second"} {
			key, err := attachment.ArchiveKey(string(org), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			if err != nil {
				t.Fatal(err)
			}
			row := projectstore.GitHubWebhook{DeliveryID: id, Repository: "example-org/example-project", EventType: "issues", Action: "opened", Digest: "digest", ReceivedAt: now, ExpiresAt: now.Add(time.Hour), NextAttemptAt: now, Status: "pending", Payload: eventstore.Receipt{OrganizationID: org, PayloadObjectKey: key, PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PayloadBytes: 1, PayloadContentBytes: 1}}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubIntakeWriter)
				created, err := owner.CreateGitHubWebhook(t.Context(), row)
				if err != nil {
					return err
				}
				if !created {
					t.Fatal("first delivery was a duplicate")
				}
				created, err = owner.CreateGitHubWebhook(t.Context(), row)
				if err != nil {
					return err
				}
				if created {
					t.Fatal("replay created another receipt")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		expectLatest(org, &now)
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(projectstore.GitHubIntakeWriter)
			row, err := owner.NextGitHubWebhook(t.Context(), now)
			if err != nil {
				return err
			}
			if row.DeliveryID != "first" || row.Payload.OrganizationID != org {
				t.Fatalf("selected=%+v", row)
			}
			row.AfterProject = "project_one"
			row.Attempts = 1
			row.NextAttemptAt = now.Add(time.Minute)
			if err := owner.UpdateGitHubWebhook(t.Context(), row, "", 0); err != nil {
				return err
			}
			if err := owner.UpdateGitHubWebhook(t.Context(), row, "", 0); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale update=%v", err)
			}
			row, err = owner.NextGitHubWebhook(t.Context(), now)
			if err != nil {
				return err
			}
			if row.DeliveryID != "second" {
				t.Fatalf("retry starved ready delivery: %+v", row)
			}
			row.Status = "completed"
			row.Attempts++
			return owner.UpdateGitHubWebhook(t.Context(), row, "", 0)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubIntakeWriter).PurgeGitHubWebhookPayloads(t.Context(), now.Add(time.Hour), 1)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner := r.(projectstore.GitHubIntakeReader)
			pending, err := owner.GitHubWebhook(t.Context(), "first")
			if err != nil {
				return err
			}
			if pending.Payload.PayloadObjectKey == "" || pending.AfterProject != "project_one" {
				t.Fatalf("pending recovery lost: %+v", pending)
			}
			completed, err := owner.GitHubWebhook(t.Context(), "second")
			if err != nil {
				return err
			}
			if completed.Payload.PayloadObjectKey != "" || completed.Digest != "digest" {
				t.Fatalf("retention lost replay identity: %+v", completed)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		original := make(map[string]projectstore.GitHubWebhook)
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, id := range []string{"first", "second"} {
				row, err := r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), id)
				if err != nil {
					return err
				}
				original[id] = row
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		latest := now.Add(time.Duration(index+1) * time.Minute)
		observation := original["first"]
		observation.ReceivedAt = latest
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubIntakeWriter).ObserveGitHubWebhookReceipt(t.Context(), observation)
		}); err != nil {
			t.Fatal(err)
		}
		expectLatest(org, &latest)
		for _, observed := range []time.Time{now, latest} {
			observation.ReceivedAt = observed
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.GitHubIntakeWriter).ObserveGitHubWebhookReceipt(t.Context(), observation)
			}); err != nil {
				t.Fatalf("matching older/equal observation %v: %v", observed, err)
			}
			expectLatest(org, &latest)
		}
		for _, change := range []struct {
			name string
			edit func(*projectstore.GitHubWebhook)
		}{
			{name: "digest", edit: func(row *projectstore.GitHubWebhook) { row.Digest = "different" }},
			{name: "event", edit: func(row *projectstore.GitHubWebhook) { row.EventType = "different" }},
			{name: "repository", edit: func(row *projectstore.GitHubWebhook) { row.Repository = "example-org/other-project" }},
			{name: "missing delivery", edit: func(row *projectstore.GitHubWebhook) { row.DeliveryID = "missing" }},
		} {
			refused := original["first"]
			refused.ReceivedAt = latest.Add(time.Hour)
			change.edit(&refused)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.(projectstore.GitHubIntakeWriter).ObserveGitHubWebhookReceipt(t.Context(), refused); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("%s observation err=%v", change.name, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			expectLatest(org, &latest)
		}
		observation.ReceivedAt = latest.Add(time.Hour)
		rollback := errors.New("example receipt rollback")
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.(projectstore.GitHubIntakeWriter).ObserveGitHubWebhookReceipt(t.Context(), observation); err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			t.Fatalf("receipt rollback err=%v", err)
		}
		expectLatest(org, &latest)
		if _, err := admin.ExecContext(t.Context(), "UPDATE github_webhook_receipts SET last_received_at=NULL WHERE organization_id=$1", org); err != nil {
			t.Fatal(err)
		}
		expectLatest(org, &now)
		observation = original["second"]
		latest = latest.Add(time.Second)
		observation.ReceivedAt = latest
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.GitHubIntakeWriter).ObserveGitHubWebhookReceipt(t.Context(), observation)
		}); err != nil {
			t.Fatal(err)
		}
		expectLatest(org, &latest)
		latestByOrganization[org] = latest
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, id := range []string{"first", "second"} {
				row, err := r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), id)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(row, original[id]) {
					t.Fatalf("receipt observation changed original processing record: before=%+v after=%+v", original[id], row)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for org, latest := range latestByOrganization {
		expectLatest(org, &latest)
	}
}
