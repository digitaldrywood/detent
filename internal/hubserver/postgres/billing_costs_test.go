package postgres_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestBillingCostsCorrectMonthlySpendWithoutDoubleCounting(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		for _, project := range []tracker.ProjectID{"project_first", "project_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		amount := int64(101)
		base := billingstore.CostObservation{OrganizationID: org, ProjectID: "project_first", ReceivedAt: boundary.Add(time.Hour + time.Nanosecond), Observation: usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: "resource", SourceID: "source", Bucket: "runner_api", Metric: "tokens", Currency: "USD", Unit: "tokens", Revision: 1, From: boundary.Add(-time.Nanosecond), To: boundary.Add(time.Nanosecond), AmountMicros: &amount}}
		historyImport := false
		write := func(record billingstore.CostObservation, applied bool, wantErr error, rollback bool) {
			t.Helper()
			raw, err := json.Marshal(record.Observation)
			if err != nil {
				t.Fatal(err)
			}
			archive, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			record.Reference = billingstore.AuditReferenceFromArchive(archive)
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var got bool
				var err error
				if historyImport {
					got, err = w.ImportBillingCostRevision(t.Context(), record)
				} else {
					got, err = w.RecordBillingCost(t.Context(), record)
				}
				if got != applied {
					t.Errorf("applied = %v, want %v", got, applied)
				}
				if err == nil && rollback {
					return mutation.ErrConflict
				}
				return err
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("write error = %v, want %v", err, wantErr)
			}
		}
		assertSpend := func(project tracker.ProjectID, january, february int64) {
			t.Helper()
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, period := range []struct {
					at   time.Time
					want int64
				}{{boundary.Add(-time.Nanosecond), january}, {boundary, february}} {
					spend, err := r.BillingMonthlySpend(t.Context(), project, period.at)
					if err != nil {
						return err
					}
					if spend.RunnerMicros != period.want || spend.LunaMicros != 0 || spend.SpriteMicros != 0 {
						t.Errorf("project %s month %v spend=%+v want runner=%d", project, period.at, spend, period.want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		write(base, true, nil, false)
		write(base, false, nil, false)
		assertSpend("project_first", 50, 51)
		assertSpend("project_second", 0, 0)
		changed := base
		otherAmount := int64(103)
		changed.Observation.AmountMicros = &otherAmount
		write(changed, false, mutation.ErrConflict, false)
		for _, mutate := range []func(*usagecost.Observation){
			func(o *usagecost.Observation) { o.RunnerID = "different_runner" },
			func(o *usagecost.Observation) { o.MachineID = "different_machine" },
			func(o *usagecost.Observation) { o.Placement = "sprite" },
		} {
			reattributed := base
			reattributed.Observation.Revision = 2
			mutate(&reattributed.Observation)
			write(reattributed, false, mutation.ErrConflict, false)
		}
		changed.Observation.Revision = 2
		write(changed, true, mutation.ErrConflict, true)
		assertSpend("", 50, 51)
		write(changed, true, nil, false)
		write(base, false, nil, false)
		assertSpend("", 51, 52)
		overlap := base
		overlap.Observation.SourceID = "overlapping"
		write(overlap, false, mutation.ErrConflict, false)
		other := base
		other.ProjectID = "project_second"
		other.Observation.ResourceID = "second_resource"
		other.Observation.SourceID = "second_source"
		write(other, true, nil, false)
		assertSpend("project_second", 50, 51)
		assertSpend("", 101, 103)
		changed.Observation.Revision = 3
		changed.Observation.Voided = true
		write(changed, true, nil, false)
		assertSpend("project_first", 0, 0)
		assertSpend("", 50, 51)
		historical := changed
		historical.Observation.SourceID = "history_source"
		historical.Observation.ResourceID = other.Observation.ResourceID
		write(historical, true, nil, false)
		historyImport = true
		historical.Observation.Revision = 1
		historical.Observation.Voided = false
		write(historical, true, nil, false)
		write(historical, false, nil, false)
		historical.Observation.Revision = 4
		write(historical, false, mutation.ErrConflict, false)
		assertSpend("", 50, 51)
		historyImport = false
		write(historical, false, mutation.ErrConflict, false)
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			latest, err := r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: base.Observation.Provider, ProviderAccount: base.Observation.ProviderAccount, SourceID: base.Observation.SourceID, From: base.Observation.From, To: base.Observation.To})
			if err != nil {
				return err
			}
			if !latest.ReceivedAt.Equal(base.ReceivedAt) || latest.Revision != 3 || latest.ProjectID != "project_first" || latest.Reference.ArchiveReference().Validate(string(org)) != nil {
				t.Errorf("latest revision lost correction or tenant evidence: %+v", latest)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
