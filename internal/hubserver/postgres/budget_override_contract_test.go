package postgres_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBudgetOverrideContractScopesCapsExpiryAndOriginalLifetime(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	policy := postgres.BudgetOverridePolicy{SettingsRevision: 1, Enabled: true, BasePerDayMaxUSD: 100, BasePerIssueMaxUSD: 10, MaxDuration: 48 * time.Hour, MaxMultiplier: 3, Now: now}
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := seedCoreOrganization(t.Context(), w, org); err != nil {
				return err
			}
			for _, id := range []tracker.ProjectID{"first", "second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []tracker.ProjectID{"first", "second"} {
			scopedPolicy := policy
			scopedPolicy.ProjectID = id
			if err := repository.WriteBudgetOverrides(t.Context(), org, corestore.Serving, func(w postgres.BudgetOverrideWriter) error {
				row, err := w.SetBudgetOverride(t.Context(), postgres.BudgetOverride{OrganizationID: org, ProjectID: id, PerDayMaxUSD: new(float64(150 + 50*index)), PerIssueMaxUSD: new(20.0), ExpiresAt: now.Add(48 * time.Hour), Reason: "example"}, scopedPolicy)
				if err == nil && (row.Revision != 1 || !row.CreatedAt.Equal(now)) {
					t.Fatalf("override=%+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	base := postgres.BudgetOverride{OrganizationID: "org_first", ProjectID: "first", PerDayMaxUSD: new(150.0), ExpiresAt: now.Add(time.Hour), Reason: "example", Revision: 1}
	policy.ProjectID = "first"
	for _, test := range []struct {
		name   string
		mutate func(*postgres.BudgetOverride, *postgres.BudgetOverridePolicy)
		want   error
	}{
		{"foreign tenant", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { v.OrganizationID = "org_second" }, corestore.ErrTenantMismatch},
		{"disabled", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { p.Enabled = false }, postgres.ErrBudgetOverride},
		{"cap too high", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { v.PerDayMaxUSD = new(301.0) }, postgres.ErrBudgetOverride},
		{"non-finite cap", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { v.PerDayMaxUSD = new(math.NaN()) }, postgres.ErrBudgetOverride},
		{"missing reason", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { v.Reason = " " }, postgres.ErrBudgetOverride},
		{"stale policy", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { p.SettingsRevision = 9 }, corestore.ErrRevisionConflict},
		{"stale override", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) { v.Revision = 9 }, corestore.ErrRevisionConflict},
		{"extended lifetime", func(v *postgres.BudgetOverride, p *postgres.BudgetOverridePolicy) {
			p.Now = now.Add(47 * time.Hour)
			v.ExpiresAt = now.Add(49 * time.Hour)
		}, postgres.ErrBudgetOverride},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, scopedPolicy := base, policy
			test.mutate(&value, &scopedPolicy)
			err := repository.WriteBudgetOverrides(t.Context(), "org_first", corestore.Serving, func(w postgres.BudgetOverrideWriter) error {
				_, err := w.SetBudgetOverride(t.Context(), value, scopedPolicy)
				return err
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
	if err := repository.WriteBudgetOverrides(t.Context(), "org_first", corestore.Serving, func(w postgres.BudgetOverrideWriter) error {
		commands := w.(postgres.BudgetOverrideCommandWriter)
		if _, err := commands.CompareAndSetBudgetOverride(t.Context(), base, policy, 0); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("absent-revision replacement error=%v", err)
		}
		unchanged := base
		unchanged.ExpiresAt = now.Add(48 * time.Hour)
		changed, err := commands.CompareAndSetBudgetOverride(t.Context(), unchanged, policy, 1)
		if err != nil {
			return err
		}
		if changed.Revision != 2 || !changed.CreatedAt.Equal(now) {
			t.Fatalf("revision update=%+v", changed)
		}
		if _, err := commands.CompareAndSetBudgetOverride(t.Context(), base, policy, 1); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("concurrent replacement error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = repositoryFor(t, admin, smallPool())
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadBudgetOverrides(t.Context(), org, corestore.Serving, func(r postgres.BudgetOverrideReader) error {
			rows, err := r.ListActiveBudgetOverrides(t.Context(), now, "", 2)
			if err != nil {
				return err
			}
			if len(rows) != 2 {
				t.Fatalf("rows=%+v", rows)
			}
			for _, row := range rows {
				if row.OrganizationID != org || row.PerDayMaxUSD == nil || *row.PerDayMaxUSD != float64(150+50*index) {
					t.Fatalf("override escaped scope=%+v", row)
				}
			}
			for _, at := range []time.Time{now.Add(48*time.Hour - time.Microsecond), now.Add(48 * time.Hour)} {
				_, err := r.ActiveBudgetOverride(t.Context(), "first", at)
				if at.Equal(now.Add(48 * time.Hour)) {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("expiry boundary=%v", err)
					}
				} else if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteBudgetOverrides(t.Context(), org, corestore.Serving, func(w postgres.BudgetOverrideWriter) error { return w.ClearBudgetOverride(t.Context(), "second") }); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteBudgetOverrides(t.Context(), org, corestore.Serving, func(w postgres.BudgetOverrideWriter) error { return w.ClearBudgetOverride(t.Context(), "second") }); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("missing clear=%v", err)
		}
	}
}
