package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedBillingDowngradePreservesGrantsAndData(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, hold string
		grant      bool
	}{
		{"canceled", "", false}, {"refunded", "refunded", false}, {"disputed", "disputed", false}, {"complimentary", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, p := newHostedBillingFixture(t)
			p.snapshot = activeBillingSnapshot(time.Now())
			if err := f.service.billing.reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			project := f.createProject(t, "Paid project")
			if test.grant {
				entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
				if err != nil {
					t.Fatal(err)
				}
				if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), "operator_fixture", hostedplans.PlanCommand{ID: "grant_fixture", GrantID: "complimentary_fixture", Action: "grant", ExpectedRevision: entitlement.Revision, Plan: hostedplans.PlanReference{ID: "test_paid", Version: 1}, Scope: []string{"projects"}, Reason: "Approved complimentary pilot access"})); err != nil {
					t.Fatal(err)
				}
			}
			if test.hold != "" {
				p.snapshot.PaymentHold = test.hold
			} else {
				p.snapshot = billing.Snapshot{Status: "canceled"}
			}
			if err := f.service.billing.reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			response := f.form(t, "owner", "/projects", url.Values{"name": {"New excess project"}, "grant_access": {"true"}})
			want := http.StatusTooManyRequests
			if test.grant {
				want = http.StatusSeeOther
			}
			requireNativeStatus(t, response, want)
			for _, path := range []string{"/api/v2/organizations/org_browser_preview/projects/" + project, "/organization/plan", "/organization/billing", "/api/cloud/billing", "/api/cloud/billing/subscription"} {
				requireNativeStatus(t, f.page(t, "owner", path), http.StatusOK)
			}
			page := f.page(t, "owner", "/organization/billing").Body.String()
			if test.grant && !strings.Contains(page, "Complimentary access") {
				t.Fatal("billing hid the independent grant")
			}
			var projects int64
			var complete bool
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				reader, ok := r.(billingstore.PopulationReader)
				if !ok {
					return hostedplans.ErrBillingUsageUnavailable
				}
				var err error
				projects, complete, err = reader.BillingPopulation(t.Context(), "projects", f.service.config.Clock(), time.Minute)
				if err != nil {
					return err
				}
				retained, err := r.Project(t.Context(), tracker.ProjectID(project))
				if err != nil {
					return err
				}
				if retained.DeletedAt != nil {
					return fmt.Errorf("paid project was deleted during downgrade")
				}
				if test.grant {
					grant, err := r.BillingGrant(t.Context(), "complimentary_fixture")
					if err != nil {
						return err
					}
					if grant.RevokedAt != nil || grant.Plan != (billingstore.PlanReference{ID: "test_paid", Version: 1}) || !slices.Equal(grant.Scope, []string{"projects"}) {
						return fmt.Errorf("downgrade changed independent complimentary grant")
					}
				}
				return nil
			}); err != nil || !complete || projects < 3 {
				t.Fatalf("downgrade removed project data: %d complete=%t error=%v", projects, complete, err)
			}
			entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
			if err != nil || entitlement.Source != "base" {
				t.Fatalf("fallback=%+v %v", entitlement, err)
			}
			if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), "operator_fixture", hostedplans.PlanCommand{ID: "override", Action: "subscription", ExpectedRevision: entitlement.Revision, Reason: "Attempted bypass"})); err == nil {
				t.Fatal("manual subscription override bypassed Stripe")
			}
		})
	}
}

func TestHostedBillingPreservesRunningLease(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	plans := hostedTestPlans(t, f.service, nil)
	free := plans.Plans[0]
	free.Version++
	free.Features = slices.DeleteFunc(slices.Clone(free.Features), func(feature string) bool { return feature == "native_execution" })
	plans.Plans = append(plans.Plans, free)
	plans.Base = free.PlanReference
	hosted := *f.service.config.Hosted
	hosted.Plans = &plans
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hosted))); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ApplyPlanCommand(t.Context(), bootstrapTokenID, hostedplans.PlanCommand{ID: "free-base", Action: "base", ExpectedRevision: 1, Plan: plans.Base, Reason: "Free has no execution"})); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.service.config.Hosted.Plans = &plans
	cfg := &hostedplans.HostedBillingConfig{Prices: []hostedplans.HostedBillingPrice{{PriceID: "price_fixture", Plan: plans.Plans[1].PlanReference}}, AccountID: "acct_fixture", CustomerID: "cus_fixture"}
	f.service.config.Hosted.Billing = cfg
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(f.service.config.Hosted))); err != nil {
		t.Fatal(err)
	}
	paid := hostedplans.ResolveBilling(cfg, hostedplans.BillingState{}, activeBillingSnapshot(now), now)
	if err := hubapi.FromFeatureError(f.service.database.plansService().CommitBilling(t.Context(), paid, 0, nil, now)); err != nil {
		t.Fatal(err)
	}
	work := newNativeFixture(t, f.service, f.service.accessOrganization(), "billing-lease-work")
	requests := make([]tracker.NativeClaim, 2)
	tokens := make([]string, 2)
	for i := range 2 {
		issue := work.create(t, fmt.Sprintf("Billing work %d", i))
		tokens[i], requests[i] = hostedPlanWorkRequest(t, work, issue, fmt.Sprintf("billing_session_%d", i))
	}
	lease, err := claimHostedPlanWork(t, work, tokens[0], requests[0])
	if err != nil {
		t.Fatal(err)
	}
	canceled := hostedplans.ResolveBilling(cfg, paid, billing.Snapshot{Status: "canceled"}, now)
	if err := hubapi.FromFeatureError(f.service.database.plansService().CommitBilling(t.Context(), canceled, 0, nil, now)); err != nil {
		t.Fatal(err)
	}
	replay, err := claimHostedPlanWork(t, work, tokens[0], requests[0])
	if err != nil || replay.ID != lease.ID || replay.FencingToken != lease.FencingToken {
		t.Fatalf("billing cancellation blocked owned claim replay: %+v %v", replay, err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(lease.ID)+"/renew", tokens[0], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 60})
	if response.Code != http.StatusOK {
		t.Fatalf("billing cancellation blocked running work: %d %s", response.Code, response.Body.String())
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/claims", tokens[1], requests[1])
	requireNativeCode(t, response, http.StatusTooManyRequests, "allowance_exhausted")
	var refused struct {
		Resource string `json:"resource"`
	}
	decodeHubResponse(t, response, &refused)
	if refused.Resource != "native_execution" {
		t.Fatalf("new dispatch refused for unrelated reason: %s", response.Body.String())
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, work.base+"/leases/"+string(lease.ID)+"/release", tokens[0], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"})
	if response.Code != http.StatusNoContent {
		t.Fatalf("safe release failed: %d %s", response.Code, response.Body.String())
	}
}

func TestHostedBillingCommitIsAtomic(t *testing.T) {
	t.Parallel()
	f, p := newHostedBillingFixture(t)
	requireNativeStatus(t, postBillingEvent(t, f, "evt_atomic", "invoice.paid", true), http.StatusOK)
	admin := hostedPostgresAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_billing_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.organization_id=TG_ARGV[0] AND OLD.event_id=TG_ARGV[1] THEN RAISE EXCEPTION 'fixture acknowledgment failure' USING ERRCODE='23514'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE TRIGGER reject_billing_ack BEFORE UPDATE OF processed_at ON hosted_billing_events FOR EACH ROW EXECUTE FUNCTION reject_billing_ack('org_browser_preview','evt_atomic')`); err != nil {
		t.Fatal(err)
	}
	p.snapshot = activeBillingSnapshot(time.Now())
	var acknowledgment *pgconn.PgError
	if err := f.service.billing.reconcile(t.Context()); !errors.As(err, &acknowledgment) || acknowledgment.Code != "23514" || acknowledgment.Message != "fixture acknowledgment failure" {
		t.Fatalf("failed acknowledgment committed or fault was not reached: %v", err)
	}
	state, err := hubapi.FeatureResult(f.service.database.plansService().ReadBilling(t.Context()))
	if err != nil || state.Status != "free" {
		t.Fatalf("state partially committed: %+v %v", state, err)
	}
	entitlement, err := hubapi.FeatureResult(f.service.database.plansService().PlanUsage(t.Context(), time.Now()))
	if err != nil || entitlement.Source != "base" {
		t.Fatal("entitlement partially committed")
	}
	var audits []billingstore.Audit
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		audits, err = r.BillingAudits(t.Context(), 0, 1)
		if err != nil {
			return err
		}
		event, err := r.BillingEvent(t.Context(), "evt_atomic")
		if err != nil {
			return err
		}
		if event.ProcessedAt != nil {
			return fmt.Errorf("failed acknowledgment marked event processed")
		}
		return nil
	}); err != nil || len(audits) != 0 {
		t.Fatalf("audit partially committed: %d %v", len(audits), err)
	}
}

type blockingHostedBillingProvider struct {
	hostedBillingProvider
	started chan struct{}
}

func (p *blockingHostedBillingProvider) Reconcile(ctx context.Context, _ billing.Binding) (billing.Snapshot, error) {
	close(p.started)
	<-ctx.Done()
	return billing.Snapshot{}, ctx.Err()
}

func TestHostedBillingWorkerShutdown(t *testing.T) {
	t.Parallel()
	f, _ := newHostedBillingFixture(t)
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	p := &blockingHostedBillingProvider{started: make(chan struct{})}
	cfg := f.service.config
	cfg.TenantRepository, cfg.AccessRepository, cfg.BillingRepository = nil, nil, nil
	cfg.Hosted.Billing.Provider = p
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	<-p.started
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.billing.done:
	default:
		t.Fatal("billing reconciliation outlived database shutdown")
	}
}

func TestHostedBillingPageStates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, status, invoice, want string }{
		{"complimentary base", "free", "", "Your existing plan access is complimentary"},
		{"subscribed", "active", "paid", "Subscribed"},
		{"canceled", "canceled", "", "Canceled"},
		{"failed", "past_due", "open", "Payment failed"},
		{"trial", "trialing", "", "Subscription trial"},
		{"paused", "paused", "", "Subscription needs attention"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, p := newHostedBillingFixture(t)
			snapshot := activeBillingSnapshot(time.Now())
			snapshot.Status, snapshot.InvoiceStatus = test.status, test.invoice
			if test.status == "free" || test.status == "canceled" {
				snapshot = billing.Snapshot{Status: test.status}
			}
			if test.status == "trialing" {
				snapshot.TrialEnd = snapshot.PeriodEnd
			}
			p.snapshot = snapshot
			if err := f.service.billing.reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			calls := p.calls
			response := f.page(t, "owner", "/organization/billing")
			requireNativeStatus(t, response, http.StatusOK)
			for _, want := range []string{test.want, "model provider accounts", `aria-label="Billing audit trail"`, `name="csrf"`, "Export billing status"} {
				if !strings.Contains(response.Body.String(), want) {
					t.Errorf("billing page missing %q", want)
				}
			}
			if p.calls != calls {
				t.Fatal("page read called Stripe")
			}
			if test.name == "complimentary base" {
				price := f.service.config.Hosted.Billing.Prices[0]
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					plan, err := w.BillingPlan(t.Context(), billingstore.PlanReference{ID: price.Plan.ID, Version: price.Plan.Version})
					if err != nil {
						return err
					}
					plan.MonthlyUSDCents = new(int64(1200))
					return w.ReplaceBillingPlan(t.Context(), plan)
				}); err != nil {
					t.Fatal(err)
				}
				if label := f.service.hostedPriceLabel(t.Context(), price); label == price.Label {
					t.Fatal("active price lookup did not read the paid plan")
				}
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				if label := f.service.hostedPriceLabel(canceled, price); label != price.Label {
					t.Fatalf("canceled price lookup = %q, want fallback %q", label, price.Label)
				}
			}
			if strings.Contains(response.Body.String(), "card-sentinel") {
				t.Fatal("webhook private data reached the page")
			}
		})
	}
}

func TestHostedBillingRejectsAdminAndSupportOwner(t *testing.T) {
	t.Parallel()
	for _, support := range []bool{false, true} {
		t.Run(strconv.FormatBool(support), func(t *testing.T) {
			t.Parallel()
			f, p := newHostedBillingFixture(t)
			if support {
				identity := f.provider.identity("user_browser_owner", "owner@example.test", "org_browser_provider", "support@example.test")
				token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
				if err != nil {
					t.Fatal(err)
				}
				f.cookies["owner"] = &http.Cookie{Name: hostedCookie, Value: token}
			} else {
				if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_owner", "admin"); err != nil {
					t.Fatal(err)
				}
			}
			requireNativeStatus(t, f.page(t, "owner", "/organization/billing"), http.StatusForbidden)
			requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodGet, "/billing", ""), http.StatusForbidden)
			usage := f.billingAPI(t, "owner", http.MethodGet, "/billing?view=usage", "")
			requireNativeStatus(t, usage, http.StatusOK)
			if !strings.Contains(usage.Body.String(), `"can_checkout":false`) || !strings.Contains(usage.Body.String(), `"can_manage":false`) || !strings.Contains(usage.Body.String(), `"can_buy_credits":false`) || strings.Contains(usage.Body.String(), `"state"`) || strings.Contains(usage.Body.String(), `"recent_audit"`) {
				t.Fatalf("read-only billing usage exposed billing authority: %s", usage.Body.String())
			}
			for _, path := range []string{"/organization/billing/checkout", "/organization/billing/portal"} {
				requireNativeStatus(t, f.form(t, "owner", path, url.Values{"price": {"price_fixture"}}), http.StatusForbidden)
			}
			if p.calls != 0 || len(p.checkouts) != 0 || len(p.portals) != 0 {
				t.Fatal("admin or support owner reached Stripe")
			}
		})
	}
}
