package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthEvaluationRetainsUnobservedCosts(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	at := service.config.now().UTC().Truncate(time.Microsecond).Add(2 * time.Minute)
	fixtures := map[tracker.OrganizationID][]nativeFixture{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		findings := []healthFinding{}
		for _, name := range []string{"First", "Second"} {
			f := newNativeFixture(t, service, org, name)
			fixtures[org] = append(fixtures[org], f)
			issue := f.create(t, "Example work")
			attempt := seedHealthAttemptRecord(t, f, issue, "machine_"+name, "succeeded", tracker.NativeRunData{}, at.Add(-time.Minute), 100)
			admin := healthFixtureDatabase(t, f)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,$3,$4,'example','example-'||n,0,0,0,0,'USD',$4 FROM generate_series(1,10000)n`, org, f.project.ID, attempt, at.Add(-time.Minute).Truncate(time.Hour)); err != nil {
				t.Fatal(err)
			}
			findings = append(findings,
				healthFinding{Fingerprint: "cost:" + string(f.project.ID), Signal: "cost_anomaly", Class: "cost", Subject: healthSubject{Kind: "work_item", ID: string(issue.WorkItemID)}, Projects: []string{string(f.project.ID)}, Severity: "info", Summary: "Previously observed cost condition", NextAction: "Review recorded usage"},
				healthFinding{Fingerprint: "flow:" + string(f.project.ID), Signal: "candidate_starvation", Class: "flow", Subject: healthSubject{Kind: "project", ID: string(f.project.ID)}, Projects: []string{string(f.project.ID)}, Severity: "info", Summary: "Previously observed flow condition", NextAction: "Review recorded flow"})
		}
		if err := service.publishHealthEvaluation(t.Context(), org, at.Add(-time.Microsecond), findings); err != nil {
			t.Fatal(err)
		}
		snapshot, err := service.readHealthSnapshotRecords(t.Context(), org, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Incomplete) != 2 {
			t.Fatalf("organization=%s incomplete signals=%+v", org, snapshot.Incomplete)
		}
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		ctx := (hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: healthEvaluationMaintenanceBytes}).Context(t.Context(), tenant)
		if err := service.publishHealthEvaluation(ctx, org, at, evaluateHealth(at, snapshot), healthEvaluationSource{Baselines: snapshot.BaselineUnavailable, Gaps: snapshot.Incomplete}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := service.config
	cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
	cfg.runMaintenance = waitForMaintenanceFixtureShutdown
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		for {
			worked, err := reopened.advanceHealthEvaluation(t.Context(), org, hubmaintenance.ItemLimit)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				break
			}
		}
		for _, f := range fixtures[org] {
			page, err := reopened.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "all", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 2 || page.LastTickAt == nil || !page.LastTickAt.Equal(at) {
				t.Fatalf("organization=%s project=%s incomplete evaluation=%+v", org, f.project.ID, page)
			}
			for _, finding := range page.Items {
				switch finding.Signal {
				case "cost_anomaly":
					if finding.ResolvedAt != nil || !finding.LastSeenAt.Equal(at.Add(-time.Microsecond)) {
						t.Fatalf("unobserved cost was cleared or refreshed: %+v", finding)
					}
				case "candidate_starvation":
					if finding.ResolvedAt == nil || !finding.ResolvedAt.Equal(at) {
						t.Fatalf("observed flow was not cleared: %+v", finding)
					}
				default:
					t.Fatalf("unexpected finding=%+v", finding)
				}
			}
			available := false
			for _, baseline := range page.BaselineUnavailable {
				available = available || baseline.Signal == "cost_anomaly"
			}
			if !available {
				t.Fatal("missing cost coverage was not recorded")
			}
		}
		next := at.Add(time.Minute)
		if err := reopened.publishHealthEvaluation(t.Context(), org, next, nil); err != nil {
			t.Fatal(err)
		}
		for _, f := range fixtures[org] {
			page, err := reopened.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "open", "", "", 100)
			if err != nil || len(page.Items) != 0 || page.LastTickAt == nil || !page.LastTickAt.Equal(next) {
				t.Fatalf("later complete evaluation retained an old gap: %+v err=%v", page, err)
			}
		}
	}
}
