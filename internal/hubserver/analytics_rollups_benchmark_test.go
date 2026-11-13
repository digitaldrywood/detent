package hubserver

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func BenchmarkAnalyticsRollupReads(b *testing.B) {
	ctx, fixture, service, credential, project, now := benchmarkPostgresDiagnostics(b)
	diagnosticsFixtureAttempts(ctx, b, fixture, project, "benchmark-source", "benchmark", 0, 0, now.Add(-8*24*time.Hour), now, tracker.NativeRunData{})
	content := eventstore.NewContent(fixture.details)
	raw, err := json.Marshal(tracker.CollaborationEvent{ID: "unread-history", OrganizationID: fixture.organization, ProjectID: project, AggregateID: "benchmark-source", Type: "issue.edited", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "benchmark"}, RecordedAt: now})
	if err != nil {
		b.Fatal(err)
	}
	payload, err := content.PrepareEvent(ctx, fixture.organization, raw)
	if err != nil {
		b.Fatal(err)
	}

	var events []eventstore.Event
	var selected []int32
	for n := 50000; len(events) < diagnosticstore.ObjectLimit+1; n-- {
		if n%100 == 0 {
			continue
		}
		at := now.Add(-7*24*time.Hour + time.Duration(n*12)*time.Second)
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("benchmark-event-%d", n), OrganizationID: fixture.organization, ProjectID: project, AggregateID: "benchmark-source", Type: "scheduler.decision", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "benchmark"}, RecordedAt: at, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "capacity", At: at}}})
		if err != nil {
			b.Fatal(err)
		}
		event, err := content.PrepareEvent(ctx, fixture.organization, raw)
		if err != nil {
			b.Fatal(err)
		}
		events = append(events, event)
		selected = append(selected, int32(n))
	}
	if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $1,$2,'benchmark-source','benchmark-event-'||n,n,CASE WHEN n%100=0 THEN 'workflow.transitioned' ELSE 'scheduler.decision' END,1,'runner','benchmark',CASE WHEN (n/100)%2=1 THEN 'Todo' ELSE 'In Progress' END,CASE WHEN (n/100)%2=1 THEN 'In Progress' ELSE 'Todo' END,$3::timestamptz-interval '7 days'+n*interval '12 seconds',$4,$5,$6,$7 FROM generate_series(1,50000)n WHERE n<>ALL($8::integer[])`, fixture.organization, project, now, payload.PayloadObjectKey, payload.PayloadSHA256, payload.PayloadBytes, payload.PayloadContentBytes, selected); err != nil {
		b.Fatal(err)
	}
	for index, event := range events {
		if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,actor_envelope_known,actor_via_key,actor_display_name,actor_agent,scheduler_outcome,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`, event.OrganizationID, event.ProjectID, event.WorkItemID, event.ID, selected[index], event.Type, event.SchemaVersion, event.ActorKind, event.ActorPrincipalID, event.ActorEnvelopeKnown, event.ActorViaKey, event.ActorDisplayName, event.ActorAgent, event.SchedulerOutcome, event.RecordedAt, event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes); err != nil {
			b.Fatal(err)
		}
	}

	var count int
	if err := fixture.admin.QueryRowContext(ctx, "SELECT count(*) FROM collaboration_events WHERE organization_id=$1", fixture.organization).Scan(&count); err != nil || count != 50000 {
		b.Fatalf("diagnostics history events=%d want=50000 error=%v", count, err)
	}
	for _, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour} {
		w := operatortool.AnalyticsWindow{From: now.Add(-span), To: now, Bucket: time.Hour}
		b.Run(fmt.Sprintf("postgres/diagnostics/%dh", int(span.Hours())), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				report, err := service.readDiagnosticsTenant(ctx, credential, w, project)
				if err != nil {
					b.Fatal(err)
				}
				if len(report.Capacity) != int(span.Hours()) || !report.Partial {
					b.Fatalf("bounded diagnostic history workload changed: %+v", report)
				}
			}
		})
	}
}

func BenchmarkAnalyticsReportSubreads(b *testing.B) {
	ctx, fixture, service, _, project, now := benchmarkPostgresReportFacts(b)
	hash := apikey.HashToken("benchmark-analytics-credential")
	if err := fixture.repository.WriteTenant(ctx, fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: fixture.organization, ID: "benchmark-analytics", Name: "Benchmark reader", Hash: hash, Scope: "operator", NativeOnly: true, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: fixture.organization, TokenID: "benchmark-analytics", ProjectID: project})
	}); err != nil {
		b.Fatal(err)
	}
	credential, status, err := service.authenticateTenantAPIHash(ctx, fixture.organization, hash, "", "")
	if err != nil || status != http.StatusOK {
		b.Fatalf("benchmark authentication status=%d error=%v", status, err)
	}
	scope := nativeScope{organization: fixture.organization, project: project, credential: credential}
	content := eventstore.NewContent(fixture.details)
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			b.Fatal(err)
		}
		return raw
	}
	for number := 0; number < 100; number++ {
		id := fmt.Sprintf("benchmark-change-%d", number)
		actor := tracker.Actor{Kind: "human", PrincipalID: "example"}
		at := now.Add(-time.Hour)
		version := tracker.ChangeVersion{ID: "version-" + id, ChangeID: id, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("b", 40), PolicyID: "example-policy"}, Actor: actor, CreatedAt: at}
		landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat("a", 40), BaseRef: "develop", LandedAt: at}
		change := tracker.ChangeRequest{ID: id, OrganizationID: fixture.organization, ProjectID: project, WorkItemID: "diagnostics-current", Revision: 1, Title: "Benchmark change", CurrentVersion: version.ID, CreatedAt: at, UpdatedAt: at, Landed: &landing}
		changeRow, err := content.PrepareChange(ctx, fixture.organization, encode(change))
		if err != nil {
			b.Fatal(err)
		}
		versionRow, err := content.PrepareVersion(ctx, fixture.organization, encode(version))
		if err != nil {
			b.Fatal(err)
		}
		frozen, err := content.PrepareVersionLanding(ctx, eventstore.VersionLanding{OrganizationID: fixture.organization, ProjectID: project, WorkItemID: change.WorkItemID, ChangeID: id, VersionID: version.ID}, landing)
		if err != nil {
			b.Fatal(err)
		}
		if err := fixture.repository.WriteTenant(ctx, fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(ctx, changeRow); err != nil {
				return err
			}
			if _, err := w.CreateVersion(ctx, versionRow); err != nil {
				return err
			}
			return w.RecordVersionLanding(ctx, frozen)
		}); err != nil {
			b.Fatal(err)
		}
	}
	for _, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		window := operatortool.AnalyticsWindow{From: now.Add(-span), To: now, Bucket: time.Hour}
		for _, read := range []struct {
			name string
			run  func() error
		}{
			{"attempts", func() error {
				usage, _, err := service.readNativeAnalyticsUsagePopulationTenant(ctx, scope, window)
				if err != nil {
					return err
				}
				if len(usage.Items) != 262 {
					b.Fatalf("usage population=%d want262", len(usage.Items))
				}
				page, _, err := service.readNativeAnalyticsAttemptPopulationTenant(ctx, scope, window, "", usage.Items, usage.Partial)
				if err == nil && len(page.Items) != 469 {
					b.Fatalf("attempt population=%d want469", len(page.Items))
				}
				return err
			}},
			{"quality", func() error {
				_, err := service.readAnalyticsQualityTenant(ctx, scope, operatortool.AnalyticsRequest{Limit: maxAnalyticsPopulation}, window)
				return err
			}},
			{"landings", func() error {
				page, err := service.readAnalyticsLandingPopulationTenant(ctx, scope, window, "landings", nil, nil)
				if err == nil && len(page.Items) != 100 {
					b.Fatalf("landing population=%d want100", len(page.Items))
				}
				return err
			}},
		} {
			b.Run(fmt.Sprintf("postgres/%s/%dh", read.name, int(span.Hours())), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if err := read.run(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkDiagnosticsProjectionReads(b *testing.B) {
	ctx, fixture, service, credential, project, now := benchmarkPostgresReportFacts(b)
	w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
	for _, focused := range []bool{false, true} {
		b.Run(fmt.Sprintf("postgres/focused=%t", focused), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if focused {
					var snapshot diagnosticstore.Snapshot
					if err := fixture.repository.ReadTenantSnapshot(ctx, fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						snapshot, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(ctx, []tracker.ProjectID{project}, w.From, w.To, now, "")
						return err
					}); err != nil {
						b.Fatal(err)
					}
					if snapshot.Attempts != 469 || len(snapshot.Usage) != 262 {
						b.Fatalf("focused diagnostics lost current population: attempts=%d usage=%d", snapshot.Attempts, len(snapshot.Usage))
					}
				} else {
					report, err := service.readDiagnosticsTenant(ctx, credential, w, project)
					if err != nil {
						b.Fatal(err)
					}
					if report.Coverage[1].Observed == nil || *report.Coverage[1].Observed != 469 {
						b.Fatalf("full diagnostics lost current population: %+v", report.Coverage)
					}
				}
			}
		})
	}
}

func benchmarkPostgresDiagnostics(b *testing.B) (context.Context, diagnosticsPostgresFixture, *Service, hubapi.APICredential, tracker.ProjectID, time.Time) {
	b.Helper()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil || timeout <= 0 {
		b.Fatal("PostgreSQL benchmarks require a positive go test -timeout")
	}
	ctx, cancel := context.WithTimeout(b.Context(), timeout)
	b.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	admin := postgresStartupTemplate.DatabaseContext(ctx, b)
	var databaseName string
	if err := admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		b.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "ALTER DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" CONNECTION LIMIT 5"); err != nil {
		b.Fatal(err)
	}
	name, password := hubapi.NewID("example"), hubapi.NewID("password")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		cleanup, finish := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer finish()
		if _, err := admin.ExecContext(cleanup, "DROP OWNED BY "+quoted); err != nil {
			b.Error(err)
		}
		if _, err := admin.ExecContext(cleanup, "DROP ROLE "+quoted); err != nil {
			b.Error(err)
		}
	})
	if _, err := admin.ExecContext(ctx, "GRANT detent_runtime_v2 TO "+quoted); err != nil {
		b.Fatal(err)
	}
	config := postgresStartupTemplate.NativeConfig(b, admin, name, password)
	config.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(ctx, pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := repository.Close(); err != nil {
			b.Error(err)
		}
	})
	organization, project := tracker.OrganizationID("org_benchmark"), tracker.ProjectID("prj_benchmark")
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	details := &memoryRunDetails{}
	service := &Service{config: (Config{TenantRepository: repository, RunDetails: details, Hosted: &HostedConfig{OrganizationID: string(organization)}, now: func() time.Time { return now }}).normalized()}
	authority := &apikey.KeyAuthority{ID: "benchmark-key", Kind: "service", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
	if !authority.Valid() {
		b.Fatal("invalid benchmark authority")
	}
	credential := hubapi.APICredential{EntryKey: authority}
	return ctx, diagnosticsPostgresFixture{repository: repository, admin: admin, details: details, organization: organization}, service, credential, project, now
}

func benchmarkPostgresReportFacts(b *testing.B) (context.Context, diagnosticsPostgresFixture, *Service, hubapi.APICredential, tracker.ProjectID, time.Time) {
	ctx, fixture, service, credential, project, now := benchmarkPostgresDiagnostics(b)
	diagnosticsFixtureAttempts(ctx, b, fixture, project, "diagnostics-current", "current", 469, 469, now.Add(-2*time.Hour), now.Add(-time.Hour), tracker.NativeRunData{Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "Recorded diagnostic failure"}})
	diagnosticsFixtureAttempts(ctx, b, fixture, project, "diagnostics-old", "history", 100000, 0, now.Add(-365*24*time.Hour), now.Add(-365*24*time.Hour), tracker.NativeRunData{})
	if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,native_created_at,native_updated_at) SELECT $1,$2,'diagnostics-issue-'||n,n+2,'Example issue','Todo',$3,$3 FROM generate_series(1,592)n`, fixture.organization, project, now.Add(-8*24*time.Hour)); err != nil {
		b.Fatal(err)
	}
	if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,'diagnostics-extra-'||n,i.id,'diagnostics-machine','diagnostics-extra-'||n,1,$3,$3,$4::timestamptz+interval '1 hour',$4 FROM generate_series(1,1213)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id='diagnostics-current'`, fixture.organization, project, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		b.Fatal(err)
	}
	if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,'current-attempt-'||n,$3,'provider','model',10,0,1,1,'USD',$3 FROM generate_series(1,262)n`, fixture.organization, project, now.Add(-time.Hour)); err != nil {
		b.Fatal(err)
	}
	ref, err := fixture.details.StoreDetail(ctx, string(fixture.organization), []byte("Benchmark comment"))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := fixture.admin.ExecContext(ctx, `INSERT INTO native_comments(organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id,created_at,updated_at) SELECT $1,'benchmark-comment-'||n,$2,'diagnostics-current',n,$3,'human','benchmark',$4,$4 FROM generate_series(1,7000)n`, fixture.organization, project, ref.Key, now); err != nil {
		b.Fatal(err)
	}
	for _, test := range []struct {
		relation string
		expected int
	}{{"issues", 594}, {"native_attempts", 100469}, {"leases", 101682}, {"attempt_usage", 262}, {"native_comments", 7000}} {
		var count int
		if err := fixture.admin.QueryRowContext(ctx, "SELECT count(*) FROM "+test.relation+" WHERE organization_id=$1", fixture.organization).Scan(&count); err != nil || count != test.expected {
			b.Fatalf("diagnostic fixture %s count=%d want=%d error=%v", test.relation, count, test.expected, err)
		}
	}
	return ctx, fixture, service, credential, project, now
}
