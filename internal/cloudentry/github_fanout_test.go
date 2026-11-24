package cloudentry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/cloudentry/webhooks"
)

func TestSharedSchedulerDeliversGitHubWebhookPastFailedTenant(t *testing.T) {
	secret := []byte("example-webhook-secret")
	f, scheduler := newSharedEntryFixture(t, func(cfg *Config) { cfg.GitHubWebhookSecret = secret })
	exec := func(statement string) {
		t.Helper()
		if _, err := f.database.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE project_settings SET repository='example/shared'")
	exec("ALTER TABLE github_webhook_receipts ADD CONSTRAINT refuse_alpha CHECK (organization_id <> 'org_alpha') NOT VALID")
	const payload = `{"repository":{"full_name":"example/shared"},"action":"opened"}`
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "shared-delivery")
	rec := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook intake returned HTTP %d", rec.Code)
	}
	receipts := func() string {
		t.Helper()
		var orgs string
		err := f.database.QueryRowContext(t.Context(), "SELECT coalesce(string_agg(organization_id,',' ORDER BY organization_id),'') FROM github_webhook_receipts WHERE delivery_id='shared-delivery'").Scan(&orgs)
		if err != nil {
			t.Fatal(err)
		}
		return orgs
	}
	if got := receipts(); got != "" {
		t.Fatalf("ingress synchronously delivered to %s", got)
	}
	if err := scheduler.Tick(t.Context()); err == nil {
		t.Fatal("scheduler lost the tenant handoff failure")
	}
	tick := func() {
		t.Helper()
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if got := receipts(); got != "org_beta" {
		t.Fatalf("receipts after alpha failure: %q", got)
	}
	exec("ALTER TABLE github_webhook_receipts DROP CONSTRAINT refuse_alpha")
	exec("UPDATE platform.github_webhook_ingress SET next_attempt_at=now() WHERE delivery_id='shared-delivery' AND attempts>0")
	tick()
	if got := receipts(); got != "org_alpha,org_beta" {
		t.Fatalf("receipts after retry: %q", got)
	}
	tick()
	tick()
	var released bool
	if err := f.database.QueryRowContext(t.Context(), "SELECT state='completed' AND payload_object_key='' FROM platform.github_webhook_ingress WHERE delivery_id='shared-delivery'").Scan(&released); err != nil || !released {
		t.Fatalf("completed fanout released its platform payload: %t, %v", released, err)
	}
	const id = "shared-delivery"
	digest := artifact.Digest([]byte(payload))
	key := webhooks.PayloadKey(id, digest)
	if err := webhooks.StorePayload(t.Context(), f.service.attachments, id, digest, key, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := webhooks.SweepReleasedPayload(t.Context(), f.service.attachments, f.service.registry.store.db.postgres.GitHubIngressReleased, key); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := f.service.attachments.Stat(t.Context(), key); err != nil || exists {
		t.Fatalf("late upload survived release: exists=%v error=%v", exists, err)
	}
}

func TestSharedGitHubRepositoryRouting(t *testing.T) {
	f := newEntryFixture(t)
	for _, tc := range []struct{ id, repo, checkout string }{{"org_alpha", "Example/ONE", "example/shared"}, {"org_beta", "example/shared", "Example/TWO"}} {
		if _, err := f.database.ExecContext(t.Context(), `UPDATE project_settings SET repository=$1,checkout_repository=$2 WHERE organization_id=$3`, tc.repo, tc.checkout, tc.id); err != nil {
			t.Fatal(err)
		}
	}
	store := f.service.registry.store.db.postgres
	for _, tc := range []struct {
		id, repo string
		ids      []string
	}{
		{"primary", "example/one", []string{"org_alpha"}},
		{"checkout", "example/two", []string{"org_beta"}},
		{"shared", "example/shared", []string{"org_alpha", "org_beta"}},
		{"no match", "example/missing", nil},
		{"disabled", "example/shared", []string{"org_alpha"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if tc.id == "disabled" {
				if _, err := f.database.ExecContext(t.Context(), "UPDATE platform.organizations SET state='disabled' WHERE id='org_beta'"); err != nil {
					t.Fatal(err)
				}
			}
			delivery := webhooks.Delivery{ID: tc.id, Repository: tc.repo, Event: "issues"}
			raw := []byte(`{"repository":{"full_name":"` + tc.repo + `"}}`)
			duplicate, err := f.service.acceptGitHubWebhook(t.Context(), delivery, raw)
			if err != nil || duplicate {
				t.Fatalf("ingress duplicate=%v error=%v", duplicate, err)
			}
			var got []string
			for range len(tc.ids) + 3 {
				page, err := store.NextGitHubFanout(t.Context(), f.service.config.Now(), 1)
				if err != nil {
					t.Fatal(err)
				}
				if page == nil {
					break
				}
				for _, target := range page.Targets {
					got = append(got, target.Organization)
				}
				if page.Cleanup {
					if err := f.service.deleteGitHubWebhookPayload(t.Context(), page.DeliveryID, page.Digest, page.ObjectKey); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.AdvanceGitHubFanout(t.Context(), page, false, f.service.config.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(got, tc.ids) {
				t.Fatalf("routes for %q = %v, want %v", tc.repo, got, tc.ids)
			}
		})
	}
}
