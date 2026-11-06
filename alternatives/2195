package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMonthlyCostsTenantScope(t *testing.T) {
	repo := projectRecordPostgres(t)
	objects := &memoryRunDetails{}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		observation := costTestObservation("cpu", from, from.Add(time.Hour))
		if err := normalizeCostObservation(&observation, from.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(observation)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := objects.StoreDetail(t.Context(), string(org), raw)
		if err != nil {
			t.Fatal(err)
		}
		err = repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: from}); err != nil {
				return err
			}
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}); err != nil {
				return err
			}
			_, err := w.RecordBillingCost(t.Context(), billingstore.CostObservation{OrganizationID: org, ProjectID: "prj_example", Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: from.Add(time.Hour)})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	d := &database{billingRepository: repo, billingAuditStore: objects}
	for _, org := range []string{"org_first", "org_second"} {
		report, err := d.monthlyCosts(t.Context(), org, "organization", nil, usageWindow{From: from, To: from.AddDate(0, 1, 0)}, from.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Sources) != 1 || report.OrganizationID != org || report.Sources[0].AmountMicros == nil || *report.Sources[0].AmountMicros != 20000 {
			t.Fatalf("report=%+v", report)
		}
		empty, err := d.monthlyCosts(t.Context(), org, "project", []string{"prj_other"}, usageWindow{From: from, To: from.AddDate(0, 1, 0)}, from.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(empty.Sources) != 0 {
			t.Fatal("foreign project costs leaked")
		}
	}
}
