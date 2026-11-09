package hubserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthIssueMaintenanceStartsAndBoundsPublication(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	now := service.config.now().UTC().Truncate(time.Microsecond)
	fixtures := map[tracker.OrganizationID][]nativeFixture{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		for _, name := range []string{"First", "Second"} {
			f := newNativeFixture(t, service, org, name)
			addHealthBacklog(t, f)
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			runner.enroll(t)
			observation := tracker.NativeAdmissionObservation{RunnerRevision: 1, ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: hubTestPolicy().ID, LastRefreshAt: now, RefreshDuration: time.Second}}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerAdmissionObservation(t.Context(), runner.binding.RunnerID, f.project.ID, observation)
			}); err != nil {
				t.Fatal(err)
			}
			fixtures[org] = append(fixtures[org], f)
		}
		admin := healthFixtureDatabase(t, fixtures[org][0])
		if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$1,created_at=$2 WHERE organization_id=$3`, now.Add(-6*time.Minute), now.Add(-time.Hour), org); err != nil {
			t.Fatal(err)
		}
		snapshot, err := service.readHealthSnapshotRecords(t.Context(), org, now)
		if err != nil {
			t.Fatal(err)
		}
		findings := evaluateHealth(now, snapshot)
		if len(findings) != 2 {
			t.Fatalf("organization=%s findings=%+v", org, findings)
		}
	}
	first := hubmaintenance.Tenant{Organization: "org_first", Generation: 1}
	second := hubmaintenance.Tenant{Organization: "org_first_second", Generation: 1}
	for _, feature := range []hubmaintenance.Feature{service.HealthIssueMaintenanceFeature(), service.HealthEvaluationMaintenanceFeature()} {
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: feature.Bytes}
		if err := feature.Step(t.Context(), first, budget); err == nil {
			t.Fatalf("%s accepted an unallocated context", feature.Name)
		}
		if err := feature.Step(budget.Context(t.Context(), first), second, budget); err == nil {
			t.Fatalf("%s accepted another tenant allocation", feature.Name)
		}
		insufficient := hubmaintenance.Budget{Items: 1, Bytes: feature.Bytes - 1}
		if err := feature.Step(insufficient.Context(t.Context(), first), first, insufficient); err == nil {
			t.Fatalf("%s accepted an insufficient byte allocation", feature.Name)
		}
	}
	cfg := service.config
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		cfg.Hosted = &HostedConfig{OrganizationID: string(org), WorkOSOrganizationID: "provider_" + string(org), PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
		ready := make(chan struct{})
		ticks := make(chan chan error)
		cfg.runMaintenance = func(ctx context.Context, scheduler *hubmaintenance.Scheduler, _ func(error)) error {
			close(ready)
			for {
				select {
				case result := <-ticks:
					result <- scheduler.Tick(ctx)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		reopened, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		select {
		case <-ready:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
		hosted := hostedSecurityFixture{service: reopened, provider: cfg.Hosted.Provider.(*hostedSecurityProvider)}
		identity := auth.Identity{Subject: "health_owner", Email: "owner@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: "health_owner", OrganizationID: cfg.Hosted.WorkOSOrganizationID, SessionID: "health_session", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
		hosted.provider.mu.Lock()
		hosted.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
		hosted.provider.mu.Unlock()
		membership, err := hosted.provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, "owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := reopened.addHostedMember(t.Context(), identity, membership); err != nil {
			t.Fatal(err)
		}
		token, _, err := reopened.hostedSessions.CreateIdentitySession(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		owner := hostedSecurityUser{identity: identity, token: token}
		linked := 0
		completed := false
		for range 3 * 16 {
			result := make(chan error, 1)
			select {
			case ticks <- result:
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			current := 0
			completed = true
			for _, f := range fixtures[org] {
				response := hosted.request(t, owner, http.MethodGet, f.base+"/health/findings?state=all", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page healthFindingsPage
				decodeHubResponse(t, response, &page)
				if len(page.Items) > 1 || len(page.Items) == 1 && len(page.Items[0].Issues) > 1 {
					t.Fatalf("organization=%s project=%s findings=%+v", org, f.project.ID, page)
				}
				if len(page.Items) == 1 {
					current += len(page.Items[0].Issues)
				}
				completed = completed && page.LastTickAt != nil
			}
			if current < linked || current-linked > 1 {
				t.Fatalf("organization=%s publication step advanced from %d to %d", org, linked, current)
			}
			linked = current
			if linked == len(fixtures[org]) && completed {
				break
			}
		}
		if linked != 2 || !completed {
			t.Fatalf("organization=%s background publications=%d completed=%t", org, linked, completed)
		}
		for _, f := range fixtures[org] {
			path := f.base + "/health/findings"
			requireNativeStatus(t, performHubAPIRequest(t, reopened, http.MethodGet, path, "", nil), http.StatusUnauthorized)
			other := tracker.OrganizationID("org_first")
			if org == other {
				other = "org_first_second"
			}
			requireNativeStatus(t, hosted.request(t, owner, http.MethodGet, fixtures[other][0].base+"/health/findings", nil), http.StatusNotFound)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
		cfg.runMaintenance = waitForMaintenanceFixtureShutdown
		retained, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = retained.Close() })
		hosted.service = retained
		for _, f := range fixtures[org] {
			response := hosted.request(t, owner, http.MethodGet, f.base+"/health/findings?state=all", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthFindingsPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || len(page.Items[0].Issues) != 1 || page.LastTickAt == nil {
				t.Fatalf("organization=%s lost background findings after reopen: %+v", org, page)
			}
		}
		if err := retained.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForMaintenanceFixtureShutdown(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
	<-ctx.Done()
	return ctx.Err()
}
