package healthprojection_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestCurrentWaitProvenanceSurvivesTenantPublication(t *testing.T) {
	hub := hubfixture.Open(t, hubfixture.Config{})
	projection := healthprojection.Service{Postgres: hub.Repository, Tenants: hub.Repository, Details: hub.Config.RunDetails, Clock: hub.Config.Clock}
	at := hub.Now().UTC().Truncate(time.Microsecond)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f := hubfixture.NewNative(t, hub, organization, "Example wait authority")
		linked := f.Create(t, "Example question")
		contract := f.Create(t, "Example required contract")
		for index, test := range []struct {
			name string
			wait healthsignals.Wait
			kind string
			id   string
		}{
			{name: "linked human question", wait: healthsignals.Wait{Item: string(linked.WorkItemID), Project: string(f.Project.ID), Since: at.Add(-time.Hour), Active: true}, kind: "work_item", id: string(linked.WorkItemID)},
			{name: "unlinked project question", wait: healthsignals.Wait{Project: string(f.Project.ID), Since: at.Add(-time.Hour), Active: true}, kind: "project", id: string(f.Project.ID)},
			{name: "current contract requirement", wait: healthsignals.Wait{Item: string(contract.WorkItemID), Project: string(f.Project.ID), Since: at, Active: true, Reason: "Complete required issue sections"}, kind: "work_item", id: string(contract.WorkItemID)},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				observed := at.Add(time.Duration(2*index) * time.Second)
				finding, active := healthsignals.HumanWaitFinding(observed, test.wait)
				if !active {
					t.Fatal("current wait was not classified")
				}
				scope := healthprojection.Scope{Projects: map[tracker.ProjectID]bool{f.Project.ID: true}}
				if err := projection.Publish(t.Context(), organization, observed, []healthsignals.Finding{finding}, scope); err != nil {
					t.Fatal(err)
				}
				page, err := projection.Read(t.Context(), hubscope.NativeScope{Organization: organization, Project: f.Project.ID}, "open", "", "", 10)
				if err != nil || len(page.Items) != 1 {
					t.Fatalf("current tenant wait=%+v error=%v", page, err)
				}
				row := page.Items[0]
				if row.Signal != "unanswered_human_wait" || row.Subject.Kind != test.kind || row.Subject.ID != test.id || !row.OpenedAt.Equal(test.wait.Since) || !row.LastSeenAt.Equal(observed) {
					t.Fatalf("wait provenance changed: %+v want kind=%s id=%s since=%s", row, test.kind, test.id, test.wait.Since)
				}
				if err := hub.Repository.ReadHealth(t.Context(), organization, corestore.Serving, func(reader postgres.HealthReader) error {
					links, err := reader.HealthRecordProjects(t.Context(), postgres.HealthRecordProjectsInput{OrganizationID: organization, FindingID: row.ID, Limit: 2})
					if err == nil && (len(links) != 1 || links[0].ProjectID != f.Project.ID) {
						t.Fatalf("wait project scope changed: %+v", links)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if test.wait.Reason != "" && row.Evidence.Reason != "issue_contract_unsatisfied" {
					t.Fatalf("contract requirement reason changed: %+v", row.Evidence)
				}
				if err := projection.Publish(t.Context(), organization, observed.Add(time.Second), nil, scope); err != nil {
					t.Fatal(err)
				}
				cleared, err := projection.Read(t.Context(), hubscope.NativeScope{Organization: organization, Project: f.Project.ID}, "open", "", "", 10)
				if err != nil || len(cleared.Items) != 0 {
					t.Fatalf("resolved wait remained current: %+v error=%v", cleared, err)
				}
			})
		}
	}
}

func TestFingerprintScopePreservesUnexaminedHistory(t *testing.T) {
	hub := hubfixture.Open(t, hubfixture.Config{})
	projection := healthprojection.Service{Postgres: hub.Repository, Tenants: hub.Repository, Details: hub.Config.RunDetails, Clock: hub.Config.Clock}
	now := hub.Now().UTC().Truncate(time.Microsecond)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		first := hubfixture.NewNative(t, hub, organization, "Example examined health")
		second := hubfixture.NewNative(t, hub, organization, "Example unexamined health")
		known := healthsignals.NewFinding("examined_source", "flow", "project", string(first.Project.ID), "Example observed condition", "Inspect", []string{string(first.Project.ID)}, healthsignals.Evidence{})
		unknown := healthsignals.NewFinding("unknown_source", "flow", "project", string(first.Project.ID), "Example unknown condition", "Inspect", []string{string(first.Project.ID)}, healthsignals.Evidence{})
		other := healthsignals.NewFinding("other_source", "flow", "project", string(second.Project.ID), "Example other project", "Inspect", []string{string(second.Project.ID)}, healthsignals.Evidence{})
		if err := projection.Publish(t.Context(), organization, now, []healthsignals.Finding{known, unknown, other}, healthprojection.Scope{Complete: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes) SELECT $1,'unexamined_'||n,'unexamined_'||n,'other_source','flow','project',$2,$3,$3,'watch','Example unexamined history','Inspect','','',0,0 FROM generate_series(1,10001) n`, organization, second.Project.ID, now); err != nil {
			t.Fatal(err)
		}
		context := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}.Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		scope := healthprojection.Scope{Projects: map[tracker.ProjectID]bool{first.Project.ID: true}, Fingerprints: map[string]bool{known.Fingerprint: true}}
		if err := projection.Publish(context, organization, now.Add(time.Second), nil, scope); err != nil {
			t.Fatal(err)
		}
		if err := hub.Repository.ReadHealth(t.Context(), organization, corestore.Background, func(reader postgres.HealthReader) error {
			for _, test := range []struct {
				finding  healthsignals.Finding
				resolved bool
			}{{known, true}, {unknown, false}, {other, false}} {
				row, err := reader.RecentHealthFingerprint(t.Context(), postgres.RecentHealthFingerprintInput{OrganizationID: organization, Fingerprint: test.finding.Fingerprint, ResolvedAt: new(now.Add(-healthsignals.ReopenWindow))})
				if err != nil {
					return err
				}
				if (row.ResolvedAt != nil) != test.resolved {
					t.Fatalf("fingerprint scope changed unexamined condition: signal=%s row=%+v", test.finding.Signal, row)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
