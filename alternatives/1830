package credits

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func fixtureService(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, details attachment.DetailStore) *Service {
	t.Helper()
	return New(Deps{
		Repository: repository, Organization: organization, Mode: billing.ModeTest, Now: time.Now, CostMultiplier: 1.5, ReadIncluded: true, Details: details,
		Plan: func(ctx context.Context, r billingstore.Reader, ref billingstore.PlanReference) (Plan, error) {
			row, err := r.BillingPlan(ctx, ref)
			if err != nil {
				return Plan{}, err
			}
			if row.MonthlyUSDCents == nil {
				return Plan{Name: row.Name}, nil
			}
			amount := max(0, *row.MonthlyUSDCents) * 1000
			return Plan{Name: row.Name, AllowanceMicros: &amount}, nil
		},
		SafeID: func(value string) bool {
			return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
				return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
			}) == -1
		},
		ArchiveReason: func(ctx context.Context, reason string) (billingstore.AuditReference, error) {
			raw, err := json.Marshal(struct {
				Reason string `json:"reason"`
			}{Reason: reason})
			if err != nil {
				return billingstore.AuditReference{}, err
			}
			archive, err := details.StoreDetail(ctx, string(organization), raw)
			return billingstore.AuditReferenceFromArchive(archive), err
		},
		ReadReason: func(ctx context.Context, ref billingstore.AuditReference) (string, error) {
			if ref.Key == "" {
				return "", nil
			}
			raw, err := details.ReadDetail(ctx, string(organization), ref.ArchiveReference())
			if err != nil {
				return "", err
			}
			var detail struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(raw, &detail); err != nil {
				return "", err
			}
			return detail.Reason, nil
		},
		ValidateDetail: func(ctx context.Context, store attachment.DetailStore, org tracker.OrganizationID, ref attachment.ArchiveReference, expected []byte) error {
			if ref.Validate(string(org)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(expected)) {
				return artifact.ErrIntegrity
			}
			actual, err := store.ReadDetail(ctx, string(org), ref)
			if err != nil {
				return errors.Join(artifact.ErrIntegrity, err)
			}
			if !bytes.Equal(actual, expected) {
				return artifact.ErrIntegrity
			}
			return nil
		},
	})
}

func creditFixture(t *testing.T, repository tenantstore.Repository, balance int64) *Service {
	t.Helper()
	organizations := [2]tracker.OrganizationID{}
	for i := range organizations {
		organizations[i] = hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
		if err := repository.WriteTenant(t.Context(), organizations[i], corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organizations[i], Mode: billing.ModeTest, BalanceMicros: balance})
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if err := repository.ReadTenant(ctx, organizations[1], corestore.Serving, func(r tenantstore.Reader) error {
			account, err := r.BillingCreditAccount(ctx, billing.ModeTest)
			if err == nil && account.BalanceMicros != balance {
				t.Errorf("other organization balance=%d want=%d", account.BalanceMicros, balance)
			}
			return err
		}); err != nil {
			t.Error(err)
		}
	})
	return fixtureService(t, repository, organizations[0], hubtest.NewDetails())
}

type failingDetails struct {
	*hubtest.Details
	fail       bool
	beforeRead func(context.Context) error
}

func (d *failingDetails) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	if d.fail {
		return attachment.ArchiveReference{}, errors.New("archive unavailable")
	}
	return d.Details.StoreDetail(ctx, org, raw)
}
func (d *failingDetails) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	if d.beforeRead != nil {
		if err := d.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	return d.Details.ReadDetail(ctx, org, ref)
}
