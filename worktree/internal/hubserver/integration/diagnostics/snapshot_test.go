package diagnostics_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const secondProject tracker.ProjectID = "prj_second"

const payloadDigest = "0000000000000000000000000000000000000000000000000000000000000001"

func seedItem(t *testing.T, f tenant, project tracker.ProjectID, item tracker.NativeWorkItemID, state string, at time.Time) {
	t.Helper()
	if err := f.repository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		number, err := w.NextWorkItemNumber(t.Context(), project)
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.organization, ProjectID: project, NativeID: item, Number: number, Title: "Diagnostic item", WorkflowStateName: &state, NativeCreatedAt: at, NativeUpdatedAt: at, CreatedAt: at, UpdatedAt: at})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedSQL(t *testing.T, f tenant, statement string, args ...any) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("%v: %s", err, statement)
	}
}

func seedEvent(t *testing.T, f tenant, project tracker.ProjectID, item tracker.NativeWorkItemID, id string, sequence int, kind string, at time.Time) {
	t.Helper()
	seedSQL(t, f, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES($1,$2,$3,$4,$5,$6,1,CASE WHEN $6='workflow.transitioned' THEN 'Todo' ELSE '' END,CASE WHEN $6='workflow.transitioned' THEN 'Done' ELSE '' END,$7,'orgs/'||$1||'/blobs/sha256/00/'||$8,$8,1,1)`, f.organization, project, item, id, sequence, kind, at, payloadDigest)
}

func seedLanding(t *testing.T, f tenant, project tracker.ProjectID, item tracker.NativeWorkItemID, id string, at time.Time, headMatches bool, merge string) {
	t.Helper()
	head := "1111111111111111111111111111111111111111"
	recorded := head
	if !headMatches {
		recorded = "2222222222222222222222222222222222222222"
	}
	seedSQL(t, f, `WITH change AS (INSERT INTO change_requests(organization_id,project_id,work_item_id,id,current_version_id,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,landed_version_id,landed_at,landed_head_sha,landed_merge_sha,landed_base_ref) VALUES($1,$2,$3,$4,$4||'-version','orgs/'||$1||'/blobs/sha256/00/'||$8,$8,1,1,$4||'-version',$5,$6,$7,'main') RETURNING organization_id,project_id,id)
INSERT INTO change_versions(organization_id,id,change_id,project_id,head_sha,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT organization_id,id||'-version',id,project_id,$9,'orgs/'||organization_id||'/blobs/sha256/00/'||$8,$8,1,1 FROM change`, f.organization, project, item, id, at, recorded, merge, payloadDigest, head)
}

func seedSnapshotSources(t *testing.T, f tenant, scale int, now time.Time) {
	t.Helper()
	for index, project := range []tracker.ProjectID{project, secondProject} {
		for item := range 2*scale + 1 {
			native := tracker.NativeWorkItemID(fmt.Sprintf("%s-item-%d", project, item))
			seedItem(t, f, project, native, []string{"Todo", "Done"}[item%2], now.Add(-3*time.Hour))
			seedEvent(t, f, project, native, fmt.Sprintf("transition-%s-%d", project, item), 2, "workflow.transitioned", now.Add(-time.Duration(item*2+index+1)*time.Minute))
		}
		archived := tracker.NativeWorkItemID(fmt.Sprintf("%s-archived", project))
		seedItem(t, f, project, archived, "Todo", now.Add(-3*time.Hour))
		seedSQL(t, f, `UPDATE issues SET archived=true WHERE organization_id=$1 AND project_id=$2 AND native_id=$3`, f.organization, project, archived)
		for decision := range 10 {
			seedEvent(t, f, project, tracker.NativeWorkItemID(fmt.Sprintf("%s-item-0", project)), fmt.Sprintf("decision-%s-%d", project, decision), 10+decision, "scheduler.decision", now.Add(-time.Hour+time.Duration(decision*2+index)*time.Minute))
		}
		for landing := range 3 {
			at := now.Add(-2*time.Hour + time.Duration(landing*2+index)*time.Minute)
			seedLanding(t, f, project, tracker.NativeWorkItemID(fmt.Sprintf("%s-item-%d", project, landing)), fmt.Sprintf("change-%s-%d", project, landing), at, true, "3333333333333333333333333333333333333333")
		}
		seedLanding(t, f, project, tracker.NativeWorkItemID(fmt.Sprintf("%s-item-1", project)), fmt.Sprintf("change-%s-unverified-head", project), now.Add(-30*time.Minute), false, "3333333333333333333333333333333333333333")
		seedLanding(t, f, project, tracker.NativeWorkItemID(fmt.Sprintf("%s-item-2", project)), fmt.Sprintf("change-%s-unverified-merge", project), now.Add(-30*time.Minute), true, "not-a-merge")
	}
	seedSQL(t, f, `UPDATE issues SET actor_principal_id='reporter_example' WHERE organization_id=$1 AND native_id ~ '-item-[0-9]*[02468]$'`, f.organization)
	seedAttempts(t, f, "usage-source", "usage", 4, now.Add(-2*time.Hour), now.Add(-time.Hour))
	period := now.Add(-2 * time.Hour)
	seedSQL(t, f, `INSERT INTO cost_sources(organization_id,source_key,project_id,provider,provider_account,resource_id,bucket,metric,period_start,period_end,revision,attribution_hash,payload_hash,amount_micros,currency,voided) VALUES($1,'usage-source-1',$2,'openai','example','usage-attempt-1','runner_api','tokens','2026-01-01T00','2026-01-01T01',1,'example','example',1,'USD',false)`, f.organization, project)
	seedSQL(t, f, `INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete,currency) VALUES($1,'usage-source-1',1,$2,'usage-attempt-1',$3,'openai','example-model',10,0,5,$4,true,'USD')`, f.organization, project, period, float64(scale))
	for _, usage := range []struct {
		attempt, currency string
		project           tracker.ProjectID
	}{{"usage-attempt-1", "USD", project}, {"usage-attempt-2", "USD", project}, {"usage-attempt-3", "EUR", project}, {"usage-attempt-4", "USD", secondProject}} {
		seedSQL(t, f, `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'openai','example-model',100,0,50,$5,$6,$4)`, f.organization, usage.project, usage.attempt, period, float64(scale)/2, usage.currency)
	}
}

func TestDiagnosticsSnapshotKeepsTenantResults(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Hour)
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	fixtures := tenants(t)
	for index, f := range fixtures {
		hubtest.Project(t, f.repository, f.organization, hubtest.ProjectOptions{ID: secondProject, Name: "Second project"})
		seedSnapshotSources(t, f, index+1, now)
	}
	for index, f := range fixtures {
		t.Run(string(f.organization), func(t *testing.T) {
			scale := index + 1
			read := func(principal string) diagnosticstore.Snapshot {
				var snapshot diagnosticstore.Snapshot
				if err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					snapshot, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{project, secondProject}, w.From, w.To, w.To, principal)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			snapshot := read("")
			if snapshot.ResidencePartial || len(snapshot.Issues) != 2*(2*scale+1)+1 {
				t.Fatalf("issues=%d residence partial=%t, want %d complete", len(snapshot.Issues), snapshot.ResidencePartial, 2*(2*scale+1)+1)
			}
			for _, issue := range snapshot.Issues {
				if issue.WorkItemID == tracker.NativeWorkItemID(string(issue.ProjectID)+"-archived") || issue.State == "" {
					t.Fatalf("issue %+v is archived or stateless", issue)
				}
			}
			transitions, decisions := 0, []string{}
			for _, event := range snapshot.Events {
				if event.OrganizationID != f.organization {
					t.Fatalf("event %s belongs to %s", event.ID, event.OrganizationID)
				}
				switch event.Type {
				case "workflow.transitioned":
					transitions++
				case "scheduler.decision":
					decisions = append(decisions, event.ID)
				}
			}
			if transitions != 2*(2*scale+1) {
				t.Fatalf("transitions=%d want %d", transitions, 2*(2*scale+1))
			}
			wantDecisions := []string{}
			for minute := 4; minute < 20; minute++ {
				wantDecisions = append(wantDecisions, fmt.Sprintf("decision-%s-%d", []tracker.ProjectID{project, secondProject}[minute%2], minute/2))
			}
			if !snapshot.SchedulerPartial || !slices.Equal(decisions, wantDecisions) {
				t.Fatalf("decisions=%v partial=%t, want newest across projects %v", decisions, snapshot.SchedulerPartial, wantDecisions)
			}
			wantUsage := []diagnosticstore.Usage{{AttemptID: "usage-attempt-1", Cost: float64(scale), Tokens: 15, Complete: true}, {AttemptID: "usage-attempt-2", Cost: float64(scale) / 2, Tokens: 150, Complete: true}}
			if len(snapshot.Usage) != len(wantUsage) || snapshot.UsagePartial {
				t.Fatalf("usage=%+v partial=%t, want %+v", snapshot.Usage, snapshot.UsagePartial, wantUsage)
			}
			for index, usage := range snapshot.Usage {
				usage.Period = time.Time{}
				if usage != wantUsage[index] {
					t.Fatalf("usage[%d]=%+v want %+v", index, usage, wantUsage[index])
				}
			}
			wantLandings := []string{}
			for minute := 5; minute >= 0; minute-- {
				wantLandings = append(wantLandings, fmt.Sprintf("change-%s-%d", []tracker.ProjectID{project, secondProject}[minute%2], minute/2))
			}
			landings := []string{}
			for _, landing := range snapshot.Landings {
				landings = append(landings, landing.ChangeID)
			}
			if !slices.Equal(landings, wantLandings) {
				t.Fatalf("landings=%v want newest verified landings across projects %v", landings, wantLandings)
			}
			reporter := read("reporter_example")
			reporterTransitions, reporterDecisions := 0, []string{}
			for _, event := range reporter.Events {
				if event.Type == "workflow.transitioned" {
					reporterTransitions++
				} else {
					reporterDecisions = append(reporterDecisions, event.ID)
				}
			}
			reporterLandings := []string{}
			for _, landing := range reporter.Landings {
				reporterLandings = append(reporterLandings, landing.ChangeID)
			}
			wantReporterLandings := []string{"change-prj_second-2", "change-prj_security-2", "change-prj_second-0", "change-prj_security-0"}
			if len(reporter.Issues) != 2*(scale+1) || reporter.ResidencePartial || reporterTransitions != 2*(scale+1) || !slices.Equal(reporterDecisions, wantDecisions) || !reporter.SchedulerPartial || len(reporter.Usage) != 0 || !slices.Equal(reporterLandings, wantReporterLandings) {
				t.Fatalf("reporter issues=%d residence partial=%t transitions=%d decisions=%v usage=%d landings=%v", len(reporter.Issues), reporter.ResidencePartial, reporterTransitions, reporterDecisions, len(reporter.Usage), reporterLandings)
			}
		})
	}
}

func TestDiagnosticsReporterSeesClippingWithoutMatchingRows(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Hour)
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	for _, test := range []struct {
		name      string
		seed      func(*testing.T, tenant)
		residence bool
	}{
		{"open issues", func(t *testing.T, f tenant) {
			seedSQL(t, f, `INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name) SELECT $1,$2,'clip-item-'||n,1000000+n,'Diagnostic item','Todo' FROM generate_series(1,50001)n`, f.organization, project)
			seedItem(t, f, project, "reporter-item", "Todo", now.Add(-3*time.Hour))
			seedSQL(t, f, `UPDATE issues SET actor_principal_id='reporter_example' WHERE organization_id=$1 AND native_id='reporter-item'`, f.organization)
		}, true},
		{"leases", func(t *testing.T, f tenant) {
			seedItem(t, f, project, "lease-item", "Todo", now.Add(-3*time.Hour))
			seedSQL(t, f, `INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'diagnostics-machine','example',1,'example',$2,$2,$2)`, f.organization, now.Add(-2*time.Hour))
			seedSQL(t, f, `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,'clip-lease-'||n,i.id,'diagnostics-machine','clip-session-'||n,1,$3,$3,$3::timestamptz+interval '2 hours',$3::timestamptz+interval '1 hour' FROM generate_series(1,50001)n JOIN issues i ON i.organization_id=$1 AND i.native_id=$2`, f.organization, "lease-item", now.Add(-2*time.Hour))
		}, false},
	} {
		for _, f := range tenants(t) {
			t.Run(test.name+"/"+string(f.organization), func(t *testing.T) {
				test.seed(t, f)
				var snapshot diagnosticstore.Snapshot
				if err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					snapshot, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{project}, w.From, w.To, w.To, "reporter_example")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !snapshot.Partial || snapshot.ResidencePartial != test.residence || len(snapshot.Leases) != 0 {
					t.Fatalf("partial=%t residence=%t leases=%d issues=%d, want clipped without matching rows", snapshot.Partial, snapshot.ResidencePartial, len(snapshot.Leases), len(snapshot.Issues))
				}
			})
		}
	}
}
