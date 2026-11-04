package hubserver

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMonthlyBudgetPut(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(67)
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			now := time.Now().UTC()
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			cfg := startupConfig(connection)
			cfg.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			cfg.Hosted.OrganizationID = string(organization)
			cfg.Hosted.WorkOSOrganizationID = "provider_" + string(organization)
			cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
			cfg.RunDetails = &memoryRunDetails{}
			plans := pilotHostedPlans()
			cfg.Hosted.Plans = &plans
			store, err := openDatabase(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			seedStartupOrganization(t, store.postgres, organization)
			err = store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, role := range []string{"owner", "admin", "member"} {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "principal_" + role, Name: role, Hash: apikey.HashToken(string(organization) + role), Scope: "operator", UserID: "user_" + role, MembershipID: "membership_" + role, Kind: "personal", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_" + role, Email: role + "@example.test", MembershipID: "membership_" + role, Role: role, Active: true, PrincipalID: "principal_" + role, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project_example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				for _, role := range []string{"owner", "admin"} {
					if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_" + role, ProjectID: "project_example", CanWrite: true}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "principal_" + role, ProjectID: "project_example"}); err != nil {
						return err
					}
				}

				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			if service.database.reader != nil || service.database.db != nil {
				t.Fatal("legacy database handles exist")
			}
			service.config.Hosted.SharedEntry.verifier.Now = cfg.now
			sessionTime := now
			call := func(method, suffix, role, support, body string) *httptest.ResponseRecorder {
				t.Helper()
				path := "/organizations/" + string(organization) + "/api/v2/organizations/" + string(organization) + suffix
				id, err := cloudassert.NewID()
				if err != nil {
					t.Fatal(err)
				}
				issued := cfg.now()
				claims := cloudassert.Claims{Issuer: "entry", Audience: string(organization), Generation: 1, Kind: cloudassert.KindBrowser, Method: method, Path: path, BodyDigest: cloudassert.BodyDigest([]byte(body)), IssuedAt: issued, ExpiresAt: issued.Add(20 * time.Second), ID: id, Subject: "user_" + role, Email: role + "@example.test", ProviderOrganization: "provider_" + string(organization), ProviderSession: "session_" + role, SessionCreatedAt: sessionTime, SessionExpiresAt: sessionTime.Add(48 * time.Hour), AccessExpiresAt: sessionTime.Add(48 * time.Hour), Role: role, Binding: cloudassert.AuthorizationBinding("shared-user_"+role, string(organization), "session_"+role), CSRF: cloudassert.CSRFToken("shared-user_"+role, string(organization)), SupportActor: support}
				if support != "" {
					claims.SupportReason = "Example support investigation"
					claims.ProviderSession += "_support"
					claims.Binding = cloudassert.AuthorizationBinding("shared-user_"+role, string(organization), claims.ProviderSession)
				}
				assertion, err := cloudassert.Sign(key, claims)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(method, path, strings.NewReader(body))
				request.Header.Set(cloudassert.Header, assertion)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-CSRF-Token", claims.CSRF)
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				return response
			}
			body := `{"idempotency_key":"budget_example","expected_revision":"0","policy":{"enabled":true,"mode":"hard_stop","luna_api_micros":11,"runner_api_micros":22,"sprite_infrastructure_micros":33,"total_micros":1234}}`
			first := call(http.MethodPut, "/monthly-budget", "owner", "", body)
			requireNativeStatus(t, first, http.StatusOK)
			var saved monthlyBudgetSettings
			decodeHubResponse(t, first, &saved)
			if saved.Revision != 1 || !saved.Policy.Enabled || saved.Policy.Mode != "hard_stop" || saved.Policy.LunaMicros == nil || *saved.Policy.LunaMicros != 11 || saved.Policy.RunnerMicros == nil || *saved.Policy.RunnerMicros != 22 || saved.Policy.SpriteMicros == nil || *saved.Policy.SpriteMicros != 33 || saved.Policy.TotalMicros == nil || *saved.Policy.TotalMicros != 1234 {
				t.Fatal("policy fidelity lost")
			}
			for _, candidate := range []string{strings.Replace(body, "1234", "4321", 1), strings.Replace(body, "budget_example", "stale_example", 1)} {
				requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", candidate), http.StatusConflict)
			}
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "member", "", body), http.StatusForbidden)
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "support@example.test", body), http.StatusUnauthorized)
			requireNativeStatus(t, call(http.MethodPut, "/projects/project_example/monthly-budget", "owner", "", body), http.StatusOK)
			newer := strings.Replace(strings.Replace(body, `"0"`, `"1"`, 1), "1234", "5678", 1)
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "admin", "", newer), http.StatusOK)
			snapshot := func() (int64, int64) {
				t.Helper()
				var archive, usage int64
				if err := admin.QueryRowContext(t.Context(), `SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1`, organization).Scan(&archive); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), `SELECT COALESCE(sum(amount),0) FROM hosted_usage_receipts WHERE organization_id=$1 AND metric='api_mutations'`, organization).Scan(&usage); err != nil {
					t.Fatal(err)
				}
				return archive, usage
			}
			beforeArchive, beforeUsage := snapshot()
			replay := call(http.MethodPut, "/monthly-budget", "owner", "", body)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != first.Body.String() {
				t.Fatal("historical replay changed")
			}
			afterArchive, afterUsage := snapshot()
			if beforeArchive != afterArchive || beforeUsage != afterUsage || beforeUsage != 3 {
				t.Fatalf("replay accounting: archive %d/%d mutations %d/%d", beforeArchive, afterArchive, beforeUsage, afterUsage)
			}

			var collaboration int64
			if err := admin.QueryRowContext(t.Context(), `SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'`, organization).Scan(&collaboration); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_usage_counters SET value=$2 WHERE organization_id=$1 AND name='collaboration_bytes'`, organization, 64<<20); err != nil {
				t.Fatal(err)
			}
			refused := `{"idempotency_key":"over_quota","expected_revision":"2","policy":{"enabled":false}}`
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "admin", "", refused), http.StatusTooManyRequests)
			quotaArchive, quotaUsage := snapshot()
			if quotaArchive != beforeArchive || quotaUsage != beforeUsage {
				t.Fatal("quota refusal changed archive or usage")
			}
			var refusedReceipts int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_command_states WHERE organization_id=$1 AND command_key='over_quota'`, organization).Scan(&refusedReceipts); err != nil {
				t.Fatal(err)
			}
			if refusedReceipts != 0 {
				t.Fatal("quota refusal committed receipt")
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_usage_counters SET value=$2 WHERE organization_id=$1 AND name='collaboration_bytes'`, organization, collaboration); err != nil {
				t.Fatal(err)
			}

			var receipt eventstore.Receipt
			operation := "PUT /api/v2/organizations/" + string(organization) + "/monthly-budget"
			if err := service.database.billingRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				receipt, err = r.Receipt(t.Context(), "principal_owner", operation, "budget_example")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !receipt.CreatedAt.Equal(now.Truncate(time.Microsecond)) || receipt.ResponseReplayUntilNs != now.Truncate(time.Microsecond).Add(receiptstore.ReplayRetention).UnixNano() {
				t.Fatal("noncanonical receipt retention")
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_members SET active=false WHERE organization_id=$1 AND user_id='user_owner'`, organization); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", body), http.StatusForbidden)
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", strings.Replace(body, "budget_example", "revoked_fresh", 1)), http.StatusForbidden)
			if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_members SET active=true WHERE organization_id=$1 AND user_id='user_owner'`, organization); err != nil {
				t.Fatal(err)
			}
			now = time.Unix(0, receipt.ResponseReplayUntilNs-1).UTC()
			clock.Store(now.UnixNano())
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", body), http.StatusOK)
			now = now.Add(time.Nanosecond)
			clock.Store(now.UnixNano())
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", body), http.StatusGone)

			if err := service.database.billingRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, now)
				if err == nil && expired {
					t.Fatal("retention removed response at exact cutoff")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}

			now = now.Add(time.Nanosecond)
			clock.Store(now.UnixNano())
			if err := service.database.billingRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, now)
				if err == nil && !expired {
					t.Fatal("receipt not expired")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, call(http.MethodPut, "/monthly-budget", "owner", "", body), http.StatusGone)
			if err := service.database.billingRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				org, err := r.BillingMonthlyPolicy(t.Context(), "")
				if err != nil {
					return err
				}
				project, err := r.BillingMonthlyPolicy(t.Context(), "project_example")
				if err != nil {
					return err
				}

				for _, item := range []struct {
					got      billingstore.MonthlyPolicy
					project  tracker.ProjectID
					actor    string
					revision tracker.Revision
					total    int64
				}{{org, "", "principal_admin", 2, 5678}, {project, "project_example", "principal_owner", 1, 1234}} {
					want := billingstore.MonthlyPolicy{OrganizationID: organization, ProjectID: item.project, Revision: item.revision, Actor: item.actor, UpdatedAt: receipt.CreatedAt, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, LunaMicros: new(int64(11)), RunnerMicros: new(int64(22)), SpriteMicros: new(int64(33)), TotalMicros: new(item.total)}}
					if !reflect.DeepEqual(item.got, want) {
						t.Fatalf("stored policy fields differ for project %q", item.project)
					}
				}

				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
