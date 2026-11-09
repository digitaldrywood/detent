package hubserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthEvaluationMaintenanceResumesBeforeTick(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	now := service.config.now().UTC().Truncate(time.Microsecond)
	fixtures := map[tracker.OrganizationID][]nativeFixture{}
	positions := map[tracker.OrganizationID]map[string]int{}
	read := func(t *testing.T, org tracker.OrganizationID, want int, tick *time.Time) {
		t.Helper()
		total := 0
		for _, f := range fixtures[org] {
			page, err := service.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "open", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			total += len(page.Items)
			if tick == nil && page.LastTickAt != nil || tick != nil && (page.LastTickAt == nil || !page.LastTickAt.Equal(*tick)) {
				t.Fatalf("organization=%s completed tick=%v want=%v", org, page.LastTickAt, tick)
			}
			for _, finding := range page.Items {
				if !finding.OpenedAt.Equal(now.Add(-time.Hour)) {
					t.Fatalf("opening time lost across checkpoint: %+v", finding)
				}
			}
		}
		if total != want {
			t.Fatalf("organization=%s findings=%d want=%d", org, total, want)
		}
	}
	work := func(t *testing.T, org tracker.OrganizationID) (healthstore.Evaluation, error) {
		t.Helper()
		var value healthstore.Evaluation
		err := service.database.postgres.ReadHealth(t.Context(), org, corestore.Background, func(r postgres.HealthReader) error {
			owner, ok := r.(healthstore.EvaluationReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			var err error
			value, err = owner.HealthEvaluation(t.Context())
			return err
		})
		return value, err
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		positions[org] = map[string]int{}
		for _, name := range []string{"First", "Second"} {
			fixtures[org] = append(fixtures[org], newNativeFixture(t, service, org, name))
		}
		findings := make([]healthFinding, 0, 66)
		baselines := []healthBaselineUnavailable{}
		for index, f := range fixtures[org] {
			baselines = append(baselines, healthBaselineUnavailable{Project: string(f.project.ID), Signal: "queue_growth", Lane: "Todo"})
			if err := service.database.postgres.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				return w.PutHealthBaseline(t.Context(), postgres.PutHealthBaselineInput{OrganizationID: org, ProjectID: f.project.ID, Signal: "retired_baseline", Lane: "Todo"})
			}); err != nil {
				t.Fatal(err)
			}
			for i := range 33 {
				position := index*33 + i
				positions[org][fmt.Sprintf("condition_%02d", position)] = position
				findings = append(findings, healthFinding{Fingerprint: fmt.Sprintf("condition_%02d", position), Signal: "candidate_starvation", Class: "flow", Subject: healthSubject{Kind: "project", ID: string(f.project.ID)}, Projects: []string{string(f.project.ID)}, ConditionSince: now.Add(-time.Hour), Severity: "info", Summary: "Example condition", NextAction: "Inspect the recorded condition", Evidence: healthEvidence{Counts: map[string]int{"position": position}}})
			}
		}
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		allocated := func() context.Context {
			return (hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: healthEvaluationMaintenanceBytes}).Context(t.Context(), tenant)
		}
		if err := service.publishHealthEvaluation(allocated(), org, now, findings, healthEvaluationSource{Baselines: baselines}); err != nil {
			t.Fatal(err)
		}
		read(t, org, 0, nil)
		if progressed, err := service.advanceHealthEvaluation(allocated(), org, 1); err != nil || !progressed {
			t.Fatalf("initial progress=%t err=%v", progressed, err)
		}
		read(t, org, 1, nil)
		pending, err := work(t, org)
		if err != nil || pending.Position != 1 || pending.Phase != "publish" {
			t.Fatalf("initial checkpoint=%+v err=%v", pending, err)
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
	service = reopened
	t.Cleanup(func() { _ = reopened.Close() })
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: 1}
		allocated := func() context.Context {
			return (hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: healthEvaluationMaintenanceBytes}).Context(t.Context(), tenant)
		}
		pending, err := work(t, org)
		if err != nil || pending.Position != 1 || pending.Phase != "publish" {
			t.Fatalf("reopened checkpoint=%+v err=%v", pending, err)
		}
		if progressed, err := service.advanceHealthEvaluation(allocated(), org, hubmaintenance.ItemLimit); err != nil || !progressed {
			t.Fatalf("bounded progress=%t err=%v", progressed, err)
		}
		read(t, org, 65, nil)
		pending, err = work(t, org)
		if err != nil || pending.Position != 65 || pending.Phase != "publish" {
			t.Fatalf("bounded checkpoint=%+v err=%v", pending, err)
		}
		for {
			progressed, err := service.advanceHealthEvaluation(allocated(), org, hubmaintenance.ItemLimit)
			if err != nil {
				t.Fatal(err)
			}
			if !progressed {
				break
			}
		}
		read(t, org, 66, &now)
		for _, f := range fixtures[org] {
			page, err := service.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "open", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 33 || len(page.BaselineUnavailable) != 1 || page.BaselineUnavailable[0].Signal != "queue_growth" || page.BaselineUnavailable[0].Project != string(f.project.ID) {
				t.Fatalf("organization=%s project=%s final evaluation=%+v", org, f.project.ID, page)
			}
			for _, finding := range page.Items {
				expected, present := positions[org][finding.Fingerprint]
				if !present || finding.Evidence.Counts["position"] != expected {
					t.Fatalf("evidence lost across checkpoint: %+v", finding)
				}
			}
		}
		next := now.Add(time.Minute)
		if err := service.publishHealthEvaluation(t.Context(), org, next, nil); err != nil {
			t.Fatal(err)
		}
		read(t, org, 0, &next)
		for _, f := range fixtures[org] {
			page, err := service.readHealthFindings(t.Context(), nativeScope{organization: org, project: f.project.ID}, "all", "", "", 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 33 || len(page.BaselineUnavailable) != 0 {
				t.Fatalf("resolved evaluation=%+v", page)
			}
			for _, finding := range page.Items {
				if finding.ResolvedAt == nil || !finding.ResolvedAt.Equal(next) {
					t.Fatalf("resolution lost: %+v", finding)
				}
			}
		}
	}
}
