//go:build !windows

package cloudentry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
)

type platformTimeoutTransport struct{}

func (platformTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestPlatformTenantDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	t.Parallel()
	f := newEntryFixture(t)
	endpoint := testSocketEndpoint("alpha.sock")
	tenant := f.tenants[endpoint]
	f.tenants[endpoint] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/platform/ai-credits/balance" {
			if err := json.NewEncoder(w).Encode(map[string]int64{"balance_micros": 10250000}); err != nil {
				t.Error(err)
			}
			return
		}
		tenant.ServeHTTP(w, r)
	})
	for _, role := range []string{"viewer", "support", "billing", "admin"} {
		user := "user_" + role
		email := role + "@example.test"
		f.provider.users[user] = email
		if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?,'test','time','time') ON CONFLICT(email) DO UPDATE SET role=excluded.role", email, role); err != nil {
			t.Fatal(err)
		}
		b := newBrowser(t, f.service.Handler())
		b.login("/auth/oidc/start", user+":")
		t.Run(role, func(t *testing.T) {
			response, body := b.get("/api/cloud/platform/organizations/org_alpha")
			var detail platformTenantDetail
			decodeJSON(t, body, &detail)
			if response.StatusCode != http.StatusOK || detail.Organization.ID != "org_alpha" || detail.CSRF == "" || detail.CanGrant != (role == "billing" || role == "admin") || detail.Organization.CanSupport != (role == "support" || role == "admin") || detail.CanResume {
				t.Fatalf("detail = %d %+v", response.StatusCode, detail)
			}
			if detail.CreditBalanceMicros == nil || *detail.CreditBalanceMicros != 10250000 || slices.Contains(detail.Unavailable, "ai_credits") {
				t.Fatalf("credit balance = %+v", detail)
			}
			if detail.Usage == nil || detail.Usage.Allowances == nil || detail.Members == nil || len(*detail.Members) != 1 || (*detail.Members)[0].Role != "owner" || detail.Runners == nil || detail.Projects == nil || len(*detail.Projects) == 0 || detail.Entitlements != nil || !slices.Contains(detail.Unavailable, "entitlements") || len(detail.Events) == 0 {
				t.Fatalf("sections = %+v", detail)
			}
			if response, _ := b.get("/api/cloud/platform/organizations/org_unknown"); response.StatusCode != http.StatusNotFound {
				t.Fatalf("unknown = %d", response.StatusCode)
			}
		})
	}
	customer := newBrowser(t, f.service.Handler())
	customer.login("/auth/oidc/start", "user_alice:")
	if response, _ := customer.get("/api/cloud/platform/organizations/org_alpha"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("customer = %d", response.StatusCode)
	}
	anonymous := newBrowser(t, f.service.Handler())
	if response, _ := anonymous.get("/api/cloud/platform/organizations/org_alpha"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", response.StatusCode)
	}
	admin := newBrowser(t, f.service.Handler())
	admin.login("/auth/oidc/start", "user_admin:")
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='failed',error_code='tenant_start_failed',error_detail='disk floor',step='tenant_files' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	response, body := admin.get("/api/cloud/platform/organizations/org_alpha")
	var detail platformTenantDetail
	decodeJSON(t, body, &detail)
	if response.StatusCode != http.StatusOK || detail.Organization.ErrorCode != "tenant_start_failed" || detail.Organization.ErrorDetail != "disk floor" || detail.Organization.Step != "tenant_files" || detail.CreditBalanceMicros != nil || len(detail.Unavailable) != 7 {
		t.Fatalf("failed tenant = %d %+v", response.StatusCode, detail)
	}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='ready' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	f.service.config.transport = func(Organization) (http.RoundTripper, error) { return platformTimeoutTransport{}, nil }
	response, body = admin.get("/api/cloud/platform/organizations/org_alpha")
	decodeJSON(t, body, &detail)
	if response.StatusCode != http.StatusOK || detail.Members != nil || detail.Runners != nil || detail.Projects != nil || detail.Entitlements != nil || detail.CreditBalanceMicros != nil || detail.Organization.Billing.Available || len(detail.Unavailable) != 7 {
		t.Fatalf("timeout = %d %+v", response.StatusCode, detail)
	}

}

func TestPlatformTenantAICreditBalance(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	f.provider.users["user_admin"] = "admin@example.test"
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES('admin@example.test','admin','test','time','time')"); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, f.service.Handler())
	b.login("/auth/oidc/start", "user_admin:")
	endpoint := testSocketEndpoint("alpha.sock")
	tenant := f.tenants[endpoint]
	for _, test := range []struct {
		name, body  string
		status      int
		want        int64
		unavailable bool
	}{
		{"full credit detail", `{"balance_micros":10250000,"spent_micros":750000,"month":"2026-10","auto_enabled":true,"history":[{"amount_micros":25000000,"kind":"complimentary","actor":"staff@example.test","reason":"Pilot credits","at":"2026-10-03T00:00:00Z"}]}`, http.StatusOK, 10250000, false},
		{"positive", `{"balance_micros":10250000}`, http.StatusOK, 10250000, false},
		{"zero", `{"balance_micros":0}`, http.StatusOK, 0, false},
		{"negative", `{"balance_micros":-1}`, http.StatusOK, -1, false},
		{"failed read", `{"code":"ai_credits_unavailable"}`, http.StatusInternalServerError, 0, true},
		{"malformed", `{`, http.StatusOK, 0, true},
		{"missing balance", `{}`, http.StatusOK, 0, true},
		{"null balance", `{"balance_micros":null}`, http.StatusOK, 0, true},
		{"wrong type", `{"balance_micros":"5"}`, http.StatusOK, 0, true},
		{"null response", `null`, http.StatusOK, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.tenants[endpoint] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/platform/ai-credits/balance" {
					w.WriteHeader(test.status)
					if _, err := w.Write([]byte(test.body)); err != nil {
						t.Error(err)
					}
					return
				}
				tenant.ServeHTTP(w, r)
			})
			response, body := b.get("/api/cloud/platform/organizations/org_alpha")
			var detail platformTenantDetail
			decodeJSON(t, body, &detail)
			if response.StatusCode != http.StatusOK || (detail.CreditBalanceMicros == nil) != test.unavailable || slices.Contains(detail.Unavailable, "ai_credits") != test.unavailable {
				t.Fatalf("detail = %d %+v", response.StatusCode, detail)
			}
			if test.name == "full credit detail" {
				if detail.AICredits == nil || detail.AICredits.SpentMicros != 750000 || detail.AICredits.Month != "2026-10" || !detail.AICredits.AutoEnabled || len(detail.AICredits.History) != 1 || detail.AICredits.History[0].AmountMicros != 25000000 || detail.AICredits.History[0].Actor != "staff@example.test" || detail.AICredits.History[0].Reason != "Pilot credits" {
					t.Fatalf("credits=%+v", detail.AICredits)
				}
			} else if detail.AICredits != nil {
				t.Fatalf("partial credits=%+v", detail.AICredits)
			}
			if !test.unavailable && *detail.CreditBalanceMicros != test.want {
				t.Fatalf("balance = %d, want %d", *detail.CreditBalanceMicros, test.want)
			}
		})
	}
}

func TestPlatformResumeProvisioning(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	t.Parallel()
	f := newEntryFixture(t)
	f.service.config.Allocation = &AllocationConfig{}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,state,endpoint,generation,managed,creator_subject,creator_email,error_code,step,created_at,updated_at) VALUES('org_failed','Failed','failed','unix:/unused.sock',1,1,'user_alice','alice@example.test','tenant_start_failed','tenant_files','2026-10-05T12:00:00Z','2026-10-05T12:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		role string
		csrf bool
		want int
	}{
		{"viewer", true, http.StatusForbidden}, {"support", true, http.StatusForbidden}, {"billing", true, http.StatusForbidden}, {"admin", false, http.StatusForbidden}, {"admin", true, http.StatusOK},
	} {
		t.Run(test.role+map[bool]string{true: " valid csrf", false: " no csrf"}[test.csrf], func(t *testing.T) {
			user, email := "user_"+test.role, test.role+"@example.test"
			f.provider.users[user] = email
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform_members VALUES(?,?,'test','time','time') ON CONFLICT(email) DO NOTHING", email, test.role); err != nil {
				t.Fatal(err)
			}
			b := newBrowser(t, f.service.Handler())
			b.login("/auth/oidc/start", user+":")
			_, body := b.get("/api/cloud/platform/organizations/org_failed")
			var detail platformTenantDetail
			decodeJSON(t, body, &detail)
			if detail.CanResume != (test.role == "admin") {
				t.Fatalf("can_resume = %v, detail=%s", detail.CanResume, body)
			}
			fields := url.Values{}
			if test.csrf {
				fields.Set("csrf", detail.CSRF)
			}
			response := b.do(http.MethodPost, "/api/cloud/platform/organizations/org_failed/resume", fields, map[string]string{"Accept": "application/json"})
			body = response.Body
			if response.StatusCode != test.want {
				t.Fatalf("resume = %d %s", response.StatusCode, body)
			}
			if test.want == http.StatusOK {
				var next map[string]string
				if err := json.Unmarshal([]byte(body), &next); err != nil || next["next"] != "/platform/tenants/org_failed" {
					t.Fatalf("result = %s %v", body, err)
				}
				organization, err := f.service.registry.Organization(t.Context(), "org_failed")
				if err != nil || organization.State != "allocating" || organization.ErrorCode != "" {
					t.Fatalf("resumed = %+v %v", organization, err)
				}
			}
		})
	}
}
