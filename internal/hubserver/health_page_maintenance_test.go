package hubserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthPageSchedulerAndLiveRoute(t *testing.T) {
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	original := service.database.postgres
	stored, _ := projectRecordDatabases.Load(original)
	admin := stored.(*sql.DB)
	wire := &boardWire{delay: 12 * time.Millisecond}
	measured := boardWireRepository(t, admin, wire)
	probe := &pageObjectProbe{DetailStore: details, t: t, repository: original}
	service.config.RunDetails = hubmaintenance.LimitDetails(probe)
	t.Cleanup(func() { service.config.TenantRepository = original; service.config.AccessRepository = original })
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			service.config.TenantRepository = original
			service.config.AccessRepository = original
			fixture := newNativeFixture(t, service, org, "Example")
			raw, err := json.Marshal(healthEvidence{Reason: string(org), EventIDs: []string{"event"}, Counts: map[string]int{"attempts": 3}, Values: map[string]float64{"rate": 2.5}, Queues: map[string]healthQueueEvidence{string(fixture.project.ID): {QueueDepth: 4}}, Productivity: &healthProductivityEvidence{WindowFrom: service.config.now(), WindowTo: service.config.now(), LatestFailureStage: "pre_provider"}})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := details.StoreDetail(t.Context(), string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubmaintenance.New(maintenanceTenantRegistry{{Organization: string(org), Generation: 1}}, []hubmaintenance.Feature{service.HealthPageMaintenanceFeature()})
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{2, 50} {
				_, err = admin.ExecContext(t.Context(), `INSERT INTO health_findings(organization_id,id,fingerprint,signal,class,subject_kind,subject_id,opened_at,last_seen_at,severity,summary,next_action,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes)
 SELECT $1,'finding_'||n,'fingerprint_'||n,'signal','class','project',$2,now(),now(),'attention',$1,'inspect',$3,$4,$5,$6 FROM generate_series(1,$7::int) n ON CONFLICT DO NOTHING`, org, fixture.project.ID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, size)
				if err != nil {
					t.Fatal(err)
				}
				_, err = admin.ExecContext(t.Context(), `INSERT INTO health_finding_projects(organization_id,finding_id,project_id) SELECT organization_id,id,$2 FROM health_findings WHERE organization_id=$1 ON CONFLICT DO NOTHING`, org, fixture.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				path := fixture.base + "/health/findings?limit=100"
				service.config.TenantRepository = original
				service.config.AccessRepository = original
				probe.forbidden = false
				cold := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
				requireNativeStatus(t, cold, http.StatusOK)
				var fallback healthFindingsPage
				decodeHubResponse(t, cold, &fallback)
				if len(fallback.Items) != size || fallback.Items[0].Evidence.Reason != string(org) {
					t.Fatalf("cold fallback lost findings: %+v", fallback)
				}
				probe.forbidden = false
				for range size {
					if err := scheduler.Tick(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				service.config.TenantRepository = measured
				service.config.AccessRepository = measured
				probe.forbidden = true
				wire.reset()
				started := time.Now()
				response := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
				exchanges, statements := wire.stop()
				requireNativeStatus(t, response, http.StatusOK)
				var page healthFindingsPage
				decodeHubResponse(t, response, &page)
				if len(page.Items) != size || page.Items[0].Evidence.Reason != string(org) || page.Items[0].Evidence.Counts["attempts"] != 3 || page.Items[0].Evidence.Values["rate"] != 2.5 || page.Items[0].Evidence.Queues[string(fixture.project.ID)].QueueDepth != 4 || page.Items[0].Evidence.Productivity == nil {
					t.Fatalf("wrong projected page: %+v", page)
				}
				if statements != 18 || exchanges != 15 {
					t.Errorf("live route budget changed: %d statements / %d exchanges", statements, exchanges)
				}
				durations := []time.Duration{time.Since(started)}
				for range 13 {
					began := time.Now()
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
					durations = append(durations, time.Since(began))
				}
				slices.Sort(durations)
				p95 := durations[len(durations)*95/100]
				if p95 > time.Second {
					t.Errorf("health page p95 at 12ms RTT = %s", p95)
				}
				t.Logf("%s/%dx live health route statements=%d exchanges=%d p95=%s", org, size, statements, exchanges, p95)
				probe.forbidden = false

			}

			service.config.TenantRepository = original
			service.config.AccessRepository = original
			probe.forbidden = false
			if _, err := admin.ExecContext(t.Context(), `UPDATE health_findings SET resolved_at=CASE WHEN id IN('finding_5','finding_9','finding_50') THEN NULL ELSE now() END WHERE organization_id=$1`, org); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"ready", "cold"} {
				if mode == "cold" {
					if _, err := admin.ExecContext(t.Context(), `UPDATE health_finding_projects SET page_sequence=0 WHERE organization_id=$1`, org); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE health_findings SET page_checkpoint=0 WHERE organization_id=$1`, org); err != nil {
						t.Fatal(err)
					}
				}
				for _, state := range []string{"open", "all"} {
					t.Run(mode+"/cursor/"+state, func(t *testing.T) {
						var ids []string
						cursor := ""
						emptyContinuation := false
						for windows := 0; ; windows++ {
							if windows > 50 {
								t.Fatal("health cursor did not terminate")
							}
							path := fixture.base + "/health/findings?state=" + state + "&limit=2"
							if cursor != "" {
								path += "&cursor=" + cursor
							}
							response := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
							requireNativeStatus(t, response, http.StatusOK)
							var page healthFindingsPage
							decodeHubResponse(t, response, &page)
							if len(page.Items) > 2 {
								t.Fatalf("page exceeds requested limit: %d", len(page.Items))
							}
							for _, item := range page.Items {
								ids = append(ids, item.ID)
							}
							if page.NextCursor == "" {
								break
							}
							if page.NextCursor == cursor {
								t.Fatal("health cursor repeated")
							}
							if len(page.Items) == 0 {
								emptyContinuation = true
							}
							cursor = page.NextCursor
						}
						if state == "open" {
							if !slices.Equal(ids, []string{"finding_5", "finding_9", "finding_50"}) || !emptyContinuation {
								t.Fatalf("sparse cursor matches=%v empty continuation=%v", ids, emptyContinuation)
							}
						} else {
							unique := map[string]bool{}
							for _, id := range ids {
								unique[id] = true
							}
							if len(ids) != 50 || len(unique) != 50 {
								t.Fatalf("dense cursor findings=%d unique=%d", len(ids), len(unique))
							}
						}
					})
				}
			}

		})
	}
}
