package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestHostedBillingResultLimits(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"shared", "dedicated"} {
		for _, test := range []struct {
			name      string
			model     string
			oversized bool
		}{
			{name: "normal", model: "example-model"},
			{name: "oversized", model: strings.Repeat("x", 300<<10), oversized: true},
		} {
			t.Run(deployment+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				f, ctx := newHostedKeyMCPFixture(t, deployment, "owner")
				now := f.service.config.now().UTC()
				operatorSQL(t, f.hostedSecurityFixture, `INSERT INTO attempt_usage
 (attempt_id, organization_id, project_id, period, provider, model, input, currency, updated_at)
 VALUES ('attempt_usage_fixture', ?, ?, ?, 'example-provider', ?, 100, 'USD', ?)`,
					f.service.config.Hosted.OrganizationID, f.project, usagePeriod(now), test.model, formatHubTime(now))
				result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{
					Name: operatortool.HostedUsage, Arguments: json.RawMessage(`{"range":"24h"}`),
				})
				if test.oversized {
					var sizeError *operatortool.ResultTooLargeError
					if !errors.Is(err, operatortool.ErrResultTooLarge) || !errors.As(err, &sizeError) {
						t.Fatalf("oversized billing report error = %v, want typed size error", err)
					}
					if sizeError.SizeBytes <= 300<<10 || sizeError.LimitBytes != operatortool.MaxResultBytes || len(result.Content) != 0 {
						t.Fatalf("oversized billing report size = %+v, content bytes = %d", sizeError, len(result.Content))
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					Report usageReport `json:"report"`
				}
				if err := json.Unmarshal(result.Content, &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Report.Breakdown.ByModel) != 1 || response.Report.Breakdown.ByModel[0].Model != test.model {
					t.Fatal("billing report lost the stored model")
				}
			})
		}
	}
}

// Provider response loss is resumable only through the application's existing
// checkout intent/key; concurrent callers must not invent a new purchase.
func TestBillingCommandResponseLoss(t *testing.T) {
	t.Parallel()
	f, provider := newHostedCustomerFixture(t)
	loss := &billingResponseLossProvider{hostedCustomerProvider: provider, sessions: make(map[string]billing.Session)}
	f.service.config.Hosted.Billing.Provider = loss
	var credential apiCredential
	f.service.echo.GET("/billing-command-fixture", func(c echo.Context) error {
		var err error
		credential, err = f.service.hostedBillingOwner(c.Request().Context(), c)
		return err
	})
	requireNativeStatus(t, f.page(t, "owner", "/billing-command-fixture"), http.StatusOK)
	authorize := func(context.Context) (apiCredential, error) { return credential, nil }
	if _, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response"); err == nil {
		t.Fatal("fixture did not lose response")
	}
	first := loss.keys[0]
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			result, err := f.service.checkoutBilling(t.Context(), authorize, "price_fixture", "lost-response")
			if err != nil || result.ID != "cs_test_fixture" {
				t.Errorf("retry=%+v %v", result, err)
			}
		})
	}
	wg.Wait()
	if len(provider.checkouts) != 1 || len(loss.keys) != 2 || loss.keys[1] != first {
		t.Fatal("response loss replaced provider key")
	}
}

type billingResponseLossProvider struct {
	*hostedCustomerProvider
	sessions map[string]billing.Session
	keys     []string
}

func (p *billingResponseLossProvider) Checkout(ctx context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	p.keys = append(p.keys, request.IdempotencyKey)
	if session, ok := p.sessions[request.IdempotencyKey]; ok {
		return session, nil
	}
	session, err := p.hostedCustomerProvider.Checkout(ctx, request)
	if err != nil {
		return session, err
	}
	p.sessions[request.IdempotencyKey] = session
	return billing.Session{}, errors.New("credential-sensitive-value-sentinel: response lost after provider effect")
}

func TestBillingStatusCreditCursor(t *testing.T) {
	t.Parallel()
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "owner")
			d := f.service.database
			d.aiCreditMode = billing.ModeTest
			if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'test',123)`, d.hostedOrganization); err != nil {
				t.Fatal(err)
			}
			for index := range 3 {
				if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,123,'purchase',?)`, d.hostedOrganization, fmt.Sprintf("purchase-%d", index), int64(index+1)); err != nil {
					t.Fatal(err)
				}
			}
			cursor := ""
			for index := range 3 {
				args, err := json.Marshal(creditPageRequest{Limit: 1, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.BillingStatus, Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				var read struct {
					Billing hostedBillingView `json:"billing"`
				}
				if err := json.Unmarshal(result.Content, &read); err != nil {
					t.Fatal(err)
				}
				credits := read.Billing.AICredits
				if credits == nil || credits.TotalRows != 3 || len(credits.History) != 1 || credits.History[0].At.UnixMicro() != int64(3-index) {
					t.Fatalf("billing=%+v", read)
				}
				if (credits.NextCursor == "") != (index == 2) {
					t.Fatalf("cursor=%s index=%d", credits.NextCursor, index)
				}
				cursor = credits.NextCursor
			}
		})
	}
}
