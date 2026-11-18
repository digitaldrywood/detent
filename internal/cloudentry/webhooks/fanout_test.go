package webhooks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type fanoutRows struct {
	pages    []*platformstore.GitHubFanoutPage
	failures []bool
	stale    bool
	released int
}

func (r *fanoutRows) NextGitHubFanout(_ context.Context, _ time.Time, limit int) (*platformstore.GitHubFanoutPage, error) {
	if limit != 1 {
		return nil, fmt.Errorf("unexpected fanout page limit %d", limit)
	}
	if len(r.pages) == 0 {
		return nil, nil
	}
	return r.pages[0], nil
}

func (r *fanoutRows) AdvanceGitHubFanout(_ context.Context, page *platformstore.GitHubFanoutPage, failed bool, _ time.Time) error {
	if len(r.pages) == 0 || r.pages[0] != page {
		return errors.New("acknowledged a different fanout page")
	}
	r.pages = r.pages[1:]
	r.failures = append(r.failures, failed)
	if r.stale {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (r *fanoutRows) ReleaseGitHubStaging(_ context.Context, p *platformstore.GitHubFanoutPage) error {
	if !p.Staging || p != r.pages[0] {
		return errors.New("released a different staging reservation")
	}
	r.released++
	r.pages = r.pages[1:]
	return nil
}

func (r *fanoutRows) Page(context.Context, string, int) ([]hubmaintenance.Tenant, error) {
	return nil, nil
}

func TestFanoutAdvancesFailedPageAndKeepsServing(t *testing.T) {
	first := hubmaintenance.Tenant{Organization: "org_alpha", Generation: 2}
	second := hubmaintenance.Tenant{Organization: "org_beta", Generation: 3}
	for _, tc := range []struct {
		name  string
		stale bool
	}{{"active selection", false}, {"another host advanced the cursor", true}} {
		t.Run(tc.name, func(t *testing.T) {
			staging := &platformstore.GitHubFanoutPage{Staging: true, GitHubIngress: platformstore.GitHubIngress{DeliveryID: "delivery", Digest: "digest", ObjectKey: "key"}}
			store := &fanoutRows{stale: tc.stale, pages: []*platformstore.GitHubFanoutPage{
				{Targets: []hubmaintenance.Tenant{first}}, {Targets: []hubmaintenance.Tenant{second}}, {}, staging, staging, staging, staging, {Cleanup: true},
			}}
			var calls []hubmaintenance.Tenant
			removals := 0
			deliver := func(ctx context.Context, org hubmaintenance.Tenant, _ *platformstore.GitHubFanoutPage) error {
				if err := hubmaintenance.RequireTenant(ctx, org); err != nil {
					t.Error(err)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("unbounded tenant handoff")
				}
				calls = append(calls, org)
				if org == first {
					panic("private example payload")
				}
				return nil
			}
			remove := func(ctx context.Context, _, _, _ string) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("unbounded payload removal")
				}
				removals++
				return nil
			}
			verified := 0
			missing := errors.New("staged webhook payload unavailable")
			read := func(ctx context.Context, delivery, digest, key string) ([]byte, error) {
				if _, ok := ctx.Deadline(); !ok || delivery != staging.DeliveryID || digest != staging.Digest || key != staging.ObjectKey {
					t.Error("staging read has invalid receipt or no deadline")
				}
				verified++
				switch verified {
				case 1:
					return nil, missing
				case 2:
					return nil, artifact.ErrMissing
				case 3:
					return nil, artifact.ErrIntegrity
				}
				return []byte("verified"), nil
			}
			feature := FanoutFeature(store, time.Now, deliver, read, remove)
			if _, err := feature.Due(t.Context(), hubmaintenance.TenantLimit); err != nil {
				t.Fatal(err)
			}
			for _, attempt := range []struct {
				tenant hubmaintenance.Tenant
				bytes  int64
				want   error
			}{{second, feature.Bytes, corestore.ErrTenantMismatch}, {first, feature.Bytes - 1, hubmaintenance.ErrByteBudget}} {
				b := hubmaintenance.Budget{Items: 1, Bytes: attempt.bytes}
				if _, err := feature.Step(b.Context(t.Context(), attempt.tenant), attempt.tenant, b); !errors.Is(err, attempt.want) {
					t.Fatalf("invalid handoff returned %v, want %v", err, attempt.want)
				}
			}
			idle := hubmaintenance.Feature{Name: "idle", Bytes: 1, Step: func(context.Context, hubmaintenance.Tenant, hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
				return hubmaintenance.Outcome{}, nil
			}}
			tick := 0
			preceding := hubmaintenance.Feature{Name: "preceding", Lead: true, Bytes: 1, Step: idle.Step, Due: func(ctx context.Context, _ int) ([]hubmaintenance.Tenant, error) {
				if tick == 3 {
					return nil, hubmaintenance.ReserveDueBytes(ctx, hubmaintenance.ByteLimit-feature.Bytes+1)
				}
				return nil, nil
			}}
			scheduler, err := hubmaintenance.NewShared(store, []hubmaintenance.Feature{preceding, feature, idle})
			if err != nil {
				t.Fatal(err)
			}
			for tick = range 10 {
				err := scheduler.Tick(t.Context())
				if tick == 0 && !tc.stale {
					if err == nil || !strings.Contains(err.Error(), "org_alpha") || strings.Contains(err.Error(), "private example payload") {
						t.Fatalf("unsanitized or missing failure: %v", err)
					}
				} else if tick == 4 && !tc.stale {
					if !errors.Is(err, missing) {
						t.Fatalf("missing staged payload: %v", err)
					}
				} else if err != nil {
					t.Fatalf("tick %d: %v", tick, err)
				}
				continued := tick == 5 || tick == 6 || !tc.stale && (tick == 1 || tick == 2 || tick == 7 || tick == 8)
				if feature.Continue() != continued {
					t.Fatalf("tick %d continued=%t want=%t", tick, feature.Continue(), continued)
				}
				if tick == 3 && (verified != 0 || len(store.failures) != 3) {
					t.Fatal("staging advanced without room to verify the payload")
				}
			}
			if !slices.Equal(calls, []hubmaintenance.Tenant{first, second}) || verified != 4 || store.released != 2 || removals != 1 || !slices.Equal(store.failures, []bool{true, false, false, true, false, false}) {
				t.Fatalf("calls %v, verifications %d, removals %d, failed pages %v", calls, verified, removals, store.failures)
			}
		})
	}
}
