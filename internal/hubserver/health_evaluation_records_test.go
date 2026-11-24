package hubserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHealthEvaluationPublishesRecurrenceAndClear(t *testing.T) {
	t.Parallel()
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	now := service.config.Clock().UTC().Truncate(time.Microsecond)
	projects := map[tracker.OrganizationID]tracker.ProjectID{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f := newNativeFixture(t, service, org, "Example")
		projects[org] = f.project.ID
		repository := service.database.postgres
		envelope, err := service.config.SecretKeys.Seal([]byte("https://hooks.slack.com/services/example/example/example"), accessstore.SecretAAD(org, "", slackWebhookSecret))
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteSlack(t.Context(), org, corestore.Serving, func(w postgres.SlackWriter) error {
			if _, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: org, ChannelName: "example", Enabled: true, UpdatedAt: now}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: slackWebhookSecret, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		findings := healthsignals.Evaluate(now, healthSnapshot{Runners: []healthRunner{{ID: "runner_first", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}, {ID: "runner_second", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}}})
		if len(findings) != 2 {
			t.Fatalf("findings=%+v", findings)
		}
		ids := map[string]string{}
		for _, step := range []struct {
			age   time.Duration
			clear bool
		}{{0, false}, {time.Minute, false}, {2 * time.Minute, true}, {3 * time.Minute, false}, {4 * time.Minute, true}, {2 * time.Hour, false}} {
			active := findings
			if step.clear {
				active = nil
			}
			if err := service.publishHealthEvaluation(t.Context(), org, now.Add(step.age), active); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings?state=all", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthprojection.Page
			decodeHubResponse(t, response, &page)
			want := 2
			if step.age == 2*time.Hour {
				want = 4
			}
			if len(page.Items) != want {
				t.Fatalf("step=%s page=%+v", step.age, page)
			}
			if page.LastTickAt == nil || !page.LastTickAt.Equal(now.Add(step.age)) {
				t.Fatalf("tick=%v want=%v", page.LastTickAt, now.Add(step.age))
			}
			open := 0
			for _, finding := range page.Items {
				if finding.ResolvedAt == nil {
					open++
				}
				if step.age == 0 {
					ids[finding.Fingerprint] = finding.ID
				}
				if step.age < 2*time.Hour && ids[finding.Fingerprint] != finding.ID {
					t.Fatal("recurrence lost finding identity")
				}
				count, present := finding.Evidence.Counts["leases"]
				if finding.Signal != "runner_heartbeat_gap" || !present || count != 0 {
					t.Fatalf("finding=%+v", finding)
				}
			}
			if step.clear && open != 0 || !step.clear && open != 2 {
				t.Fatalf("step=%s open=%d", step.age, open)
			}

		}
	}
	cfg := service.config
	cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = nil, nil, nil
	cfg.Hosted = &hostedplans.HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
	tamperHostedBinding(t, hostedPostgresAdmin(t, service), "UPDATE hosted_tenant SET deployment='origin',public_url=$2 WHERE organization_id=$1", cfg.Hosted.OrganizationID, cfg.Hosted.PublicURL)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		page, err := reopened.readHealthFindings(t.Context(), hubscope.NativeScope{Organization: org, Project: projects[org]}, "all", "", "", 100)
		if err != nil || len(page.Items) != 4 {
			t.Fatalf("reopened organization=%s page=%+v err=%v", org, page, err)
		}
	}
}

func (s *Service) publishHealthEvaluation(ctx context.Context, organization tracker.OrganizationID, at time.Time, findings []healthFinding) error {
	return s.healthProjection().Publish(ctx, organization, at, findings, healthprojection.Scope{Complete: true})
}

func newHealthFinding(signal, class, kind, id, summary, action string, projects []string, evidence healthEvidence) healthFinding {
	return healthsignals.NewFinding(signal, class, kind, id, summary, action, projects, evidence)
}
