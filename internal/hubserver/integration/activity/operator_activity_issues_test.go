package activity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestActivityIssueRead(t *testing.T) {
	t.Parallel()
	lanes := []tracker.NativeState{{Name: "Ideas"}, {Name: "Ready", Dispatchable: true}, {Name: "Build", Dispatchable: true}, {Name: "Waiting"}, {Name: "Shipped", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	type entry struct {
		lane   string
		second int
	}
	for _, tt := range []struct {
		name                       string
		entries                    []entry
		live                       bool
		attempts                   int
		finished                   bool
		worked                     float64
		runner                     string
		reporter, expired, missing bool
		reset, missingRun          bool
	}{
		{name: "two overlapping attempts yield one running issue", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 2, worked: 80},
		{name: "live terminal candidates are deduplicated ordered and capped", entries: []entry{{"Ready", 0}, {"Shipped", 90}}, live: true, finished: true, attempts: 1, worked: 70},
		{name: "parked holding issue without live attempt omitted", entries: []entry{{"Ready", 0}, {"Waiting", 20}}, attempts: 1},
		{name: "active issue without live attempt omitted", entries: []entry{{"Ready", 0}}, attempts: 1},
		{name: "custom terminal in window with no attempts included", entries: []entry{{"Ready", 0}, {"Shipped", 90}}, finished: true},
		{name: "cancelled issue included with worked time excluding parked time", entries: []entry{{"Ready", 0}, {"Waiting", 20}, {"Cancelled", 90}}, attempts: 1, finished: true, worked: 30},
		{name: "terminal before window omitted", entries: []entry{{"Ready", 0}, {"Shipped", 10}}},
		{name: "runner filter matches a previous runner", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 2, worked: 80, runner: "runner_previous"},
		{name: "runner filter excludes other issues", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, runner: "runner_other"},
		{name: "reporter sees only their issue and baseline", entries: []entry{{"Ready", 0}, {"Shipped", 90}}, finished: true, attempts: 1, worked: 30, reporter: true},
		{name: "expired attempt is not live", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, expired: true},
		{name: "live issue remains visible without rollups", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, missing: true},
		{name: "reset excludes earlier attempt phases", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Ideas", 20}, {"Ready", 60}, {"Build", 70}}, live: true, attempts: 2, worked: 40, reset: true},
		{name: "holding after active does not reset", entries: []entry{{"Ready", 0}, {"Waiting", 20}, {"Build", 60}}, live: true, attempts: 2, worked: 80},
		{name: "live issue without immutable run envelope has unknown worked timing", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, missingRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newActivityPostgresFixture(t, "org_browser_preview")
			now := f.now
			base := now.Add(-100 * time.Second)
			project := f.createProject(t, "Activity example "+tt.name)
			scope := nativeScope{organization: f.organization, project: tracker.ProjectID(project)}
			updateWorkflowPolicyFixtureProject(t, nativeFixture{service: f.service, project: tracker.NativeProject{OrganizationID: scope.organization, ID: scope.project}}, func(project *projectstore.Record) { project.States = lanes })
			identity := f.provider.identity("reporter_0", "reporter@example.test", f.service.config.Hosted.WorkOSOrganizationID, "")
			membership, err := f.provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, "reporter")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.addHostedMember(t.Context(), identity, membership); err != nil {
				t.Fatal(err)
			}
			token, _ := browserHostedSession(t, f.service, identity)
			f.cookies["reporter"] = &http.Cookie{Name: hostedCookie, Value: token}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events}
			if err := f.repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: scope.organization, UserID: identity.Subject, ProjectID: scope.project}); err != nil {
					return err
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: "hosted_" + apikey.HashToken(identity.Subject)[:32], ProjectID: scope.project}); err != nil {
					return err
				}
				for _, runner := range []string{"runner_previous", "runner_current"} {
					machine := "machine_" + runner
					if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: scope.organization, ID: machine, Hostname: "example", Capacity: 64, Version: "example"}); err != nil {
						return err
					}
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: scope.organization, ID: runner, Name: runner, Hash: apikey.HashToken(runner), Scope: "worker", Kind: "service", NativeOnly: true, ProjectAccess: "all", CreatedAt: base, UpdatedAt: base}); err != nil {
						return err
					}
					if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: scope.organization, ID: runner, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenHash: apikey.HashToken("enroll_" + runner), Scope: "organization", Operations: operations, CreatedAt: base, ExpiresAt: now.Add(time.Hour), CreatedBy: "user_browser_owner"}); err != nil {
						return err
					}
					if err := w.RedeemRunnerEnrollment(t.Context(), runner, runner, tracker.MachineID(machine), base); err != nil {
						return err
					}
					if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: scope.organization, RunnerID: runner, MachineID: tracker.MachineID(machine), TokenID: runner, EnrollmentID: runner, Scope: "organization", State: "active", Operations: operations, CapacityLimit: 64, ReportedCapacity: 64, CreatedAt: base, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			content := eventstore.NewContent(f.service.runDetailStore())
			appendEvent := func(value tracker.CollaborationEvent) {
				t.Helper()
				value.OrganizationID, value.SchemaVersion = scope.organization, 1
				if value.ProjectID == "" {
					value.ProjectID = scope.project
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				record, err := content.PrepareEvent(t.Context(), scope.organization, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.AppendEvent(t.Context(), record)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			ids := []string{}
			for n := range 2 {
				id := tracker.NativeWorkItemID(fmt.Sprintf("activity_issue_%d", n))
				actor := tracker.Actor{Kind: "human", PrincipalID: fmt.Sprintf("reporter_%d", n)}
				if n == 0 {
					actor.PrincipalID = "hosted_" + apikey.HashToken(identity.Subject)[:32]
				}
				state := tt.entries[len(tt.entries)-1].lane
				updated := now
				if tt.name == "terminal before window omitted" {
					updated = base.Add(10 * time.Second)
				}
				var issue corestore.WorkItem
				if err := f.repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					issue, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.organization, ProjectID: scope.project, NativeID: id, Number: int64(n + 1), Title: fmt.Sprintf("Example issue %d", n), WorkflowStateName: &state, ActorKind: &actor.Kind, ActorPrincipalID: &actor.PrincipalID, NativeCreatedAt: base, NativeUpdatedAt: updated})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if tt.name == "terminal before window omitted" {
					if result, err := f.admin.ExecContext(t.Context(), "UPDATE issues SET terminal_entered_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4", updated, scope.organization, scope.project, id); err != nil {
						t.Fatal(err)
					} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
						t.Fatalf("old terminal fixture affected=%d error=%v", affected, err)
					}
					var terminal, storedUpdate time.Time
					if err := f.admin.QueryRowContext(t.Context(), "SELECT terminal_entered_at,native_updated_at FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", scope.organization, scope.project, id).Scan(&terminal, &storedUpdate); err != nil {
						t.Fatal(err)
					}
					if !terminal.Equal(updated) || !storedUpdate.Equal(updated) || !terminal.Before(base.Add(50*time.Second)) {
						t.Fatalf("terminal source window setup terminal=%s updated=%s", terminal, storedUpdate)
					}
				}
				ids = append(ids, string(id))
				for i, e := range tt.entries {
					if tt.missing {
						continue
					}
					value := tracker.CollaborationEvent{ID: fmt.Sprintf("%s_transition_%d", id, i), AggregateID: id, Type: "issue.created", Actor: actor, RecordedAt: base.Add(time.Duration(e.second) * time.Second)}
					if i > 0 {
						value.Type = "workflow.transitioned"
						value.Data.FromState, value.Data.ToState = tt.entries[i-1].lane, e.lane
					}
					appendEvent(value)
				}
				for i := range tt.attempts {
					attemptID := fmt.Sprintf("activity_%s_%d_attempt", id, i)
					runner := "runner_previous"
					if i > 0 {
						runner = "runner_current"
					}
					status := "succeeded"
					end := base.Add(50 * time.Second)
					f.fence++
					lease := leasestore.Lease{OrganizationID: scope.organization, LeaseID: "lease_" + attemptID, IssueID: issue.ID, MachineID: "machine_" + runner, SessionID: "session_" + attemptID, FencingToken: f.fence, WorkItemRevision: issue.Revision, AcquiredAt: base, RenewedAt: base, ExpiresAt: now.Add(time.Hour), ReleasedAt: &end}
					if tt.live && i == tt.attempts-1 {
						status, lease.ReleasedAt = "running", nil
					}
					if tt.expired {
						lease.ExpiresAt = now.Add(-time.Second)
					}
					if err := f.repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: f.receipt(t, "core-lease", lease.LeaseID, lease)}); err != nil {
						t.Fatal(err)
					}
					assignment := runnerstore.LeaseAssignment{OrganizationID: scope.organization, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: base}
					if err := f.repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: f.receipt(t, "core-runner-lease-assignment", lease.LeaseID, assignment)}); err != nil {
						t.Fatal(err)
					}
					phase := tracker.NativePhase{StartedAt: base.Add(time.Duration(20+i*10) * time.Second), FinishedAt: end}
					if status == "running" {
						phase.FinishedAt = time.Time{}
					}
					run := tracker.NativeRunData{AttemptID: attemptID, RunID: attemptID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, Sequence: 1, RunnerID: runner, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "security_audit"}, Phases: []tracker.NativePhase{phase}}}
					kind := "run.observed"
					if status != "running" {
						run.Outcome, kind = status, "run.finished"
					}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(scope.organization), raw)
					if err != nil {
						t.Fatal(err)
					}
					attempt := leasestore.Attempt{OrganizationID: scope.organization, ID: attemptID, IssueID: issue.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: attemptID, Sequence: 1, Status: status, WorkItemRevision: issue.Revision, PayloadObjectKey: ref.Key, StartedAt: base, UpdatedAt: end}
					if err := f.repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: f.receipt(t, "core-attempt", attemptID, attempt)}); err != nil {
						t.Fatal(err)
					}
					if !tt.missingRun {
						appendEvent(tracker.CollaborationEvent{ID: "event_" + attemptID, AggregateID: id, Type: kind, Actor: tracker.Actor{Kind: "machine", PrincipalID: lease.MachineID}, RecordedAt: end, Data: tracker.CollaborationData{Run: &run}})
					}
				}
			}
			connection := startupConnection(t, f.admin, "detent_runtime_v2", "")
			config, err := connection.PoolConfig()
			if err != nil {
				t.Fatal(err)
			}
			pgtransport.Configure(config)
			config.MaxConns, config.ConnConfig.Tracer = 3, runtimeHistoryTrace{}
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			if err != nil {
				pool.Close()
				t.Fatal(err)
			}
			originalTenant, originalAccess, originalPostgres := f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres
			t.Cleanup(func() {
				f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres = originalTenant, originalAccess, originalPostgres
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.database.postgres = reader, reader, reader
			if tt.live && tt.finished {
				slices.Sort(ids)
				for _, newer := range []bool{false, true} {
					stamp := now
					if newer {
						stamp = stamp.Add(time.Second)
					}
					if result, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET native_updated_at=$1 WHERE organization_id=$2 AND project_id=$3 AND native_id=$4`, stamp, scope.organization, scope.project, ids[1]); err != nil {
						t.Fatal(err)
					} else if affected, err := result.RowsAffected(); err != nil || affected != 1 {
						t.Fatalf("ranking timestamp fixture affected=%d error=%v", affected, err)
					}
					for _, limit := range []int{1, 2, 3} {
						var got activitystore.Snapshot
						if err := reader.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							got, err = r.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{scope.project}, From: base, To: now, Now: now, Limit: limit})
							return err
						}); err != nil {
							t.Fatal(err)
						}
						want := slices.Clone(ids)
						if newer {
							slices.Reverse(want)
						}
						want = want[:min(len(want), limit)]
						var actual []string
						for _, candidate := range got.Issues {
							actual = append(actual, string(candidate.WorkItemID))
						}
						if !slices.Equal(actual, want) {
							t.Fatalf("newer=%t limit=%d candidates=%v want=%v", newer, limit, actual, want)
						}
						query := url.Values{"project_id": {project}, "limit": {fmt.Sprint(limit)}, "from": {base.Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}}
						response := f.page(t, "owner", "/api/v2/organizations/"+string(scope.organization)+"/activity?"+query.Encode())
						requireNativeStatus(t, response, http.StatusOK)
						var page operatortool.ActivityReport
						decodeHubResponse(t, response, &page)
						actual = nil
						for _, issue := range page.RunningIssues {
							actual = append(actual, issue.WorkItemID)
						}
						liveWant := slices.Clone(ids)
						if newer {
							slices.Reverse(liveWant)
						}
						if !slices.Equal(actual, liveWant) || len(page.FinishedIssues) != 0 {
							t.Fatalf("newer=%t limit=%d public candidates=%v want=%v finished=%v", newer, limit, actual, liveWant, page.FinishedIssues)
						}
					}
				}
			}

			if tt.live && tt.finished {
				t.Run("HTTP limit caps finished work while older live work stays visible", func(t *testing.T) {
					otherProject := tracker.ProjectID("project_activity_terminal")
					otherID := tracker.NativeWorkItemID("activity_recent_terminal")
					state, kind := "Shipped", "human"
					var actor string
					if err := f.repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: otherProject, Name: "Recent terminal project", Profile: "native"}, States: lanes}); err != nil {
							return err
						}
						member, err := w.AccessMember(t.Context(), "user_browser_owner")
						if err != nil {
							return err
						}
						actor = member.PrincipalID
						if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: scope.organization, UserID: member.UserID, ProjectID: otherProject, CanWrite: true}); err != nil {
							return err
						}
						if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: actor, ProjectID: otherProject}); err != nil {
							return err
						}
						_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.organization, ProjectID: otherProject, NativeID: otherID, Number: 1, Title: "Recent terminal item", WorkflowStateName: &state, ActorKind: &kind, ActorPrincipalID: &actor, NativeCreatedAt: base, NativeUpdatedAt: now.Add(2 * time.Second)})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					appendEvent(tracker.CollaborationEvent{ID: "recent_terminal_created", ProjectID: otherProject, AggregateID: otherID, Type: "issue.created", Actor: tracker.Actor{Kind: kind, PrincipalID: actor}, RecordedAt: base})
					appendEvent(tracker.CollaborationEvent{ID: "recent_terminal_finished", ProjectID: otherProject, AggregateID: otherID, Type: "workflow.transitioned", Actor: tracker.Actor{Kind: kind, PrincipalID: actor}, RecordedAt: base.Add(90 * time.Second), Data: tracker.CollaborationData{FromState: "Ready", ToState: state}})
					var selected activitystore.Snapshot
					if err := reader.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						selected, err = r.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{scope.project, otherProject}, From: base, To: now, Now: now, Limit: 1})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if len(selected.Issues) != 1 || selected.Issues[0].WorkItemID != otherID || !selected.IssuesPartial {
						t.Fatalf("bounded owner cohort lost newest-first selection: %+v", selected)
					}
					query := url.Values{"limit": {"1"}, "from": {base.Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}}
					response := f.page(t, "owner", "/api/v2/organizations/"+string(scope.organization)+"/activity?"+query.Encode())
					requireNativeStatus(t, response, http.StatusOK)
					var page operatortool.ActivityReport
					decodeHubResponse(t, response, &page)
					var live []string
					for _, issue := range page.RunningIssues {
						live = append(live, issue.WorkItemID)
					}
					want := slices.Clone(ids)
					slices.Reverse(want)
					if !slices.Equal(live, want) || len(page.FinishedIssues) != 1 || page.FinishedIssues[0].WorkItemID != string(otherID) {
						t.Fatalf("HTTP request limit hid live work: live=%v want=%v finished=%v", live, want, page.FinishedIssues)
					}
				})
			}

			query := url.Values{"project_id": {project}, "limit": {"100"}, "from": {base.Add(50 * time.Second).Format(time.RFC3339Nano)}, "to": {now.Format(time.RFC3339Nano)}, "runner_id": {tt.runner}}
			account := "owner"
			if tt.reporter {
				account = "reporter"
			}
			capture := &runtimeHistoryTraceCapture{}
			request := httptest.NewRequestWithContext(context.WithValue(t.Context(), runtimeHistoryTraceKey{}, capture), http.MethodGet, "/api/v2/organizations/"+string(scope.organization)+"/activity?"+query.Encode(), nil)
			request.AddCookie(f.cookies[account])
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusOK)
			var out operatortool.ActivityReport
			decodeHubResponse(t, response, &out)
			assertActivityIssueOwnerStatements(t, pool, scope.organization, capture, tt.runner != "runner_other" && tt.name != "terminal before window omitted", !tt.missing)

			rows := out.RunningIssues
			want := 0
			if tt.live && !tt.expired && tt.runner != "runner_other" {
				want = 2
			}
			if tt.reporter {
				want = 1
			}
			if tt.finished {
				if !tt.live {
					rows = out.FinishedIssues
				}
				want = 2
				if tt.reporter {
					want = 1
				}
				if out.IssueTypicalDurations[0].Total.Count != want || out.IssueTypicalDurations[0].Total.P50Seconds != 90 {
					t.Fatalf("baseline=%+v", out.IssueTypicalDurations)
				}
			}
			if len(rows) != want || !tt.finished && len(out.FinishedIssues) != 0 || !tt.live && len(out.RunningIssues) != 0 {
				t.Fatalf("running=%+v finished=%+v", out.RunningIssues, out.FinishedIssues)
			}
			for _, row := range rows {
				if tt.missingRun && (!row.Partial || !out.IssuesPartial) {
					t.Fatalf("missing run envelope presented complete worked timing: row=%+v report_partial=%t", row, out.IssuesPartial)
				}
				if tt.missing && (!row.ClockStartedAt.IsZero() || !row.Partial) {
					t.Fatalf("missing timing=%+v", row)
				}
				if !reflect.DeepEqual([]string{row.Lanes[0].Name, row.Lanes[1].Name}, []string{"Ready", "Build"}) || row.WorkedSeconds != tt.worked {
					t.Fatalf("row=%+v", row)
				}
				if tt.reset && (row.TotalSeconds != 40 || row.ClockStartedAt.Sub(base).Seconds() != 60) {
					t.Fatalf("reset=%+v", row)
				}
				if tt.attempts == 2 && !tt.reset && (len(row.RunnerIDs) != 2 || len(row.WorkedByRole) != 1 || row.WorkedByRole[0].Attempts != 2 || row.RunnerID != "runner_current") {
					t.Fatalf("work=%+v", row)
				}
			}
			if tt.name == "custom terminal in window with no attempts included" {
				t.Run("as-of sequence order and history sentinel", func(t *testing.T) {
					item := ids[0]
					var issueID int64
					if err := f.admin.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", scope.organization, scope.project, item).Scan(&issueID); err != nil {
						t.Fatal(err)
					}
					_, err := f.admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT organization_id,project_id,work_item_id,id||'_history_'||n,1000+n,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,
 CASE WHEN n<=48 THEN $4::timestamptz-n*interval '1 second' ELSE $4::timestamptz+interval '1 hour' END,
 payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events CROSS JOIN generate_series(1,2000) AS history(n)
 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='workflow.transitioned'`, scope.organization, scope.project, item, now)
					if err != nil {
						t.Fatal(err)
					}
					_, err = f.admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT organization_id,project_id,work_item_id,id||'_comment_'||n,100000+n,'comment.created',schema_version,actor_kind,actor_principal_id,from_state,to_state,$4::timestamptz-interval '2 hours'-n*interval '1 second',
 payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events CROSS JOIN generate_series(1,5000) AS volume(n)
 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='workflow.transitioned' AND sequence<1000`, scope.organization, scope.project, item, now)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.admin.ExecContext(t.Context(), "ANALYZE collaboration_events"); err != nil {
						t.Fatal(err)
					}
					for _, test := range []struct {
						cutoff      time.Time
						newest      int64
						transitions int
						clipped     bool
					}{
						{now, 1048, 49, false},
						{now.Add(time.Hour), 3000, 50, true},
					} {
						capture := &runtimeHistoryTraceCapture{}
						ctx := context.WithValue(t.Context(), runtimeHistoryTraceKey{}, capture)
						var snapshot activitystore.Snapshot
						if err := reader.ReadTenantSnapshot(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							snapshot, err = r.(activitystore.Reader).ActivitySnapshot(ctx, activitystore.Filter{Projects: []tracker.ProjectID{scope.project}, From: base, To: now, Now: test.cutoff, Limit: activitystore.IssuePopulationLimit})
							return err
						}); err != nil {
							t.Fatal(err)
						}
						var sequences []int64
						for _, event := range snapshot.Transitions {
							if event.IssueID == issueID {
								sequences = append(sequences, event.Sequence)
							}
						}
						if len(sequences) != test.transitions || sequences[0] != test.newest || sequences[1] != test.newest-1 || snapshot.Created[issueID] == test.clipped || snapshot.IssuesPartial != test.clipped {
							t.Fatalf("cutoff=%v sequences=%v created=%v partial=%v", test.cutoff, sequences, snapshot.Created[issueID], snapshot.IssuesPartial)
						}
						assertActivityIssueOwnerStatements(t, pool, scope.organization, capture, true, true)
					}
				})
			}
		})
	}
}

func assertActivityIssueOwnerStatements(t *testing.T, pool *pgxpool.Pool, organization tracker.OrganizationID, capture *runtimeHistoryTraceCapture, historyExpected, complete bool) {
	t.Helper()
	capture.mu.Lock()
	statements := slices.Clone(capture.statements)
	capture.mu.Unlock()
	if len(statements) == 0 {
		t.Fatal("activity PostgreSQL owner statements were not observed")
	}
	found := map[string]bool{}
	for _, statement := range statements {
		if strings.HasPrefix(statement.query, "SELECT id,project_id,native_id,coalesce(workflow_state_name,''),native_created_at,native_updated_at FROM issues") {
			found["selected"] = true
			ids, ok := statement.args[1].([]int64)
			if !ok || len(ids) == 0 || len(ids) > 3*activitystore.IssuePopulationLimit || statement.args[2] != activitystore.IssuePopulationLimit {
				t.Fatalf("HTTP request limit replaced activity population budget: %v", statement.args)
			}
			continue
		}
		kind := ""
		bound := float64(0)
		relation := ""
		if strings.HasPrefix(statement.query, "SELECT i.id,e.sequence,e.type,e.from_state,e.to_state,e.recorded_at") {
			kind, relation = "history", "collaboration_events"
			ids, ok := statement.args[1].([]int64)
			if !ok || len(ids) == 0 || statement.args[3] != activitystore.HistoryLimit+1 {
				t.Fatalf("activity history lost its selected-item bound: %v", statement.args)
			}
			bound = float64(len(ids) * (activitystore.HistoryLimit + 1))
		}
		if strings.HasPrefix(statement.query, "SELECT id FROM issues WHERE organization_id=$1 AND native_updated_at>=$2") {
			kind, relation = "candidates", "issues"
			if statement.args[2] != activitystore.PopulationLimit+1 {
				t.Fatalf("activity candidates lost their population bound: %v", statement.args)
			}
			bound = float64(activitystore.PopulationLimit + 1)
		}
		if strings.HasPrefix(statement.query, "SELECT i.id,i.project_id,i.native_id,i.number,i.title,p.name") {
			kind, relation = "terminal", "issues"
			ids, ok := statement.args[2].([]int64)
			if !ok || len(ids) > activitystore.PopulationLimit || statement.args[3] != activitystore.PopulationLimit {
				t.Fatalf("activity terminal issues lost their candidate bound: %v", statement.args)
			}
			bound = float64(activitystore.PopulationLimit)
		}
		if kind == "" {
			continue
		}
		found[kind] = true
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		_, err = tx.Exec(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(organization))
		if err == nil {
			err = tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), hubReadTimeout)
		rollbackErr := tx.Rollback(cleanup)
		cancel()
		if err := errors.Join(err, rollbackErr); err != nil {
			t.Fatal(err)
		}
		type planNode struct {
			Relation  string     `json:"Relation Name"`
			Rows      float64    `json:"Actual Rows"`
			Removed   float64    `json:"Rows Removed by Filter"`
			Rechecked float64    `json:"Rows Removed by Index Recheck"`
			Loops     float64    `json:"Actual Loops"`
			Plans     []planNode `json:"Plans"`
		}
		var plans []struct{ Plan planNode }
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("invalid activity PostgreSQL plan: %s error=%v", raw, err)
		}
		work := float64(0)
		var inspect func(planNode)
		inspect = func(node planNode) {
			if node.Relation == relation {
				work += (node.Rows + node.Removed + node.Rechecked) * node.Loops
			}
			for _, child := range node.Plans {
				inspect(child)
			}
		}
		inspect(plans[0].Plan)
		if work > bound || kind == "history" && complete && work == 0 {
			t.Fatalf("activity %s owner work=%g bound=%g plan=%s", kind, work, bound, raw)
		}
	}
	if found["history"] != historyExpected || found["selected"] != historyExpected || !found["candidates"] || !found["terminal"] {
		t.Fatalf("activity source owners were not observed: %v", found)
	}
}
