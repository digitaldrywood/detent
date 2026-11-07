package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubWebhookReplayRetryAndTenantIsolation(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
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
	}
}
