package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			raw, err := json.Marshal(healthEvidence{Reason: string(org), EventIDs: []string{"event"}, Counts: map[string]int{"attempts": 3}, Values: map[string]float64{"rate": 2.5}, Queues: map[string]healthQueueEvidence{string(fixture.project.ID): {QueueDepth: 4}}, Productivity: &healthProductivityEvidence{WindowFrom: service.config.Clock(), WindowTo: service.config.Clock(), LatestFailureStage: "pre_provider"}})
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
				var fallback healthprojection.Page
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
				var page healthprojection.Page
				decodeHubResponse(t, response, &page)
				if len(page.Items) != size || page.Items[0].Evidence.Reason != string(org) || page.Items[0].Evidence.Counts["attempts"] != 3 || page.Items[0].Evidence.Values["rate"] != 2.5 || page.Items[0].Evidence.Queues[string(fixture.project.ID)].QueueDepth != 4 || page.Items[0].Evidence.Productivity == nil {
					t.Fatalf("wrong projected page: %+v", page)
				}
				if statements != 15 || exchanges != 10 {
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
							var page healthprojection.Page
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
	t.Run("maintenance produces and clears scheduled health for both tenants", func(t *testing.T) {
		clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, Clock: clock.Now})
		organizations := []tracker.OrganizationID{"org_first", "org_first_second"}
		fixtures := map[tracker.OrganizationID]nativeFixture{}
		for _, organization := range organizations {
			fixtures[organization] = newNativeFixture(t, service, organization, "Example scheduled health source")
		}
		slices.SortFunc(organizations, func(a, b tracker.OrganizationID) int {
			return strings.Compare(string(fixtures[b].project.ID), string(fixtures[a].project.ID))
		})
		feature := service.HealthPageMaintenanceFeature()
		for _, organization := range organizations {
			fixture := fixtures[organization]
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				project, err := writer.ProjectRecord(t.Context(), fixture.project.ID)
				if err != nil {
					return err
				}
				project.Settings.RepositoryEnabled = true
				project.Settings.Repository = "example-org/example-project"
				project.Settings.CheckoutRepository = project.Settings.Repository
				if _, err := writer.UpdateProjectRecord(t.Context(), project); err != nil {
					return err
				}
				_, err = writer.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), healthstore.ScheduledReport{OrganizationID: organization, ProjectID: fixture.project.ID, RecordedAt: clock.Now().Add(-7 * time.Hour), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1001", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1001/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "success"}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubmaintenance.New(maintenanceTenantRegistry{{Organization: string(organization), Generation: 1}}, []hubmaintenance.Feature{feature})
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			read := func() healthprojection.Page {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/health/findings?state=all", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page healthprojection.Page
				decodeHubResponse(t, response, &page)
				return page
			}
			before := read()
			if len(before.Items) != 1 || before.Items[0].Signal != "dead_man" || before.Items[0].ResolvedAt != nil || before.Items[0].Evidence.ScheduledValidation == nil || before.Items[0].Evidence.ScheduledValidation.LastRunURL != "https://github.com/example-org/example-project/actions/runs/1001/attempts/1" {
				t.Fatalf("live health producer did not publish scoped scheduled evidence: %+v", before)
			}
			clock.Advance(time.Second)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				_, err := writer.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), healthstore.ScheduledReport{OrganizationID: organization, ProjectID: fixture.project.ID, RecordedAt: clock.Now(), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1002", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1002/attempts/1", HeadSHA: strings.Repeat("b", 40), Conclusion: "success"}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := scheduler.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			after := read()
			if len(after.Items) != 1 || after.Items[0].ID != before.Items[0].ID || after.Items[0].ResolvedAt == nil {
				t.Fatalf("live health producer did not resolve recovered subject: %+v", after)
			}
		}
		failures := make(chan error, 2*len(organizations))
		for _, organization := range organizations {
			for range 2 {
				go func() {
					tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
					budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
					_, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget)
					failures <- err
				}()
			}
		}
		for range 2 * len(organizations) {
			if err := <-failures; err != nil {
				t.Error(err)
			}
		}
		for _, organization := range organizations {
			response := performHubAPIRequest(t, service, http.MethodGet, fixtures[organization].base+"/health/findings?state=all", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthprojection.Page
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].ResolvedAt == nil {
				t.Fatalf("concurrent shared feature changed tenant findings: organization=%s page=%+v", organization, page)
			}
		}

	})

	t.Run("completed clipped cycle advances freshness while storage failure withholds it", func(t *testing.T) {
		clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
		details := &memoryRunDetails{}
		service := newPostgresNativeService(t, Config{RunDetails: details, Clock: clock.Now})
		organization := tracker.OrganizationID("org_first")
		clipped := newNativeFixture(t, service, organization, "Example clipped health source")
		healthy := newNativeFixture(t, service, organization, "Example healthy health source")
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			project, err := writer.ProjectRecord(t.Context(), clipped.project.ID)
			if err != nil {
				return err
			}
			project.Core.RequireDependencies = true
			if _, err = writer.UpdateProjectRecord(t.Context(), project); err != nil {
				return err
			}
			for number := int64(1); number <= 201; number++ {
				if _, err := writer.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: clipped.project.ID, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_clipped_%03d", number)), Number: number, Title: "Example completed item", WorkflowStateName: new("Done")}); err != nil {
					return err
				}
			}
			project, err = writer.ProjectRecord(t.Context(), healthy.project.ID)
			if err != nil {
				return err
			}
			project.Settings.RepositoryEnabled = true
			project.Settings.Repository = "example-org/example-project"
			project.Settings.CheckoutRepository = project.Settings.Repository
			if _, err = writer.UpdateProjectRecord(t.Context(), project); err != nil {
				return err
			}
			_, err = writer.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), healthstore.ScheduledReport{OrganizationID: organization, ProjectID: healthy.project.ID, RecordedAt: clock.Now().Add(-7 * time.Hour), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1001", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1001/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "success"}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
		ctx := budget.Context(t.Context(), tenant)
		old := newHealthFinding("dependency_hold", "flow", "project", string(clipped.project.ID), "Example previous observed hold", "Inspect", []string{string(clipped.project.ID)}, healthEvidence{Reason: "previous_observation"})
		if err := service.healthProjection().Publish(ctx, organization, clock.Now(), []healthFinding{old}, healthprojection.Scope{Projects: map[tracker.ProjectID]bool{clipped.project.ID: true}, Fingerprints: map[string]bool{old.Fingerprint: true}}); err != nil {
			t.Fatal(err)
		}
		feature := service.HealthPageMaintenanceFeature()
		read := func(fixture nativeFixture) healthprojection.Page {
			t.Helper()
			response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/health/findings?state=all", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthprojection.Page
			decodeHubResponse(t, response, &page)
			return page
		}
		clock.Advance(time.Second)
		for range 20 {
			if outcome, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget); err != nil || !outcome.Progress || outcome.Items != budget.Items {
				t.Fatalf("health composite step outcome=%+v err=%v: it may make durable source progress and reserves the whole envelope", outcome, err)
			}
		}
		clippedPage, healthyPage := read(clipped), read(healthy)
		if clippedPage.LastTickAt == nil || !clippedPage.LastTickAt.Equal(clock.Now()) || healthyPage.LastTickAt == nil || !healthyPage.LastTickAt.Equal(clock.Now()) {
			t.Fatalf("unequal phase lengths or clipping withheld completed cycle: clipped=%+v healthy=%+v", clippedPage, healthyPage)
		}
		if !slices.ContainsFunc(clippedPage.BaselineUnavailable, func(gap healthBaselineUnavailable) bool { return gap.Signal == "dependency_hold" }) || !slices.ContainsFunc(clippedPage.Items, func(finding healthFinding) bool {
			return finding.Fingerprint == old.Fingerprint && finding.ResolvedAt == nil
		}) {
			t.Fatalf("clipped dependency lost unavailable truth or resolved unexamined finding: %+v", clippedPage)
		}
		if !slices.ContainsFunc(healthyPage.Items, func(finding healthFinding) bool { return finding.Signal == "dead_man" && finding.ResolvedAt == nil }) {
			t.Fatalf("healthy project did not publish beside clipped input: %+v", healthyPage)
		}

		issue := healthy.create(t, "Example running health source")
		var item corestore.WorkItem
		var lease leasestore.Lease
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			var err error
			item, err = writer.WorkItem(t.Context(), healthy.project.ID, issue.WorkItemID)
			if err != nil {
				return err
			}
			if err := writer.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "health_machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			lease, err = writer.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: "health_machine", SessionID: "health_session", LeaseID: "health_lease", TTL: time.Hour})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: "health_run_started", OrganizationID: organization, ProjectID: healthy.project.ID, AggregateID: issue.WorkItemID, AggregateSequence: item.EventSequence + 1, Type: "run.started", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "health_machine"}, RecordedAt: clock.Now(), Data: tracker.CollaborationData{Revision: issue.Revision, Run: &tracker.NativeRunData{AttemptID: "health_attempt", RunID: "health_run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "health_machine", SessionID: lease.SessionID, PolicyID: hubTestPolicy().ID, Sequence: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(details).PrepareEvent(t.Context(), organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		reference := eventstore.Reference(event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes)
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			_, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "health_attempt", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "health_run", Sequence: 1, Status: "running", StartedAt: clock.Now(), PayloadObjectKey: reference.Key, PayloadReference: reference})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		retained := newHealthFinding("retry_storm", "flow", "work_item", string(issue.WorkItemID), "Example previously observed retry condition", "Inspect", []string{string(healthy.project.ID)}, healthEvidence{Reason: "previous_observation"})
		if err := service.healthProjection().Publish(budget.Context(t.Context(), tenant), organization, clock.Now(), []healthFinding{retained}, healthprojection.Scope{Projects: map[tracker.ProjectID]bool{healthy.project.ID: true}, Fingerprints: map[string]bool{retained.Fingerprint: true}}); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"attempt before first event", "missing immutable object"} {
			if kind == "missing immutable object" {
				if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error { _, err := writer.AppendEvent(t.Context(), event); return err }); err != nil {
					t.Fatal(err)
				}
				details.mu.Lock()
				delete(details.data, event.PayloadObjectKey)
				details.keyReads = map[string]int{}
				details.mu.Unlock()
			}
			clock.Advance(time.Second)
			source, err := service.readHealthProjectSource(budget.Context(t.Context(), tenant), organization, healthy.project.ID, "", clock.Now())
			if err != nil {
				t.Fatalf("%s failed an otherwise healthy project: %v", kind, err)
			}
			if source.attemptsKnown || len(source.diagnostics.AttemptRecords) != 1 || (source.diagnostics.AttemptRecords[0].Event.ID == "") != (kind == "attempt before first event") {
				t.Fatalf("%s lost current unknown attempt metadata: %+v", kind, source)
			}
			for range 20 {
				if _, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget); err != nil {
					t.Fatalf("%s failed observation cycle: %v", kind, err)
				}
			}
			healthyPage = read(healthy)
			if healthyPage.LastTickAt == nil || !healthyPage.LastTickAt.Equal(clock.Now()) || !slices.ContainsFunc(healthyPage.BaselineUnavailable, func(gap healthBaselineUnavailable) bool { return gap.Signal == "runner_setup_failures" }) || !slices.ContainsFunc(healthyPage.Items, func(finding healthFinding) bool {
				return finding.Fingerprint == retained.Fingerprint && finding.ResolvedAt == nil
			}) || !slices.ContainsFunc(healthyPage.Items, func(finding healthFinding) bool { return finding.Signal == "dead_man" && finding.ResolvedAt == nil }) {
				t.Fatalf("%s froze tick, discarded healthy output, or lost unavailable truth: %+v", kind, healthyPage)
			}
			if kind == "missing immutable object" {
				details.mu.Lock()
				reads := details.keyReads[event.PayloadObjectKey]
				details.mu.Unlock()
				if reads == 0 {
					t.Fatal("missing-object case never read its actual selected immutable object")
				}
			}
		}
		clippedPage = read(clipped)
		completed := *clippedPage.LastTickAt
		clock.Advance(time.Second)
		details.mu.Lock()
		details.fail = true
		details.mu.Unlock()
		failures := 0
		for range 20 {
			if _, err := feature.Step(budget.Context(t.Context(), tenant), tenant, budget); err != nil {
				failures++
			}
		}
		after := read(clipped)
		if failures == 0 || after.LastTickAt == nil || !after.LastTickAt.Equal(completed) {
			t.Fatalf("storage error falsely advanced completed cycle: failures=%d page=%+v", failures, after)
		}
	})
	t.Run("conversation-only wait retires beyond unresolved prefix", func(t *testing.T) {
		clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
		service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}, Clock: clock.Now})
		organizations := []tracker.OrganizationID{"org_first", "org_first_second"}
		fixtures := map[tracker.OrganizationID]nativeFixture{}
		for _, organization := range organizations {
			fixtures[organization] = newNativeFixture(t, service, organization, "Example conversation health")
			filler := newNativeFixture(t, service, organization, "Example other question project")
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
				if err := writer.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "health_wait_owner", Name: "Example owner", Hash: "health_wait_hash_" + string(organization), Fingerprint: "health_wait_fingerprint_" + string(organization), Scope: "operator", Kind: "service", ProjectAccess: "selected", CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
					return err
				}
				row, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: filler.project.ID, ID: "conv_00000000000000000000000000000001", OwnerPrincipalID: "health_wait_owner", Origin: "user", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, ExecutionContinuation: true, CreatedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now()})
				if err != nil {
					return err
				}
				for number := 1; number <= 201; number++ {
					q := conversation.Question{ID: fmt.Sprintf("q_%032x", number), ConversationID: row.ID, Status: conversation.QuestionUnknown, Prompts: []conversation.Prompt{{ID: "example", Question: "Example question", FreeText: true}}, CreatedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now()}
					if err := writer.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: organization, ConversationID: row.ID, Question: q}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		budget := hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}
		read := func(organization tracker.OrganizationID) healthprojection.Page {
			t.Helper()
			response := performHubAPIRequest(t, service, http.MethodGet, fixtures[organization].base+"/health/findings?state=all", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthprojection.Page
			decodeHubResponse(t, response, &page)
			return page
		}
		for index, kind := range []string{"answered", "settled", "archived", "deleted", "expired"} {
			conversationID := fmt.Sprintf("conv_%032x", 1000+index)
			questionID := fmt.Sprintf("q_%032x", 1000+index)
			for _, organization := range organizations {
				fixture := fixtures[organization]
				if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
					if _, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: fixture.project.ID, ID: conversationID, OwnerPrincipalID: "health_wait_owner", Origin: "user", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, ExecutionContinuation: true, CreatedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now()}); err != nil {
						return err
					}
					return writer.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: organization, ConversationID: conversationID, Question: conversation.Question{ID: questionID, ConversationID: conversationID, Status: conversation.QuestionUnknown, Prompts: []conversation.Prompt{{ID: "example", Question: "Example question", FreeText: true}}, CreatedAt: clock.Now().Add(-time.Hour), UpdatedAt: clock.Now()}})
				}); err != nil {
					t.Fatal(err)
				}
				tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
				ctx := budget.Context(t.Context(), tenant)
				var afterConversation, afterQuestion string
				for range 5 {
					if err := service.healthProjection().MaintainQuestions(ctx, organization, &afterConversation, &afterQuestion, hubmaintenance.ItemLimit); err != nil {
						t.Fatal(err)
					}
				}
				page := read(organization)
				if len(page.Items) != 1 || page.Items[0].Subject.Kind != "project" || page.Items[0].Subject.ID != string(fixture.project.ID) || page.Items[0].ResolvedAt != nil {
					t.Fatalf("unknown conversation-only question did not publish project wait: kind=%s organization=%s page=%+v", kind, organization, page)
				}
			}
			for position, organization := range organizations {
				if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
					row, err := writer.Conversation(t.Context(), fixtures[organization].project.ID, conversationID)
					if err != nil {
						return err
					}
					switch kind {
					case "answered", "expired":
						question, err := writer.Question(t.Context(), conversationID, questionID)
						if err != nil {
							return err
						}
						question.UpdatedAt = clock.Now()
						if kind == "answered" {
							question.Status = conversation.QuestionAnswered
						} else {
							question.ExpiresAt = new(clock.Now().Add(-time.Microsecond))
						}
						return writer.UpsertQuestion(t.Context(), question)
					case "settled":
						row.Status = conversation.StatusSettled
					case "archived":
						row.Archived = true
					case "deleted":
						return writer.(conversationstore.RemovalWriter).DeleteConversation(t.Context(), fixtures[organization].project.ID, conversationID, clock.Now())
					}
					row.UpdatedAt = clock.Now()
					_, err = writer.UpdateConversation(t.Context(), row, row.Revision)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				tenant := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
				ctx := budget.Context(t.Context(), tenant)
				var after string
				var proof healthProjectWaitCursor
				for range 12 {
					if err := service.maintainRetiredHealthSubjects(ctx, organization, &after, &proof, hubmaintenance.ItemLimit); err != nil {
						t.Fatal(err)
					}
				}
				page := read(organization)
				if len(page.Items) != 1 || page.Items[0].ResolvedAt == nil {
					t.Fatalf("conversation-only wait remained after current question retired: kind=%s organization=%s page=%+v", kind, organization, page)
				}
				if position == 0 {
					untouched := read(organizations[1])
					if len(untouched.Items) != 1 || untouched.Items[0].ResolvedAt != nil {
						t.Fatalf("one tenant retired another tenant wait: kind=%s page=%+v", kind, untouched)
					}
				}
			}
		}
	})

}

type maintenanceTenantRegistry []hubmaintenance.Tenant

func (r maintenanceTenantRegistry) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	var page []hubmaintenance.Tenant
	for _, tenant := range r {
		if tenant.Organization > after && len(page) < limit {
			page = append(page, tenant)
		}
	}
	return page, nil
}
