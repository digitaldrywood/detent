package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimAuditWorkloadRepository struct {
	tenantstore.Repository
	workloads []corestore.Workload
}

func (r *claimAuditWorkloadRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.workloads = append(r.workloads, workload)
	return r.Repository.ReadTenant(ctx, org, workload, read)
}

func TestClaimMergingHydrationHasFixedSnapshotCost(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config := postgresStartupTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	config.MaxConns = 3
	var exchanges atomic.Int64
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &nativeLeaseWireConn{Conn: conn, exchanges: &exchanges, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{}
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: details, SecretKeys: secretTestKeys(t, "1", "1"), now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	var baselineExchanges, baselineStatements int64
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
		descriptor := hubTestPolicy()
		policy, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policy.ProjectID = "example"
		policy.ApprovedBy = "test_hub_admin"
		policy.ApprovedAt = now
		reviewPolicy, err := content.PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: org, ProjectID: "example", PolicyRevisionID: descriptor.ID, Revision: 1, UpdatedBy: "test_hub_admin", UpdatedAt: now}, defaultChangeReviewPolicy(descriptor))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: append(nativeFixtureStates(), tracker.NativeState{Name: "Merging", Dispatchable: true})}); err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), policy, ""); err != nil {
				return err
			}
			return w.SetReviewPolicy(t.Context(), reviewPolicy, "")
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: org, project: "example", credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		for index := range 100 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
			changeID := fmt.Sprintf("change_%03d", index)
			versionID := fmt.Sprintf("version_%03d", index)
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Merging", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, CreatedAt: now, UpdatedAt: now}
			raw, _ := json.Marshal(issue)
			collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now}
			if index == 0 {
				change.CreatedAt = now.Add(time.Microsecond)
			}
			raw, _ = json.Marshal(change)
			changeRow, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: descriptor, Actor: issue.Actor, CreatedAt: now}
			raw, _ = json.Marshal(version)
			versionRow, err := content.PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			run := securityaudit.Run{ID: int64(index + 1), ProjectID: "example", Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, Findings: []securityaudit.Finding{{ID: "finding", Body: "Example", Certainty: securityaudit.CertaintyConfirmed}}}
			review := tracker.ChangeReview{ID: "review", VersionID: versionID, Decision: "changes_requested", Actor: issue.Actor, CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: run.ID, Run: run, VersionID: versionID}}
			raw, _ = json.Marshal(review)
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, raw)
			if err != nil {
				t.Fatal(err)
			}
			disposition := securityaudit.Disposition{AuditRunID: run.ID, FindingID: "finding", Status: securityaudit.DispositionFalsePositive, ServiceIdentity: securityaudit.ServiceIdentity("example"), RecordedAt: now, Evidence: "Example source evidence"}
			raw, _ = json.Marshal(disposition)
			ref, err := details.StoreDetail(t.Context(), string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: issue.Title, Revision: 1, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return fmt.Errorf("CreateWorkItem: %w", err)
				}
				if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: "example", WorkItemID: id}); err != nil {
					return err
				}
				if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: org, ProjectID: "example", WorkItemID: id, Scope: "example", State: "Merging", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
					return fmt.Errorf("CreateCollaborationVersion: %w", err)
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return fmt.Errorf("CreateChange: %w", err)
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return fmt.Errorf("CreateVersion: %w", err)
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
					return fmt.Errorf("LinkIssue: %w", err)
				}
				evidence, err = w.AppendEvidence(t.Context(), evidence)
				if err != nil {
					return fmt.Errorf("append evidence: %w", err)
				}
				_, err = w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), eventstore.AuditDisposition{Organization: org, Project: "example", ChangeID: changeID, ReviewSequence: evidence.Sequence, ActorID: "test_hub_admin", RequestKey: changeID, RequestHash: "example", Reference: ref, Disposition: disposition})
				if err != nil {
					return fmt.Errorf("disposition: %w", err)
				}
				if index == 0 {
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "publication-machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
						return err
					}
					item, err := w.WorkItem(t.Context(), "example", id)
					if err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "publication-machine", SessionID: "publication-session", LeaseID: "publication-lease", TTL: time.Minute})
					if err != nil {
						return err
					}
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "publication-attempt", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "publication-run", Sequence: 1, Status: "succeeded", StartedAt: now, PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
						return err
					}
					if err := w.SetAttemptClaimFacts(t.Context(), leasestore.AttemptClaimFacts{AttemptID: "publication-attempt", Sequence: 1, Status: "succeeded", PayloadObjectKey: ref.Key, Disposition: "progress"}); err != nil {
						return err
					}
					if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "publication-complete"); err != nil {
						return err
					}
					if err := w.PutPRPublication(t.Context(), eventstore.NativePRPublication{OrganizationID: org, VersionID: versionID, ChangeID: changeID, Repository: input.Repository, BaseRef: "develop", Branch: "example-change", HeadSHA: input.HeadSHA, PolicyID: descriptor.ID, RecorderAttemptID: "publication-attempt", External: tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: input.Repository + "/pull/1"}}); err != nil {
						return err
					}
					for _, kind := range []string{"run.observed", "workflow.transitioned"} {
						data := tracker.CollaborationData{Revision: 1}
						if kind == "run.observed" {
							data.Run = &tracker.NativeRunData{AttemptID: "publication-attempt", Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Waiting: true, ChangeID: changeID, VersionID: versionID, HeadSHA: input.HeadSHA}}}
						} else {
							data.Change = &tracker.NativeChangeReference{ChangeID: changeID, VersionID: versionID, HeadSHA: input.HeadSHA}
							data.FromState = "Merging"
							data.ToState = "Done"
						}
						raw, err := json.Marshal(tracker.CollaborationEvent{ID: kind, OrganizationID: org, ProjectID: "example", AggregateID: id, Type: kind, SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: data})
						if err != nil {
							return err
						}
						event, err := content.PrepareEvent(t.Context(), org, raw)
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if index != 1 && index != 49 && index != 99 {
				continue
			}
			request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkflowStates: []string{"Merging"}}
			scan, err := service.readNativeClaimCandidates(t.Context(), scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.plans) != index+1 {
				t.Fatalf("candidates=%d want %d", len(scan.plans), index+1)
			}
			legacy := scan.plans[0]
			legacy.changeFacts = nil
			expected, err := service.hydrateCurrentChangeDetail(t.Context(), scope, legacy.candidate.WorkItemID, legacy.changeRecord, &legacy.changeVersion, legacy.changePolicy, now)
			if err != nil {
				t.Fatal(err)
			}
			var statements int64
			queries := map[string]string{}
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
				statements++
				if strings.Contains(query, "-- name: ClaimSnapshotRows ") {
					queries["ClaimSnapshot"] = query
				}
			})
			exchanges.Store(0)
			scan, err = service.readNativeClaimCandidates(ctx, scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			afterSnapshotStatements, afterSnapshotExchanges := statements, exchanges.Load()
			for _, plan := range scan.plans {
				hydrated, err := service.hydrateNativeClaimPlan(ctx, scope, request, now, plan, &descriptor)
				if err != nil {
					t.Fatal(err)
				}
				if hydrated.changeRecord.OrganizationID != org {
					t.Fatal("cross tenant change")
				}
			}
			if statements != afterSnapshotStatements || exchanges.Load() != afterSnapshotExchanges {
				t.Fatal("claim hydration performed PostgreSQL work")
			}
			got := exchanges.Load()
			detail, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, scan.plans[0], now)
			if err != nil || !reflect.DeepEqual(detail, expected) {
				got, _ := json.Marshal(detail)
				want, _ := json.Marshal(expected)
				t.Logf("snapshot_got=%s", got)
				t.Logf("snapshot_want=%s", want)
				t.Fatalf("snapshot changed validated detail: %v", err)
			}
			if index == 49 || index == 99 {
				t.Run(string(org)+"/bounded observation plans", func(t *testing.T) {
					noise := make([]eventstore.Event, 128)
					for i := range noise {
						raw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: org, ProjectID: scope.project, AggregateID: "wi_000", Type: "issue.edited", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now})
						if err != nil {
							t.Fatal(err)
						}
						noise[i], err = content.PrepareEvent(t.Context(), org, raw)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						for _, row := range noise {
							if _, err := w.AppendEvent(t.Context(), row); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					literal := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

					ids, revisions := []string{}, []string{}
					for _, plan := range scan.plans {
						ids = append(ids, fmt.Sprintf("%d", plan.candidate.ID))
						revisions = append(revisions, fmt.Sprintf("%d", plan.candidate.Revision))
					}
					for _, test := range []struct {
						name, types, arguments string
						candidates             int
						populated              bool
					}{
						{"ClaimSnapshot", "text,text,bigint[],bigint[]", strings.Join([]string{literal(string(org)), literal(string(scope.project)), "ARRAY[" + strings.Join(ids, ",") + "]::bigint[]", "ARRAY[" + strings.Join(revisions, ",") + "]::bigint[]"}, ","), len(scan.plans), false},
						{"ClaimSnapshotPopulated", "text,text,bigint[],bigint[]", strings.Join([]string{literal(string(org)), literal(string(scope.project)), "ARRAY[" + ids[0] + "]::bigint[]", "ARRAY[" + revisions[0] + "]::bigint[]"}, ","), 1, true},
					} {
						t.Run(test.name, func(t *testing.T) {
							query := queries["ClaimSnapshot"]
							if query == "" {
								t.Fatal("version observation query was not captured")
							}
							tx, err := admin.BeginTx(t.Context(), nil)
							if err != nil {
								t.Fatal(err)
							}
							defer tx.Rollback()
							for _, statement := range []string{"SET LOCAL ROLE detent_runtime_v2", "SET LOCAL plan_cache_mode=force_generic_plan", "SELECT set_config('detent.organization_id'," + literal(string(org)) + ",true)", "PREPARE claim_observations(" + test.types + ") AS " + query} {
								if _, err := tx.ExecContext(t.Context(), statement); err != nil {
									t.Fatal(err)
								}
							}
							defer func() {
								if _, err := tx.ExecContext(t.Context(), "DEALLOCATE claim_observations"); err != nil {
									t.Error(err)
								}
							}()
							var raw []byte
							if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE claim_observations("+test.arguments+")").Scan(&raw); err != nil {
								t.Fatal(err)
							}
							type queryPlan struct {
								Relation  string  `json:"Relation Name"`
								Rows      float64 `json:"Actual Rows"`
								Removed   float64 `json:"Rows Removed by Filter"`
								Rechecked float64 `json:"Rows Removed by Index Recheck"`
								Loops     float64 `json:"Actual Loops"`
								Plans     []queryPlan
							}
							var plans []struct{ Plan queryPlan }
							if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
								t.Fatalf("decode claim observation plan: %v", err)
							}
							if plans[0].Plan.Rows != float64(test.candidates) {
								t.Fatalf("claim plan returned %.0f candidates, want %d", plans[0].Plan.Rows, test.candidates)
							}
							var visits float64
							relations := map[string]float64{}
							var inspect func(queryPlan)
							inspect = func(plan queryPlan) {
								if plan.Relation != "" && plan.Rows+plan.Removed+plan.Rechecked > 1 {
									t.Errorf("claim lookup visits unrelated rows: %+v", plan)
								}
								if plan.Relation != "" {
									visits += (plan.Rows + plan.Removed + plan.Rechecked) * plan.Loops
									relations[plan.Relation] += plan.Rows * plan.Loops
								}
								for _, child := range plan.Plans {
									inspect(child)
								}
							}
							inspect(plans[0].Plan)
							if visits > float64(test.candidates*16) {
								t.Errorf("version observations visited %.0f rows for %d candidates: %s", visits, test.candidates, raw)
							}
							if test.populated {
								for _, relation := range []string{"native_attempts", "collaboration_events", "native_pr_publications", "change_review_policies", "change_review_policy_revisions"} {
									if relations[relation] == 0 {
										t.Errorf("populated observation plan never visited %s: %s", relation, raw)
									}
								}
							}
							t.Logf("candidate_count=%d row_visits=%.0f observation_visits=%v plan=%s", test.candidates, visits, relations, raw)
						})
					}
				})
				observations := scan.plans[0].changeFacts.VersionObservations
				if observations == nil || observations.Publication == nil || observations.Landing == nil || detail.Versions[0].External == nil || detail.Versions[0].Landing == nil || detail.Versions[0].Landing.TargetState != "Done" {
					t.Fatalf("captured matching receipts lost: %+v", detail)
				}
				if absent := scan.plans[1].changeFacts.VersionObservations; absent == nil || absent.Publication != nil || absent.Landing != nil {
					t.Fatalf("known absence=%+v", absent)
				}
				for _, test := range []struct {
					name     string
					mutate   func(*nativeClaimPlan)
					want     error
					notReady bool
				}{
					{"links overflow", func(p *nativeClaimPlan) {
						facts := *p.changeFacts
						facts.Links = make([]tenantstore.BoardChangeLink, 33)
						p.changeFacts = &facts
					}, corestore.ErrInvalidPage, false},
					{"evidence overflow", func(p *nativeClaimPlan) {
						facts := *p.changeFacts
						facts.Evidence = make([]eventstore.Evidence, corestore.MaxPageSize+1)
						p.changeFacts = &facts
					}, corestore.ErrInvalidPage, false},
					{"missing snapshot", func(p *nativeClaimPlan) { p.snapshotLoaded = false }, nil, true},
					{"missing required disposition", func(p *nativeClaimPlan) {
						p.candidate.State, p.landingPending = "Todo", false
						p.latestAttempt.ClaimDisposition = nil
					}, nil, true},
					{"loaded required disposition", func(p *nativeClaimPlan) {
						p.candidate.State, p.landingPending = "Todo", false
					}, nil, false},
				} {
					t.Run(string(org)+"/"+test.name, func(t *testing.T) {
						plan := scan.plans[0]
						if plan.latestAttempt.ClaimDisposition == nil || *plan.latestAttempt.ClaimDisposition != "progress" {
							t.Fatal("snapshot lost the persisted claim disposition")
						}
						test.mutate(&plan)
						if strings.Contains(test.name, "required disposition") && !nativeClaimAnswerRequired(plan) {
							t.Fatal("case does not require a typed claim disposition")
						}
						_, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, plan, &descriptor)
						if test.notReady {
							var refusal *nativeError
							if !errors.As(err, &refusal) || refusal.Code != "not_ready" {
								t.Fatalf("claim accepted unavailable facts: %v", err)
							}
						} else if !errors.Is(err, test.want) {
							t.Fatalf("claim hydration error=%v want=%v", err, test.want)
						}
					})
				}
				for _, tc := range []struct {
					name   string
					mutate func(*tenantstore.ChangeVersionRows)
					want   error
				}{
					{"other item", func(r *tenantstore.ChangeVersionRows) { r.Request.WorkItemID = "wi_001" }, corestore.ErrRevisionConflict},
					{"other head", func(r *tenantstore.ChangeVersionRows) { r.Request.Reference.HeadSHA = "other" }, corestore.ErrRevisionConflict},
					{"other project", func(r *tenantstore.ChangeVersionRows) { r.ProjectID = "other" }, corestore.ErrRevisionConflict},
					{"foreign publication", func(r *tenantstore.ChangeVersionRows) {
						p := *r.Publication
						p.OrganizationID = "other"
						r.Publication = &p
					}, corestore.ErrTenantMismatch},
					{"foreign landing", func(r *tenantstore.ChangeVersionRows) {
						l := *r.Landing
						l.Event.OrganizationID = "other"
						r.Landing = &l
					}, corestore.ErrTenantMismatch},
					{"publication source mismatch", func(r *tenantstore.ChangeVersionRows) {
						p := *r.Publication
						p.SourceAttemptID = "other"
						r.Publication = &p
					}, nil},
				} {
					t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
						captured := *observations
						tc.mutate(&captured)
						version, err := service.hydrateChangeVersionWithRows(t.Context(), scope, scan.plans[0].candidate.WorkItemID, scan.plans[0].changeVersion, &captured)
						if !errors.Is(err, tc.want) {
							t.Fatalf("hydrate error=%v, want %v", err, tc.want)
						}
						if tc.want == nil && version.External != nil {
							t.Fatal("publication source mismatch was accepted")
						}
					})
				}
				func() {
					original := service.config.TenantRepository
					counter := &claimAuditWorkloadRepository{Repository: repository}
					service.config.TenantRepository = counter
					defer func() { service.config.TenantRepository = original }()
					for _, tc := range []struct {
						name                          string
						workload                      corestore.Workload
						captured, credential, allowed bool
					}{
						{"background_projection", corestore.Background, false, false, true},
						{"serving_authorized", corestore.Serving, false, true, true},
						{"serving_denied", corestore.Serving, false, false, false},
						{"captured_claim", corestore.Serving, true, true, true},
					} {
						t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
							plan := scan.plans[0]
							facts := *plan.changeFacts
							if !tc.captured {
								facts.AuditDispositions = nil
							}
							hydrateScope := scope
							if !tc.credential {
								hydrateScope.credential = apiCredential{}
							}
							counter.workloads = nil
							got, err := service.hydrateCurrentChangeDetailWithFacts(t.Context(), hydrateScope, plan.candidate.WorkItemID, plan.changeRecord, &plan.changeVersion, plan.changePolicy, now, &facts, nil, tc.workload)
							if tc.allowed {
								if err != nil || !reflect.DeepEqual(got, expected) {
									t.Errorf("workload-aware detail differs: %v", err)
								}
							} else if err == nil {
								t.Error("serving hydration bypassed credential authorization")
							}
							if tc.captured {
								if len(counter.workloads) != 0 {
									t.Errorf("captured claim opened %d read transactions", len(counter.workloads))
								}
							} else if !reflect.DeepEqual(counter.workloads, []corestore.Workload{tc.workload}) {
								t.Errorf("audit workloads=%v want %v", counter.workloads, tc.workload)
							}
						})
					}
				}()
				if index == 99 {
					t.Run(string(org)+"/pending landing intent", func(t *testing.T) {
						current := scan.plans[0]
						if current.candidate.WorkItemID != "wi_000" || !current.latestAttemptFound || !current.latestRunEventFound || !current.latestRunEvent.LandingWaiting || current.latestAttempt.WorkItemRevision != current.item.Revision || current.latestAttempt.DispatchGeneration != current.item.DispatchGeneration || !current.landingPending {
							t.Fatalf("current waiting landing is not pending: item=%s attempt_revision=%d item_revision=%d pending=%t", current.candidate.WorkItemID, current.latestAttempt.WorkItemRevision, current.item.Revision, current.landingPending)
						}
						merging, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, current, &descriptor)
						if err != nil {
							t.Fatal(err)
						}
						waiting := current
						waiting.candidate.State = "Todo"
						continuation, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, waiting, &descriptor)
						if err != nil {
							t.Fatal(err)
						}
						if !continuation.landingPending || continuation.changeReady != merging.changeReady || continuation.landingRequired != merging.landingRequired {
							t.Fatalf("waiting continuation left the canonical landing path: pending=%t ready=%t want=%t capacity=%t want=%t", continuation.landingPending, continuation.changeReady, merging.changeReady, continuation.landingRequired, merging.landingRequired)
						}
						edited := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_000", Number: 1, Revision: tracker.Revision(current.item.Revision + 1)}, Title: "Edited example", State: "Merging", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, CreatedAt: now, UpdatedAt: now}
						raw, err := json.Marshal(edited)
						if err != nil {
							t.Fatal(err)
						}
						version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_000", RecordID: "wi_000", Revision: current.item.Revision + 1}, raw)
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), "example", "wi_000")
							if err != nil {
								return err
							}
							item.Title = edited.Title
							if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
								return err
							}
							_, err = w.CreateCollaborationVersion(t.Context(), version)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						rescan, err := service.readNativeClaimCandidates(t.Context(), scope, request, now)
						if err != nil {
							t.Fatal(err)
						}
						var stale nativeClaimPlan
						for _, plan := range rescan.plans {
							if plan.candidate.WorkItemID == "wi_000" {
								stale = plan
							}
						}
						if stale.item.Revision != current.item.Revision+1 || stale.latestAttempt.ID != current.latestAttempt.ID || stale.latestAttempt.WorkItemRevision != current.latestAttempt.WorkItemRevision || stale.latestRunEvent.ID != current.latestRunEvent.ID || !stale.latestRunEvent.LandingWaiting {
							t.Fatalf("edit did not leave the recorded waiting attempt behind the item: item_revision=%d attempt=%s attempt_revision=%d event=%s", stale.item.Revision, stale.latestAttempt.ID, stale.latestAttempt.WorkItemRevision, stale.latestRunEvent.ID)
						}
						if stale.landingPending {
							t.Fatalf("waiting receipt of attempt revision %d is still pending for item revision %d", stale.latestAttempt.WorkItemRevision, stale.item.Revision)
						}
						canonical, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, stale, &descriptor)
						if err != nil {
							t.Fatal(err)
						}
						if canonical.landingPending || canonical.changeReady != merging.changeReady || canonical.landingRequired != merging.landingRequired {
							t.Fatalf("stale waiting receipt changed the Merging path: pending=%t ready=%t want=%t capacity=%t want=%t", canonical.landingPending, canonical.changeReady, merging.changeReady, canonical.landingRequired, merging.landingRequired)
						}
						outside := stale
						outside.candidate.State = "Todo"
						dispatched, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, outside, &descriptor)
						if err != nil {
							t.Fatal(err)
						}
						if dispatched.landingPending || dispatched.changeReady || dispatched.landingRequired {
							t.Fatalf("stale waiting receipt routed the edited item to landing: pending=%t ready=%t capacity=%t", dispatched.landingPending, dispatched.changeReady, dispatched.landingRequired)
						}
					})
				}
			}
			t.Logf("tenant=%s candidates=%d statements=%d exchanges=%d", org, index+1, statements, got)
			if baselineExchanges == 0 {
				baselineExchanges = got
				baselineStatements = statements
			}
			if got > 10 || statements > 9 || got != baselineExchanges || statements != baselineStatements {
				t.Errorf("snapshot cost grew: statements=%d exchanges=%d; baseline=%d/%d", statements, got, baselineStatements, baselineExchanges)
			}
		}
		input := changeTestInput()
		for index := range 50 {
			id := int64(1000 + index)
			if index == 49 {
				id = 501
			}
			versionID := fmt.Sprintf("version_%03d", index)
			run := securityaudit.Run{ID: id, ProjectID: "example", Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
			review := tracker.ChangeReview{ID: "legacy-run", VersionID: versionID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: id, VersionID: versionID, Run: run}}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: fmt.Sprintf("change_%03d", index), Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			evidence.ReviewKind = ""
			evidence.ReviewSessionKnown = false
			evidence.ReviewSessionID = 0
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), evidence); return err }); err != nil {
				t.Fatal(err)
			}
		}

		disposition := securityaudit.Disposition{AuditRunID: 501, FindingID: "foreign", Status: securityaudit.DispositionFalsePositive, ServiceIdentity: securityaudit.ServiceIdentity("example"), RecordedAt: now}
		raw, _ := json.Marshal(disposition)
		ref, err := details.StoreDetail(t.Context(), string(org), raw)
		if err != nil {
			t.Fatal(err)
		}
		request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: "wi_049", WorkflowStates: []string{"Merging"}}
		scalar, err := service.readNativeClaimPlanRows(t.Context(), scope, request, now, nil, nil, nativeClaimPlan{})
		if err != nil {
			t.Fatal(err)
		}
		scalar.changeFacts = nil
		expected, err := service.hydrateCurrentChangeDetail(t.Context(), scope, scalar.candidate.WorkItemID, scalar.changeRecord, &scalar.changeVersion, scalar.changePolicy, now)
		if err != nil {
			t.Fatalf("scalar legacy audit without dispositions: %v", err)
		}
		actual, err := service.readNativeClaimPlan(t.Context(), scope, request, now)
		if err != nil {
			t.Errorf("batch legacy audit without dispositions tenant=%s: %v", org, err)
		} else {
			detail, err := service.nativeClaimCurrentChangeDetail(t.Context(), scope, actual, now)
			if err != nil || !reflect.DeepEqual(detail, expected) {
				t.Errorf("legacy audit detail changed: %v", err)
			}
		}
		var legacyExchanges, legacyStatements int64
		for _, size := range []int{2, 50, 100} {
			pageRequest := tracker.NativeClaim{PolicyID: descriptor.ID, WorkflowStates: []string{"Merging"}}
			for offset := range size {
				index := offset
				if offset == size-1 {
					index = 99
				}
				pageRequest.ProviderCandidates = append(pageRequest.ProviderCandidates, tracker.NativeCapacityCandidate{WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))})
			}
			var statements int64
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(time.Time, string) { statements++ })
			exchanges.Store(0)
			scan, err := service.readNativeClaimCandidates(ctx, scope, pageRequest, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(scan.plans) != size {
				t.Fatalf("legacy candidates=%d want %d", len(scan.plans), size)
			}
			for _, plan := range scan.plans {
				if _, err := service.hydrateNativeClaimPlan(ctx, scope, pageRequest, now, plan, &descriptor); err != nil {
					t.Errorf("legacy page hydration tenant=%s size=%d: %v", org, size, err)
				}
			}
			got := exchanges.Load()
			t.Logf("legacy tenant=%s candidates=%d statements=%d exchanges=%d", org, size, statements, got)
			if size == 2 {
				legacyExchanges, legacyStatements = got, statements
			} else if got > 14 || statements > 12 || got != legacyExchanges || statements != legacyStatements {
				t.Errorf("legacy snapshot cost grew: %d/%d versus %d/%d", statements, got, legacyStatements, legacyExchanges)
			}
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(t.Context(), eventstore.AuditDisposition{Organization: org, Project: "example", ChangeID: "change_000", ReviewSequence: 1, ActorID: "test_hub_admin", RequestKey: "foreign-run", RequestHash: "example", Reference: ref, Disposition: disposition})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.hydrateCurrentChangeDetail(t.Context(), scope, scalar.candidate.WorkItemID, scalar.changeRecord, &scalar.changeVersion, scalar.changePolicy, now); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Errorf("scalar legacy foreign disposition: %v", err)
		}
		if _, err := service.readNativeClaimPlan(t.Context(), scope, request, now); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("foreign audit disposition outside candidate page: %v", err)
		}

		for index, tc := range []struct {
			name       string
			reviewKind string
			corrupt    func(*eventstore.Evidence)
		}{
			{"kind", "implementation", func(row *eventstore.Evidence) { row.ReviewKind = "human" }},
			{"landing", "security_audit", func(row *eventstore.Evidence) { row.ReviewLanding = true }},
			{"session_id", "validator", func(row *eventstore.Evidence) { row.ReviewSessionID++ }},
			{"session_machine", "implementation", func(row *eventstore.Evidence) { row.ReviewSessionMachine = "machine_other" }},
		} {
			t.Run(string(org)+"/typed_"+tc.name, func(t *testing.T) {
				changeID, versionID := fmt.Sprintf("change_%03d", index), fmt.Sprintf("version_%03d", index)
				sessionID := int64(800 + index)
				review := tracker.ChangeReview{ID: "provenance_" + tc.name, VersionID: versionID, Decision: "approved", CreatedAt: now, SessionMachineID: "machine_expected"}
				switch tc.reviewKind {
				case "implementation":
					review.ImplementationReview = &gate.ValidatorResult{SessionID: sessionID, VersionID: versionID}
				case "validator":
					review.Validator = &gate.ValidatorResult{SessionID: sessionID, VersionID: versionID}
				case "security_audit":
					review.SecurityAudit = &tracker.SecurityAuditReview{SessionID: sessionID, VersionID: versionID, Run: securityaudit.Run{ID: sessionID, ProjectID: "example"}}
				}
				raw, err := json.Marshal(review)
				if err != nil {
					t.Fatal(err)
				}
				evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, raw)
				if err != nil {
					t.Fatal(err)
				}
				tc.corrupt(&evidence)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), evidence); return err }); err != nil {
					t.Fatal(err)
				}
				request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index)), WorkflowStates: []string{"Merging"}}
				plan, err := service.readNativeClaimPlanRows(t.Context(), scope, request, now, nil, nil, nativeClaimPlan{})
				if err != nil {
					t.Fatal(err)
				}
				scalar := plan
				scalar.changeFacts = nil
				if _, err := service.hydrateCurrentChangeDetail(t.Context(), scope, scalar.candidate.WorkItemID, scalar.changeRecord, &scalar.changeVersion, scalar.changePolicy, now); !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("scalar accepted typed %s mismatch: %v", tc.name, err)
				}
				if _, err := service.hydrateNativeClaimPlan(t.Context(), scope, request, now, plan, &descriptor); !errors.Is(err, artifact.ErrIntegrity) {
					t.Errorf("batch accepted typed %s mismatch: %v", tc.name, err)
				}
			})
		}

		t.Run(string(org)+"/linked item observations", func(t *testing.T) {
			reference := tracker.NativeChangeReference{ChangeID: "change_000", VersionID: "version_000", HeadSHA: input.HeadSHA}
			data := tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "linked-attempt", Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ChangeID: reference.ChangeID, VersionID: reference.VersionID, HeadSHA: reference.HeadSHA, MergeSHA: "linked-merge"}}}}
			raw, err := json.Marshal(tracker.CollaborationEvent{ID: "linked-landing", OrganizationID: org, ProjectID: "example", AggregateID: "wi_001", Type: "run.observed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, RecordedAt: now, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_001", ChangeID: reference.ChangeID, CreatedAt: now}); err != nil {
					return err
				}
				_, err := w.AppendEvent(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			requests := []tenantstore.ChangeVersionRequest{{WorkItemID: "wi_000", Reference: reference}, {WorkItemID: "wi_001", Reference: reference}, {WorkItemID: "wi_049", Reference: reference}}
			var rows []tenantstore.ChangeVersionRows
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				rows, err = r.(tenantstore.ChangeVersionRowsReader).ChangeVersionRows(t.Context(), "example", requests)
				if err != nil {
					return err
				}
				candidates := make([]claimstore.Candidate, 2)
				for i := range candidates {
					item, err := r.WorkItemByNativeID(t.Context(), requests[i].WorkItemID)
					if err != nil {
						return err
					}
					candidates[i] = claimstore.Candidate{OrganizationID: org, ProjectID: "example", ID: item.ID, WorkItemID: item.NativeID, Revision: item.Revision}
				}
				joined, err := r.(tenantstore.ClaimPreviewPlanRowsReader).ClaimPreviewPlanRows(t.Context(), "example", candidates)
				if err != nil {
					return err
				}
				if len(joined) != 2 || len(rows) != 3 {
					return fmt.Errorf("shared change rows joined=%d observations=%d", len(joined), len(rows))
				}
				for i, row := range joined {
					if row.Err != nil || row.Observations == nil || row.Observations.Request != requests[i] || row.Observations.Landing == nil || row.Observations.Publication == nil || rows[i].Landing == nil || row.Observations.Landing.Event.AttemptID != rows[i].Landing.Event.AttemptID {
						return fmt.Errorf("joined shared change lost item %d observation: %+v", i, row)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 || rows[0].Landing == nil || rows[1].Landing == nil || rows[2].Landing != nil {
				t.Fatalf("item-specific observations=%+v", rows)
			}
			if rows[0].Landing.Event.AttemptID != "publication-attempt" || rows[1].Landing.Event.AttemptID != "linked-attempt" {
				t.Fatal("shared change reused another item's landing observation")
			}
			for i, row := range rows {
				if row.Request != requests[i] || row.Publication == nil || row.Publication.VersionID != reference.VersionID {
					t.Fatalf("request %d lost publication or identity: %+v", i, row)
				}
			}
		})
		t.Run(string(org)+"/waiting landing dispatch", func(t *testing.T) {
			binding := runnerauth.NewBinding()
			credentialID, enrollmentID := "credential_"+binding.RunnerID, "enrollment_"+binding.RunnerID
			tokenHash := apikey.HashToken("example_runner_" + string(org))
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}
			expires := now.Add(time.Hour)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credentialID, ProjectID: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: enrollmentID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("example_enrollment_" + string(org)), Scope: "projects", ProjectIDs: []tracker.ProjectID{"example"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, binding.RunnerID, binding.MachineID, now); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: string(binding.MachineID), Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: operations, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"example"}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
				return w.UpdateRunnerCapacityConfig(t.Context(), binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			runner := nativeScope{organization: org, project: "example", credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: binding, OrganizationID: org}}}
			for index, seed := range []struct {
				name                      string
				blocked, edit, redispatch bool
				pending, ready, claimed   bool
			}{
				{name: "ready current", pending: true, ready: true, claimed: true},
				{name: "blocked current", blocked: true, pending: true},
				{name: "blocked edited", blocked: true, edit: true, claimed: true},
				{name: "blocked redispatched", blocked: true, redispatch: true, claimed: true},
			} {
				t.Run(seed.name, func(t *testing.T) {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_intent_%d", index))
					changeID, versionID := fmt.Sprintf("change_intent_%d", index), fmt.Sprintf("version_intent_%d", index)
					attemptID, leaseID := fmt.Sprintf("intent-attempt-%d", index), fmt.Sprintf("intent-lease-%d", index)
					issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: 200 + index, Revision: 1}, Title: "Example", State: "In Progress", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, CreatedAt: now, UpdatedAt: now}
					raw, err := json.Marshal(issue)
					if err != nil {
						t.Fatal(err)
					}
					collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
					if err != nil {
						t.Fatal(err)
					}
					change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now}
					raw, err = json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					changeRow, err := content.PrepareChange(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					published := changeTestInput()
					published.PolicyID = descriptor.ID
					version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: published, Policy: descriptor, ReviewPolicy: defaultChangeReviewPolicy(descriptor), Actor: issue.Actor, CreatedAt: now}
					raw, err = json.Marshal(version)
					if err != nil {
						t.Fatal(err)
					}
					versionRow, err := content.PrepareVersion(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					var evidence eventstore.Evidence
					if seed.blocked {
						run := securityaudit.Run{ID: int64(9000 + index), ProjectID: "example", Repository: published.Repository, BaseSHA: published.BaseSHA, HeadSHA: published.HeadSHA, Findings: []securityaudit.Finding{{ID: "finding", Body: "Example", Certainty: securityaudit.CertaintyConfirmed}}}
						review := tracker.ChangeReview{ID: "review", VersionID: versionID, Decision: "changes_requested", Actor: issue.Actor, CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: run.ID, Run: run, VersionID: versionID}}
						raw, err = json.Marshal(review)
						if err != nil {
							t.Fatal(err)
						}
						evidence, err = content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, raw)
						if err != nil {
							t.Fatal(err)
						}
					}
					waiting := tracker.NativeRunData{AttemptID: attemptID, Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Waiting: true, ChangeID: changeID, VersionID: versionID, HeadSHA: published.HeadSHA}}}
					raw, err = json.Marshal(waiting)
					if err != nil {
						t.Fatal(err)
					}
					payload, err := details.StoreDetail(t.Context(), string(org), raw)
					if err != nil {
						t.Fatal(err)
					}
					raw, err = json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("intent-observed-%d", index), OrganizationID: org, ProjectID: "example", AggregateID: id, Type: "run.observed", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: now, Data: tracker.CollaborationData{Revision: 1, Run: &waiting}})
					if err != nil {
						t.Fatal(err)
					}
					observed, err := content.PrepareEvent(t.Context(), org, raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(200 + index), Title: issue.Title, Revision: 1, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
							return err
						}
						if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: "example", WorkItemID: id}); err != nil {
							return err
						}
						if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: org, ProjectID: "example", WorkItemID: id, Scope: "example", State: "In Progress", CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
							return err
						}
						if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
							return err
						}
						if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
							return err
						}
						if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
							return err
						}
						if seed.blocked {
							if _, err := w.AppendEvidence(t.Context(), evidence); err != nil {
								return err
							}
						}
						item, err := w.WorkItem(t.Context(), "example", id)
						if err != nil {
							return err
						}
						lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "publication-machine", SessionID: fmt.Sprintf("intent-session-%d", index), LeaseID: leaseID, TTL: time.Minute})
						if err != nil {
							return err
						}
						if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: fmt.Sprintf("intent-run-%d", index), Sequence: 1, Status: "succeeded", StartedAt: now, PayloadObjectKey: payload.Key, PayloadReference: payload}); err != nil {
							return err
						}
						if err := w.SetAttemptClaimFacts(t.Context(), leasestore.AttemptClaimFacts{AttemptID: attemptID, Sequence: 1, Status: "succeeded", PayloadObjectKey: payload.Key, Disposition: "progress"}); err != nil {
							return err
						}
						if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "intent-complete"); err != nil {
							return err
						}
						_, err = w.AppendEvent(t.Context(), observed)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if seed.edit {
						issue.Revision, issue.Title = 2, "Edited example"
						raw, err = json.Marshal(issue)
						if err != nil {
							t.Fatal(err)
						}
						edited, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 2}, raw)
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), "example", id)
							if err != nil {
								return err
							}
							item.Title = issue.Title
							if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
								return err
							}
							_, err = w.CreateCollaborationVersion(t.Context(), edited)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if seed.redispatch {
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), "example", id)
							if err != nil {
								return err
							}
							attempt, err := w.LatestAttempt(t.Context(), item.ID)
							if err != nil {
								return err
							}
							dispatch, ok := w.(tenantstore.DispatchWriter)
							if !ok {
								return errors.New("dispatch owner is unavailable")
							}
							return dispatch.RequestIssueDispatch(t.Context(), "example", id, leasestore.Fence{LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}, now)
						}); err != nil {
							t.Fatal(err)
						}
					}
					claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: id, MachineID: binding.MachineID, SessionID: fmt.Sprintf("intent-claim-%d", index), TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, WorkflowStates: []string{"In Progress"}, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
					plan, err := service.readNativeClaimPlan(t.Context(), runner, claim, now)
					if err != nil {
						t.Fatalf("read persisted claim plan: %v", err)
					}
					if plan.candidate.State != "in progress" || !plan.latestRunEvent.LandingWaiting || plan.landingPending != seed.pending || plan.changeReady != seed.ready {
						t.Errorf("persisted plan state=%q waiting=%t pending=%t ready=%t item_revision=%d item_generation=%d attempt_revision=%d attempt_generation=%d, want pending=%t ready=%t", plan.candidate.State, plan.latestRunEvent.LandingWaiting, plan.landingPending, plan.changeReady, plan.item.Revision, plan.item.DispatchGeneration, plan.latestAttempt.WorkItemRevision, plan.latestAttempt.DispatchGeneration, seed.pending, seed.ready)
					}
					lease, err := service.claimNativeIssueTenantWithPlan(t.Context(), runner, claim, 5*time.Minute, now, plan, nil)
					if !seed.claimed {
						if !errors.Is(err, ErrNoClaimableWork) {
							t.Fatalf("claim of a waiting landing that is not ready: lease=%q error=%v, want no claimable work", lease.LeaseID, err)
						}
						return
					}
					if err != nil || lease.LeaseID == "" {
						t.Fatalf("claim refused: lease=%q error=%v", lease.LeaseID, err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "intent-claimed")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})

	}
}
