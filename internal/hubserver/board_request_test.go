package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardRequestHasFixedSQLBudget(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	repository := service.config.TenantRepository
	value, _ := projectRecordDatabases.Load(original)
	wire := &boardWire{}
	if delay, err := strconv.Atoi(os.Getenv("A5_BOARD_RTT_MS")); err == nil {
		wire.delay = time.Duration(delay) * time.Millisecond
	}
	measured := boardWireRepository(t, value.(*sql.DB), wire)
	t.Cleanup(func() { service.config.TenantRepository = original; service.config.AccessRepository = original })
	expectedCounts := map[string][2]int{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		service.config.TenantRepository = original
		service.config.AccessRepository = original
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, org, "Example")
		service.localOrganization = string(org)
		scope := nativeScope{organization: org, project: fixture.project.ID, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		now := service.config.now().UTC().Truncate(time.Microsecond)
		seeded := 0
		for _, size := range []int{20, 500} {
			service.config.TenantRepository = original
			service.config.AccessRepository = original
			details.beforeRead = nil
			for seeded < size {
				seeded++
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_example_%04d", seeded))
				card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, Number: seeded, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: now, UpdatedAt: now}}
				payload, err := viewstore.NewContent(service.runDetailStore()).PrepareBoard(t.Context(), card)
				if err != nil {
					t.Fatal(err)
				}
				err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: scope.project, NativeID: id, Number: int64(seeded), Title: "Example", WorkflowStateName: new("Todo")})
					if err != nil {
						return err
					}
					row := viewstore.BoardRecord{OrganizationID: org, ProjectID: scope.project, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("%s#%04d", scope.project, seeded)}, Dispatchable: true, CreatedAt: now, UpdatedAt: now, Payload: payload, ActorPrincipalID: "example", PageIssue: &card.Issue, ListIssue: &card.Issue, ListBytes: 1024, ProjectedAt: now, ReporterRelations: &viewstore.BoardReporterRelations{DependencyReady: true}, Display: viewstore.BoardDisplay{Ready: true, Exact: true, Title: "Example", Number: seeded, Actor: tracker.Actor{Kind: "human"}}}
					return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: scope.project, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			service.config.TenantRepository = measured
			service.config.AccessRepository = measured
			for _, limit := range []int{1, 10, 200} {
				for _, include := range []string{"board", "work"} {
					t.Run(fmt.Sprintf("%s/%d/%d/%s", org, size, limit, include), func(t *testing.T) {
						details.beforeRead = func(context.Context) error { return errors.New("page request read an object") }
						statements := []string{}
						ctx := postgres.WithTransactionTiming(t.Context(), func(string, time.Time) {}, func(_ time.Time, query string) { statements = append(statements, query) })
						if err := measured.Ping(t.Context()); err != nil {
							t.Fatal(err)
						}
						wire.reset()
						page, err := service.readBoardRecords(ctx, scope, url.Values{"include": {include}, "limit": {fmt.Sprint(limit)}})
						if err != nil {
							t.Fatal(err)
						}
						if len(page.Items) == 0 || len(page.Items) > min(limit, size) || page.Total != -1 {
							t.Fatalf("items=%d total=%d", len(page.Items), page.Total)
						}
						if include == "work" && ((len(page.Work.Items) == 0 || len(page.Work.Items) > min(limit, size)) || page.Work.Items[0].Number != size) {
							t.Fatalf("work page=%+v", page.Work)
						}
						for _, item := range page.Items {
							if item.OrganizationID != org {
								t.Fatal("cross-tenant item")
							}
						}
						exchanges, actualStatements := wire.stop()
						if exchanges > 9 || actualStatements > 16 {
							t.Errorf("wire exchanges=%d statements=%d", exchanges, actualStatements)
						}
						t.Logf("wire exchanges=%d statements=%d", exchanges, actualStatements)
						key := fmt.Sprintf("%s/%d", include, limit)
						if expected, ok := expectedCounts[key]; ok {
							if expected != [2]int{exchanges, actualStatements} {
								t.Errorf("SQL/wire cost changed with data size or tenant: got %d/%d want %v", exchanges, actualStatements, expected)
							}
						} else {
							expectedCounts[key] = [2]int{exchanges, actualStatements}
						}

						if samples, err := strconv.Atoi(os.Getenv("A5_BOARD_RTT_SAMPLES")); err == nil && samples > 0 && limit == 10 {
							durations := make([]time.Duration, 0, samples)
							for range samples {
								wire.reset()
								started := time.Now()
								_, err := service.readBoardRecords(t.Context(), scope, url.Values{"include": {include}, "limit": {"10"}})
								durations = append(durations, time.Since(started))
								wire.stop()
								if err != nil {
									t.Fatal(err)
								}
							}
							slices.Sort(durations)
							t.Logf("RTT=%s samples=%d p50=%s p95=%s", wire.delay, samples, durations[len(durations)/2], durations[(len(durations)-1)*95/100])
						}
						count := len(statements) + 2
						t.Logf("statements=%d", count)
						modelReads := 0
						for _, query := range statements {
							if strings.Contains(query, "native_board_sequences") || strings.Contains(query, "WITH page AS MATERIALIZED") {
								modelReads++
							}
							if strings.Contains(query, "RelationshipBatch") || strings.Contains(query, "FROM issue_dependency_reasons") || strings.Contains(query, "count(*)") {
								t.Errorf("serving query derives page data: %s", strings.Split(query, "\n")[0])
							}
						}
						if modelReads != 2 {
							t.Errorf("model reads=%d want 2", modelReads)
						}
						if count > 16 {
							for _, query := range statements {
								t.Log(strings.Split(query, "\n")[0])
							}
							t.Errorf("SQL statements=%d, want <=16", count)
						}
					})
				}
			}
		}
	}
}
