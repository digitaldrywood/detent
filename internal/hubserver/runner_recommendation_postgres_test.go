package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCapacityRecommendationReadOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	f := newSharedRouteFixture(t, func(cfg *Config) { cfg.now = func() time.Time { return now } })

	var first *Service
	for _, service := range f.services {
		first = service
		break
	}
	runtimeConfig, err := first.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDB := stdlib.OpenDB(*runtimeConfig.ConnConfig)
	adminConnection := postgresStartupTemplate.Connection(t, runtimeDB, "", "")
	if err := runtimeDB.Close(); err != nil {
		t.Fatal(err)
	}
	adminConfig, err := adminConnection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*adminConfig.ConnConfig)
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Error(err)
		}
	})
	now = time.Now().UTC()
	to := now.Truncate(time.Hour)
	events := map[string]map[string]eventstore.Event{}
	for org, service := range f.services {
		organization := tracker.OrganizationID(org)
		events[org] = map[string]eventstore.Event{}
		repository := service.database.postgres
		content := eventstore.NewContent(service.runDetailStore())
		receipt := func(operation, key string) eventstore.Receipt {
			t.Helper()
			r, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization, ActorID: "production-import", Operation: operation, CommandKey: key, RequestHash: artifact.Digest([]byte(operation + key)), CreatedAt: now}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		for index := range 2 {
			runner := fmt.Sprintf("runner_%d", index)
			project := tracker.ProjectID(fmt.Sprintf("project_%d", index))
			hours, runs := recommendationFixture()
			for i := range hours {
				hours[i].Hour = to.Add(time.Duration(i-24) * time.Hour)
				hours[i].SegmentID = hours[i].Hour.Add(time.Minute)
				hours[i].MemoryAvailableSumBytes = hours[i].MemoryAvailableMinBytes * hours[i].MemorySampleCount
				hours[i].CPUBusyMaxPercent = 10
			}
			for i := range runs {
				runs[i].StartedAt = hours[i].Hour
			}
			var item corestore.WorkItem
			err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: project, CanWrite: true, ManageRunner: true}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: org + "_owner", ProjectID: project}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.CreateAccessCredential(t.Context(), accessCredentialForRunner(organization, runner, now)); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: runner, Hostname: runner, Capacity: 4, Version: "example"}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: runner, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenHash: apikey.HashToken(org + runner + "enroll"), Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), CreatedBy: org + "_owner"}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: tracker.MachineID(runner), TokenID: runner, EnrollmentID: runner, Scope: "organization", State: "active", MachineUse: "dedicated", Operations: []string{runnerauth.Read, runnerauth.Claim}, CapacityLimit: 4, ReportedCapacity: 4, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				var err error
				item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{ID: int64(index + 1), OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID("item_" + runner), Number: 1, Title: "Example", CreatedAt: now.Add(-25 * time.Hour)})
				if err != nil {
					return fmt.Errorf("seed %s/%s: %w", org, runner, err)
				}
				segments := make([]hostmetrics.Summary, len(hours))
				for i := range hours {
					segments[i] = hours[i].Summary
				}
				_, err = w.AppendRunnerHostMetricSegments(t.Context(), runner, segments, now)
				return err
			})
			if err != nil {
				var failure *pgconn.PgError
				if errors.As(err, &failure) {
					t.Fatalf("seed %s/%s: SQLSTATE=%s constraint=%s detail=%s", org, runner, failure.Code, failure.ConstraintName, failure.Detail)
				}
				t.Fatal(err)
			}
			for i := 0; i < 15; i++ {
				run := runs[i%len(runs)]
				status := "succeeded"
				if i == 0 {
					status = "failed"
				}
				if i == 1 {
					status = "cancelled"
				}
				if i >= 10 {
					run.StartedAt = hours[i].Hour
					run.Usage.PeakMemoryBytes = 128 * capacityGiB
				}
				switch i {
				case 10:
					status = "running"
				case 11:
					run.StartedAt = to.Add(-720 * time.Hour)
				case 12:
					status = "interrupted"
				case 13:
					run.Usage.PeakMemoryBytes = 4096
					run.Usage.UserCPUSeconds, run.Usage.SystemCPUSeconds, run.Usage.WallSeconds = 1e308, 1e308, 1
				case 14:
					run.Usage.PeakMemoryBytes = 4096
					run.Usage.UserCPUSeconds, run.Usage.SystemCPUSeconds, run.Usage.WallSeconds = 1e308, 0, 1e-300
				}
				key := fmt.Sprintf("%s_%d", runner, i)
				end := run.StartedAt.Add(time.Hour)
				lease := leasestore.Lease{OrganizationID: organization, LeaseID: key, FencingToken: int64(index*100 + i + 1), IssueID: item.ID, MachineID: runner, SessionID: key, WorkItemRevision: item.Revision, AcquiredAt: run.StartedAt, RenewedAt: run.StartedAt, ExpiresAt: end, ReleasedAt: &end}
				if err := repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: receipt("core-lease", key)}); err != nil {
					t.Fatal(err)
				}
				assignment := runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: key, RunnerID: runner, AssignedAt: run.StartedAt}
				if err := repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: receipt("core-runner-lease-assignment", key)}); err != nil {
					t.Fatal(err)
				}
				usage := run.Usage
				if org == "org_other" {
					usage.PeakMemoryBytes *= 2
				}
				data := tracker.NativeRunData{AttemptID: key, RunID: key, LeaseID: tracker.LeaseID(key), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, RunnerID: runner, Outcome: status, ProcessUsage: &usage}
				if i == 0 {
					data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Landing: &tracker.NativeLandingReceipt{Waiting: true, ChangeID: "change_example", VersionID: "version_example", HeadSHA: strings.Repeat("a", 40)}}
				}
				if status == "running" {
					data.Outcome = ""
					data.ProcessUsage = nil
				}
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := service.runDetailStore().StoreDetail(t.Context(), org, raw)
				if err != nil {
					t.Fatal(err)
				}
				attempt := leasestore.Attempt{OrganizationID: organization, ID: key, IssueID: item.ID, LeaseID: key, FencingToken: lease.FencingToken, RunID: key, Sequence: 1, Status: status, WorkItemRevision: item.Revision, PayloadObjectKey: ref.Key, StartedAt: run.StartedAt, UpdatedAt: end}
				if err := repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt("core-attempt", key)}); err != nil {
					t.Fatal(err)
				}
				event := tracker.CollaborationEvent{ID: key, OrganizationID: organization, ProjectID: project, AggregateID: item.NativeID, Type: "run.finished", SchemaVersion: 1, RecordedAt: end, Actor: tracker.Actor{Kind: "runner", PrincipalID: runner}, Data: tracker.CollaborationData{Run: &data}}
				if status == "running" {
					event.Type = "run.started"
				}
				raw, err = json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := content.PrepareEvent(t.Context(), organization, raw)
				if err != nil {
					t.Fatal(err)
				}
				events[org][key] = prepared
				for replay := range 2 {
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						appendEvent := func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), prepared); return err }
						if i == 0 && replay == 0 {
							return w.(interface {
								WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
							}).WithRunnerEventFacts(t.Context(), eventstore.RunCommand{Project: project, WorkItem: item.NativeID, LeaseID: key, AttemptID: key, Operation: "process-projection"}, appendEvent)
						}
						return appendEvent(w)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	for pass := range 2 {
		for org, service := range f.services {
			for index := range 2 {
				t.Run(fmt.Sprintf("%s/%d/%d", org, index, pass), func(t *testing.T) {
					runner := fmt.Sprintf("runner_%d", index)
					originalObjects := service.config.RunDetails
					guard := &capacityObjectReadGuard{DetailStore: service.runDetailStore()}
					service.config.RunDetails = guard
					defer func() { service.config.RunDetails = originalObjects }()
					if pass == 0 {
						t.Run("CPU rate overflow HTTP and MCP", func(t *testing.T) {
							keys := make([]string, 10)
							for i := range keys {
								keys[i] = fmt.Sprintf("%s_%d", runner, i)
							}
							if _, err := admin.ExecContext(t.Context(), "UPDATE report_event_facts SET process_evidence_status='unavailable' WHERE organization_id=$1 AND event_id=ANY($2::text[])", org, keys); err != nil {
								t.Fatal(err)
							}
							defer func() {
								if _, err := admin.ExecContext(t.Context(), "UPDATE report_event_facts SET process_evidence_status='usable' WHERE organization_id=$1 AND event_id=ANY($2::text[])", org, keys); err != nil {
									t.Error(err)
								}
							}()
							assertAdvice := func(advice runnerRecommendation) {
								t.Helper()
								if advice.Partial || len(advice.Periods) != 1 || advice.Periods[0].Status != "insufficient_evidence" || advice.Periods[0].RunsObserved != 0 || advice.Periods[0].P90CPUCores != 0 || advice.Periods[0].Slots != nil {
									t.Errorf("invalid rate became usable evidence: %+v", advice)
								}
							}
							httpRead := f.request(t, org, "/api/v2/organizations/"+org+"/runners/"+runner+"/capacity", "", func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = http.MethodGet })
							if httpRead.Code != http.StatusOK {
								t.Errorf("HTTP overflow read status=%d body=%s", httpRead.Code, httpRead.Body.String())
							}
							var got runnerCapacityRead
							if err := json.Unmarshal(httpRead.Body.Bytes(), &got); err != nil {
								t.Errorf("HTTP overflow response is not JSON: %v", err)
							} else {
								assertAdvice(got.Recommendation)
							}
							raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.GetRunnerCapacity, "arguments": map[string]any{"runner_id": runner}, "_meta": json.RawMessage(fleetProtocolMeta)}})
							if err != nil {
								t.Fatal(err)
							}
							mcp := f.request(t, org, "/api/v2/organizations/"+org+"/mcp", string(raw), sharedRouteBrowserClaims, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.GetRunnerCapacity, "X-CSRF-Token": strings.Repeat("b", 64)})
							if mcp.Code != http.StatusOK {
								t.Errorf("MCP overflow read status=%d body=%s", mcp.Code, mcp.Body.String())
							}
							var envelope struct {
								Result struct {
									Data struct {
										Data runnerCapacityRead `json:"data"`
									} `json:"structuredContent"`
									IsError bool `json:"isError"`
								} `json:"result"`
							}
							if err := json.Unmarshal(mcp.Body.Bytes(), &envelope); err != nil {
								t.Errorf("MCP overflow response is not JSON: %v", err)
							} else if envelope.Result.IsError {
								t.Errorf("MCP overflow read failed: %s", mcp.Body.String())
							} else {
								assertAdvice(envelope.Result.Data.Data.Recommendation)
							}
							if guard.reads.Load() != 0 {
								t.Errorf("overflow read fetched %d history objects", guard.reads.Load())
							}
						})
					}

					path := "/api/v2/organizations/" + org + "/runners/" + runner
					read := func(suffix string) []byte {
						t.Helper()
						response := f.request(t, org, path+suffix, "", func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = http.MethodGet })
						requireNativeStatus(t, response, http.StatusOK)
						return response.Body.Bytes()
					}
					before := read("/routing")
					var out runnerCapacityRead
					if err := json.Unmarshal(read("/capacity"), &out); err != nil {
						t.Fatal(err)
					}
					peak := uint64(4 * capacityGiB)
					if org == "org_other" {
						peak *= 2
					}
					if out.Recommendation.Partial || !out.Recommendation.Advisory || len(out.Recommendation.Periods) != 1 {
						t.Fatalf("recommendation=%+v", out.Recommendation)
					}
					p := out.Recommendation.Periods[0]
					slots := 14
					if org == "org_other" {
						slots = 7
					}
					if p.Slots == nil || *p.Slots != (slotRange{Min: slots, Max: slots}) {
						t.Fatalf("slot range=%+v", p)
					}
					if p.Status != "recommended" || p.HoursCovered != 24 || p.RunsObserved != 10 || p.SlotHours != 14 || p.P90PeakMemoryBytes != peak || p.P90CPUCores != 1 || p.Slots == nil {
						t.Fatalf("period=%+v", p)
					}
					if out.Recommendation.Suggested.Direction == "insufficient_evidence" || out.Recommendation.Suggested.Limit == nil {
						t.Fatalf("suggested=%+v", out.Recommendation.Suggested)
					}
					call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.GetRunnerCapacity, "arguments": map[string]any{"runner_id": runner}, "_meta": json.RawMessage(fleetProtocolMeta)}}
					raw, err := json.Marshal(call)
					if err != nil {
						t.Fatal(err)
					}
					mcp := f.request(t, org, "/api/v2/organizations/"+org+"/mcp", string(raw), sharedRouteBrowserClaims, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.GetRunnerCapacity, "X-CSRF-Token": strings.Repeat("b", 64)})
					requireNativeStatus(t, mcp, http.StatusOK)
					var envelope struct {
						Result struct {
							Data struct {
								Data runnerCapacityRead `json:"data"`
							} `json:"structuredContent"`
							IsError bool `json:"isError"`
						} `json:"result"`
					}
					decodeHubResponse(t, mcp, &envelope)
					if envelope.Result.IsError || !reflect.DeepEqual(envelope.Result.Data.Data, out) {
						t.Fatalf("MCP capacity differs from HTTP: %s", mcp.Body.String())
					}
					if guard.reads.Load() != 0 {
						t.Fatalf("capacity read fetched %d history objects", guard.reads.Load())
					}
					if !reflect.DeepEqual(before, read("/routing")) {
						t.Fatal("capacity read changed runner routing")
					}
					response := f.request(t, org, path+"/capacity", "", func(c *cloudassert.Claims) {
						sharedRouteBrowserClaims(c)
						c.Method = http.MethodGet
						c.Audience = "org_startup"
						if org == "org_startup" {
							c.Audience = "org_other"
						}
						c.Subject = c.Audience + "_owner"
						c.ProviderOrganization = "provider_" + c.Audience
					})
					requireNativeStatus(t, response, http.StatusUnauthorized)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, path+"/capacity", "", nil), http.StatusUnauthorized)
					denied := f.request(t, org, path+"/capacity", "", func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Method = http.MethodGet; c.Role = "viewer" })
					if denied.Code != http.StatusForbidden && denied.Code != http.StatusNotFound {
						t.Fatalf("viewer capacity status=%d body=%s", denied.Code, denied.Body.String())
					}
					if pass == 0 {
						key := runner + "_13"
						for _, test := range []struct{ name, query string }{
							{"missing outcome", "UPDATE report_attempt_facts SET outcome_event_id=NULL WHERE organization_id=$1 AND attempt_id=$2"},
							{"missing projection", "DELETE FROM report_event_facts WHERE organization_id=$1 AND event_id=$2"},
							{"upgraded unknown", "UPDATE report_event_facts SET process_evidence_status='unknown',process_peak_memory_bytes=0,process_user_cpu_seconds=0,process_system_cpu_seconds=0,process_wall_seconds=0 WHERE organization_id=$1 AND event_id=$2"},
						} {
							t.Run("unknown history/"+test.name, func(t *testing.T) {
								if _, err := admin.ExecContext(t.Context(), test.query, org, key); err != nil {
									t.Fatal(err)
								}
								defer func() {
									if err := service.database.postgres.ImportReportEventProjection(t.Context(), events[org][key]); err != nil {
										t.Error(err)
									}
								}()
								var got runnerCapacityRead
								if err := json.Unmarshal(read("/capacity"), &got); err != nil {
									t.Fatal(err)
								}
								if !got.Recommendation.Partial || got.Recommendation.Periods[0].RunsObserved != 10 || got.Recommendation.Periods[0].Slots != nil || got.Recommendation.Suggested.Limit != nil {
									t.Fatalf("10 usable plus unknown produced complete advice: %+v", got.Recommendation)
								}
								unknown := envelope
								unknown.Result.Data.Data = runnerCapacityRead{}
								mcp := f.request(t, org, "/api/v2/organizations/"+org+"/mcp", string(raw), sharedRouteBrowserClaims, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.GetRunnerCapacity, "X-CSRF-Token": strings.Repeat("b", 64)})
								requireNativeStatus(t, mcp, http.StatusOK)
								decodeHubResponse(t, mcp, &unknown)
								advice := unknown.Result.Data.Data.Recommendation
								if unknown.Result.IsError || !advice.Partial || advice.Periods[0].RunsObserved != 10 || advice.Periods[0].Slots != nil || advice.Suggested.Limit != nil {
									t.Fatalf("MCP hid unknown evidence: %+v", unknown.Result)
								}
								var facts reportstore.RunnerCapacityFacts
								if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
									var err error
									facts, err = r.(reportstore.RunnerCapacityReader).ReadRunnerCapacityFacts(t.Context(), reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: tracker.MachineID(runner), From: to.Add(-24 * time.Hour), To: to, Limit: reportstore.RunnerCapacityFactLimit})
									return err
								}); err != nil {
									t.Fatal(err)
								}
								if facts.EligibleRuns != 12 || facts.UnknownRuns != 1 || len(facts.Runs) != 10 {
									t.Fatalf("eligible/unknown/usable counts concealed history: %+v", facts)
								}

							})
						}
						for _, test := range []struct{ name, set, reset string }{
							{"lease", "process_lease_id='foreign'", "process_lease_id=event_id"},
							{"fence", "process_fencing_token=process_fencing_token+1", "process_fencing_token=process_fencing_token-1"},
							{"sequence", "run_sequence=999", "run_sequence=1"},
							{"runner", "process_runner_id='foreign'", "process_runner_id='" + runner + "'"},
							{"machine", "process_machine_id='foreign'", "process_machine_id=''"},
						} {
							t.Run("process owner/"+test.name, func(t *testing.T) {
								key := runner + "_0"
								if _, err := admin.ExecContext(t.Context(), "UPDATE report_event_facts SET "+test.set+" WHERE organization_id=$1 AND event_id=$2", org, key); err != nil {
									t.Fatal(err)
								}
								defer func() {
									if _, err := admin.ExecContext(t.Context(), "UPDATE report_event_facts SET "+test.reset+" WHERE organization_id=$1 AND event_id=$2", org, key); err != nil {
										t.Error(err)
									}
								}()
								var got runnerCapacityRead
								if err := json.Unmarshal(read("/capacity"), &got); err != nil {
									t.Fatal(err)
								}
								if !got.Recommendation.Partial || got.Recommendation.Periods[0].Slots != nil {
									t.Fatalf("foreign owner produced advice: %+v", got.Recommendation)
								}
							})
						}
					}
					err = service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
						facts, err := r.(reportstore.RunnerCapacityReader).ReadRunnerCapacityFacts(t.Context(), reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: tracker.MachineID(runner), From: to.Add(-24 * time.Hour), To: to, Limit: 2})
						if err != nil {
							return err
						}
						if !facts.OccupancyPartial || !facts.RunsPartial || len(facts.Runs) != 2 || len(facts.Occupancy) != 2 {
							t.Fatalf("bounded facts=%+v", facts)
						}
						hours, _ := recommendationFixture()
						clipped, err := runnerRecommendationFromFacts(runnerauth.Routing{CapacityLimit: 4, MachineUse: "dedicated"}, hours, facts)
						if err != nil {
							return err
						}
						if clipped.Suggested.Limit != nil || clipped.Periods[0].Slots != nil {
							t.Fatalf("clipped evidence produced advice=%+v", clipped)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
		if pass == 0 {
			for org, service := range f.services {
				cfg := service.config
				if err := service.Close(); err != nil {
					t.Fatal(err)
				}
				f.services[org] = openTestService(t, cfg)
			}
		}
	}

	for org, service := range f.services {
		for _, protocol := range []string{"HTTP", "MCP"} {
			t.Run(org+"/grant removed during "+protocol, func(t *testing.T) {
				if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE organization_id=$1 AND user_id=$2", org, org+"_owner"); err != nil {
					t.Fatal(err)
				}
				owner := service.config.TenantRepository
				probe := &capacityReadProbeRepository{Repository: service.database.postgres, after: func() {
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner=false WHERE organization_id=$1 AND user_id=$2 AND project_id='project_0'", org, org+"_owner"); err != nil {
						t.Fatal(err)
					}
				}}
				service.config.TenantRepository = probe
				defer func() {
					service.config.TenantRepository = owner
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner=true WHERE organization_id=$1 AND user_id=$2", org, org+"_owner"); err != nil {
						t.Error(err)
					}
					if _, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET role='owner' WHERE organization_id=$1 AND user_id=$2", org, org+"_owner"); err != nil {
						t.Error(err)
					}
				}()
				claims := func(c *cloudassert.Claims) { sharedRouteBrowserClaims(c); c.Role = "member" }
				path := "/api/v2/organizations/" + org
				if protocol == "HTTP" {
					response := f.request(t, org, path+"/runners/runner_0/capacity", "", func(c *cloudassert.Claims) { claims(c); c.Method = http.MethodGet })
					requireNativeStatus(t, response, http.StatusNotFound)
				} else {
					raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.GetRunnerCapacity, "arguments": map[string]any{"runner_id": "runner_0"}, "_meta": json.RawMessage(fleetProtocolMeta)}})
					if err != nil {
						t.Fatal(err)
					}
					response := f.request(t, org, path+"/mcp", string(raw), claims, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.GetRunnerCapacity, "X-CSRF-Token": strings.Repeat("b", 64)})
					requireNativeStatus(t, response, http.StatusOK)
					var result struct {
						Result struct {
							IsError bool `json:"isError"`
						} `json:"result"`
					}
					decodeHubResponse(t, response, &result)
					if !result.Result.IsError || strings.Contains(response.Body.String(), "p90_peak_memory_bytes") {
						t.Fatalf("revoked MCP access leaked capacity: %s", response.Body.String())
					}
				}
				if !probe.fired {
					t.Fatal("grant removal was not interleaved after the capacity facts read")
				}
			})
		}
	}
}

type capacityObjectReadGuard struct {
	attachment.DetailStore
	reads atomic.Int64
}

func (g *capacityObjectReadGuard) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	g.reads.Add(1)
	return nil, artifact.ErrStorage
}

type capacityReadProbeRepository struct {
	*postgres.Repository
	after func()
	fired bool
}

func (p *capacityReadProbeRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, work corestore.Workload, read func(tenantstore.Reader) error) error {
	observed := false
	err := p.Repository.ReadTenant(ctx, org, work, func(r tenantstore.Reader) error {
		return read(&capacityFactsProbe{Reader: r, ProjectAccessPageReader: r.(accessstore.ProjectAccessPageReader), owner: r.(reportstore.RunnerCapacityReader), observed: &observed})
	})
	if err == nil && observed && !p.fired {
		p.fired = true
		p.after()
	}
	return err
}

type capacityFactsProbe struct {
	tenantstore.Reader
	accessstore.ProjectAccessPageReader
	owner    reportstore.RunnerCapacityReader
	observed *bool
}

func (p *capacityFactsProbe) ReadRunnerCapacityFacts(ctx context.Context, q reportstore.RunnerCapacityQuery) (reportstore.RunnerCapacityFacts, error) {
	*p.observed = true
	return p.owner.ReadRunnerCapacityFacts(ctx, q)
}

func accessCredentialForRunner(org tracker.OrganizationID, id string, now time.Time) accessstore.Credential {
	return accessstore.Credential{OrganizationID: org, ID: id, Name: "Example " + id, Hash: apikey.HashToken(string(org) + id), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
}

func TestRunnerRecommendationLeaseHistoryCost(t *testing.T) {
	hours, _ := recommendationFixture()
	from := hours[0].Hour
	cases := []struct {
		name       string
		start, end time.Time
	}{
		{"before window", from.Add(-30 * time.Minute), from.Add(30 * time.Minute)},
		{"late release", from.Add(15 * time.Minute), from.Add(90 * time.Minute)},
		{"early release", from.Add(4 * time.Hour), from.Add(5 * time.Hour)},
		{"expired before window", from.Add(-2 * time.Hour), from.Add(-time.Hour)},
		{"ends at window start", from.Add(-time.Hour), from},
		{"begins at window end", from.Add(24 * time.Hour), from.Add(25 * time.Hour)},
	}
	facts := reportstore.RunnerCapacityFacts{}
	for _, c := range cases {
		facts.Occupancy = append(facts.Occupancy, reportstore.RunnerOccupancy{LeaseID: c.name, From: c.start, To: c.end})
	}
	if _, err := runnerRecommendationFromFacts(runnerauth.Routing{CapacityLimit: 4, MachineUse: "dedicated"}, hours, facts); err != nil {
		t.Fatal(err)
	}
	for i, hour := range hours {
		want := float64(0)
		switch i {
		case 0:
			want = 1.25
		case 1:
			want = .5
		case 4:
			want = 1
		}
		if hour.Slots != want {
			t.Fatalf("hour %d slot-hours=%v,want %v", i, hour.Slots, want)
		}
	}
}
